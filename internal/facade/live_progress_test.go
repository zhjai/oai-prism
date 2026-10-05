package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/metrics"
	"github.com/oai-prism/oaiprism/internal/prism"
)

func TestProgressDedupWindow(t *testing.T) {
	d := newProgressDeduper()
	event := prism.LiveProgressEvent{Type: "agent_reasoning", LineIndex: 1, Text: "one"}
	if len(d.fresh([]prism.LiveProgressEvent{event, event})) != 1 {
		t.Fatal("duplicate in snapshot emitted")
	}
	event.Text = "one two"
	if got := d.fresh([]prism.LiveProgressEvent{event}); len(got) != 1 || got[0].Text != " two" {
		t.Fatalf("cumulative extension: %+v", got)
	}
	for i := 2; i < progressDedupWindow*3; i++ {
		event.LineIndex = i
		if len(d.fresh([]prism.LiveProgressEvent{event})) != 1 {
			t.Fatal("new entry lost")
		}
	}
	if len(d.seen) > progressDedupWindow {
		t.Fatalf("unbounded dedup: %d", len(d.seen))
	}
	for i := 1; i < progressDedupWindow*3; i++ {
		event.LineIndex = i
		if len(d.fresh([]prism.LiveProgressEvent{event})) != 0 {
			t.Fatal("old snapshot entry replayed")
		}
	}
	c := prism.New(nil, prism.UpstreamOptions{}, prism.SchemaOptions{})
	for i, payload := range []string{`{"text":null}`, `{"text":"corrected"}`} {
		st, err := c.ParseStatusPayload([]byte(`{"request_id":"r","codex_live_progress":{"eventPreviews":[{"line_index":1000,"payload_type":"agent_reasoning","payload":`+payload+`}]}}`), "r", "")
		if err != nil || len(d.fresh(st.Progress)) != i {
			t.Fatalf("malformed entry suppressed corrected one: %+v %v", st, err)
		}
	}
	if len(newProgressDeduper().fresh([]prism.LiveProgressEvent{event})) != 1 {
		t.Fatal("dedup leaked across requests")
	}
}

type progressFixture struct {
	runner    *Runner
	handler   *Handler
	request   *RunRequest
	starts    atomic.Int32
	polls     atomic.Int32
	stops     atomic.Int32
	pollError atomic.Int32
	startBody string
}

func newProgressFixture(t *testing.T, statuses []string, interval time.Duration) *progressFixture {
	t.Helper()
	f := &progressFixture{startBody: `{"status":"started","request_id":"req","turn_state":{"seq":0}}`}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case prism.PathResponseStart:
			f.starts.Add(1)
			io.WriteString(w, f.startBody)
		case prism.PathResponseStatus:
			i := int(f.polls.Add(1)) - 1
			if code := int(f.pollError.Load()); code != 0 {
				http.Error(w, "synthetic upstream failure", code)
				return
			}
			if i >= len(statuses) {
				i = len(statuses) - 1
			}
			io.WriteString(w, statuses[i])
		case prism.PathResponseStop:
			f.stops.Add(1)
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected synthetic upstream path: %s", r.URL.Path)
			http.Error(w, "unexpected path", 500)
		}
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.URL
	cfg.Creds.Accounts = []config.AccountConfig{{ID: "test", AccessToken: "synthetic-test-only"}}
	cfg.Facade.PollInterval = interval
	cfg.Facade.PollBackoffMax = interval
	cfg.Facade.MaxPollTimeout = 10 * time.Second
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := account.NewPool(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	client := prism.New(pool.Get("test").Client, prism.UpstreamOptions{}, prism.SchemaOptions{StartPath: prism.PathResponseStart, StatusPath: prism.PathResponseStatus})
	app := metrics.NewApp()
	f.runner = NewRunner(cfg, log, pool, client, app)
	t.Cleanup(f.runner.Close)
	f.runner.sandboxes.Put("test", &prism.Sandbox{URL: up.URL, Token: "synthetic"})
	f.runner.sandboxes.MarkSynced("test", "project", time.Now().Add(time.Hour))
	f.handler = NewHandler(cfg, log, f.runner, app)
	f.request = &RunRequest{Input: []prism.InputItem{prism.NewUserItem("answer")}, Model: "test", API: "responses", ProjectID: "project"}
	return f
}

const progressPending = `{"request_id":"req","status":"pending","turn_state":{"seq":1},"codex_live_progress":{"eventPreviews":[{"line_index":1,"payload_type":"agent_reasoning","raw":{"payload":{"text":"thinking"}}},{"line_index":2,"payload_type":"agent_message","raw":{"payload":{"message":"working"}}}]}}`
const progressFinal = `{"request_id":"req","status":"completed","response":{"status":"success","payload":{"id":"resp","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"21"}]}]}}}`

func TestRunnerDoesNotRetryAfterProgressOnlyOutput(t *testing.T) {
	for _, kind := range []string{"progress", "reasoning"} {
		t.Run(kind, func(t *testing.T) {
			pending := progressPending
			if kind == "reasoning" {
				pending = `{"request_id":"req","status":"pending","response":{"status":"success","payload":{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]}]}}}`
			}
			f := newProgressFixture(t, []string{pending}, time.Millisecond)
			// A 500 remains eligible for account retries and does not cool the
			// only account, so a missing stream guard would actually restart.
			calls := 0
			_, err := f.runner.Run(context.Background(), f.request, func(d Delta) error {
				calls++
				if d.Text != "" {
					t.Fatal("fixture unexpectedly emitted final text")
				}
				f.pollError.Store(http.StatusInternalServerError)
				return nil
			})
			if err == nil || calls != 1 || f.starts.Load() != 1 {
				t.Fatalf("progress replayed: starts=%d emits=%d err=%v", f.starts.Load(), calls, err)
			}
		})
	}
}

func TestRunnerProgressPreventsNativeConversationReplay(t *testing.T) {
	failure := `{"request_id":"req","status":"completed","response":{"status":"error","payload":{"reason":"conversation_too_large","message":"conversation_too_large"}}}`
	f := newProgressFixture(t, []string{progressPending, failure}, time.Millisecond)
	key := "native-progress-" + t.Name()
	b := nativeBindingFor(key)
	b.mu.Lock()
	b.cid, b.account, b.project = "old-conversation", "test", "project"
	b.mu.Unlock()
	t.Cleanup(func() {
		nativeBindings.mu.Lock()
		delete(nativeBindings.m, key)
		nativeBindings.mu.Unlock()
	})
	f.request.Native = &nativeTurn{key: key, strong: true, conv: nativeConv("", "answer")}
	calls := 0
	_, err := f.runner.Run(context.Background(), f.request, func(d Delta) error {
		calls++
		return nil
	})
	if err == nil || !isConversationGone(err) || calls != 1 || f.starts.Load() != 1 {
		t.Fatalf("native conversation replayed after progress: starts=%d emits=%d err=%v", f.starts.Load(), calls, err)
	}
}

func TestRunnerKeepsRetriesBeforeAnyOutput(t *testing.T) {
	f := newProgressFixture(t, []string{progressPending}, time.Millisecond)
	f.pollError.Store(http.StatusInternalServerError)
	_, err := f.runner.Run(context.Background(), f.request, func(d Delta) error {
		t.Fatal("unexpected output before fixture failure")
		return nil
	})
	if err == nil || f.starts.Load() != int32(f.runner.accountRetries) {
		t.Fatalf("pre-output account retries disabled: starts=%d err=%v", f.starts.Load(), err)
	}
}

func TestRunnerReasoningSourcesDoNotDuplicateOrConcatenateRewrites(t *testing.T) {
	payload := `{"status":"success","payload":{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]}]}}`
	legacy := `{"request_id":"req","status":"pending","response":` + payload + `}`
	both := strings.TrimSuffix(progressPending, "}") + `,"response":` + payload + `}`
	for name, statuses := range map[string][]string{
		"same snapshot":     {both, progressFinal},
		"live then payload": {progressPending, legacy, progressFinal},
		"payload then live": {legacy, progressPending, progressFinal},
		"rewritten payload": {legacy, strings.Replace(legacy, "thinking", "rewritten", 1), progressFinal},
	} {
		t.Run(name, func(t *testing.T) {
			f := newProgressFixture(t, statuses, time.Millisecond)
			var text strings.Builder
			_, err := f.runner.Run(context.Background(), f.request, func(d Delta) error {
				text.WriteString(d.Reasoning)
				for _, ev := range d.Progress {
					if ev.Type == "agent_reasoning" {
						text.WriteString(ev.Text)
					}
				}
				return nil
			})
			if err != nil || text.String() != "thinking" {
				t.Fatalf("reasoning concatenated: %q %v", text.String(), err)
			}
		})
	}
}

func TestRunnerLiveProgressOrderAndPacing(t *testing.T) {
	interval := 25 * time.Millisecond
	f := newProgressFixture(t, []string{progressPending, progressPending, progressFinal}, interval)
	var events []Delta
	started := time.Now()
	res, err := f.runner.Run(context.Background(), f.request, func(d Delta) error {
		if len(d.Progress) != 0 && f.polls.Load() != 1 {
			t.Error("progress did not precede terminal poll")
		}
		events = append(events, d)
		return nil
	})
	if err != nil || res.Text != "21" || res.Reasoning != "" || len(events) != 2 || events[0].Text != "" || len(events[0].Progress) != 2 || events[1].Text != "21" {
		t.Fatalf("result=%+v events=%+v err=%v", res, events, err)
	}
	if time.Since(started) < 2*interval {
		t.Fatal("nonterminal progress bypassed poll pacing")
	}
	if f.starts.Load() != 1 || f.stops.Load() != 0 {
		t.Fatal("unexpected start replay or stop")
	}
	var out bytes.Buffer
	if err := f.runner.app.Reg.Render(&out); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"oaiprism_facade_first_progress_seconds_count", "oaiprism_facade_first_output_seconds_count"} {
		if !strings.Contains(out.String(), name+`{api="responses"} 1`) {
			t.Fatalf("missing single timing: %s", name)
		}
	}
}

func TestRunnerTerminalSkipsPollInterval(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			body := progressFinal
			if failed {
				body = `{"request_id":"req","status":"completed","response":{"status":"error","payload":{"message":"synthetic failure"}}}`
			}
			f := newProgressFixture(t, []string{body}, 2*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
			defer cancel()
			start := time.Now()
			res, err := f.runner.Run(ctx, f.request, func(Delta) error { return nil })
			if (err != nil) != failed || errors.Is(err, context.DeadlineExceeded) || time.Since(start) >= 750*time.Millisecond || f.stops.Load() != 0 {
				t.Fatalf("terminal waited/stopped: result=%+v err=%v stops=%d", res, err, f.stops.Load())
			}
		})
	}
}

func TestRunnerReasoningOnlyAndCancellation(t *testing.T) {
	reasoning := `{"request_id":"req","status":"pending","response":{"status":"success","payload":{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"summary only"}]}]}}}`
	f := newProgressFixture(t, []string{reasoning, `{"request_id":"req","status":"pending"}`, reasoning, progressFinal}, time.Millisecond)
	var got []Delta
	_, err := f.runner.Run(context.Background(), f.request, func(d Delta) error { got = append(got, d); return nil })
	if err != nil || len(got) != 2 || got[0].Text != "" || got[0].Reasoning != "summary only" {
		t.Fatalf("reasoning callback: %+v %v", got, err)
	}
	f = newProgressFixture(t, []string{progressPending}, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	_, err = f.runner.Run(ctx, f.request, func(d Delta) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) || f.stops.Load() != 1 {
		t.Fatalf("cancel did not stop: %v, stops=%d", err, f.stops.Load())
	}
}

func TestRunnerStartProgressAndTerminalWriteFailure(t *testing.T) {
	f := newProgressFixture(t, []string{progressFinal}, time.Second)
	f.startBody = progressPending
	var got []Delta
	_, err := f.runner.Run(context.Background(), f.request, func(d Delta) error {
		if len(d.Progress) > 0 && f.polls.Load() != 0 {
			t.Error("start progress was delayed")
		}
		got = append(got, d)
		return nil
	})
	if err != nil || len(got) != 2 || len(got[0].Progress) != 2 {
		t.Fatalf("start progress: %+v %v", got, err)
	}
	f = newProgressFixture(t, []string{progressFinal}, time.Second)
	_, err = f.runner.Run(context.Background(), f.request, func(Delta) error { return context.Canceled })
	if !errors.Is(err, context.Canceled) || f.stops.Load() != 0 {
		t.Fatalf("known terminal triggered stop: %v %d", err, f.stops.Load())
	}
}

func progressSSE(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.TrimPrefix(line, "data: ") == "[DONE]" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
			t.Fatalf("invalid SSE: %s: %v", line, err)
		}
		result = append(result, data)
	}
	return result
}

func TestResponsesProgressItemsAndBridgeIsolation(t *testing.T) {
	for _, bridge := range []bool{false, true} {
		t.Run(fmt.Sprint(bridge), func(t *testing.T) {
			// A tool-looking narration must remain commentary and never reach the bridge parser.
			pending := strings.Replace(progressPending, "working", "```codex-exec\\nawait tools.exec_command({cmd: \\\"wrong\\\"});\\n```", 1)
			extended := strings.Replace(pending, "thinking", "thinking more", 1)
			f := newProgressFixture(t, []string{pending, extended, progressFinal}, time.Millisecond)
			rec := httptest.NewRecorder()
			f.handler.streamResponses(rec, httptest.NewRequest("POST", "/v1/responses", nil), f.request, &responsesTurn{id: "resp-local", publicModel: "test", bridge: bridge, isAux: true, execToolName: "exec"})
			events := progressSSE(t, rec.Body.String())
			added := map[string]float64{}
			done := map[string]map[string]any{}
			var completed []any
			var reasoning bool
			for _, event := range events {
				switch event["type"] {
				case "response.output_item.added":
					item := event["item"].(map[string]any)
					added[item["id"].(string)] = event["output_index"].(float64)
				case "response.reasoning_summary_text.delta":
					reasoning = true
				case "response.output_item.done":
					item := event["item"].(map[string]any)
					id := item["id"].(string)
					if index, ok := added[id]; !ok || index != event["output_index"] {
						t.Fatalf("done without matching add: %+v", event)
					}
					done[id] = item
				case "response.completed":
					completed = event["response"].(map[string]any)["output"].([]any)
				}
				if id, ok := event["item_id"].(string); ok {
					if index, exists := added[id]; !exists || index != event["output_index"] {
						t.Fatalf("event without matching item: %+v", event)
					}
				}
			}
			if events[len(events)-1]["type"] != "response.completed" {
				t.Fatal("terminal event must be last")
			}
			if !reasoning || len(completed) != 3 || len(done) != 3 {
				t.Fatalf("incomplete progress lifecycle: %s", rec.Body.String())
			}
			finals := 0
			for i, raw := range completed {
				item := raw.(map[string]any)
				id := item["id"].(string)
				if added[id] != float64(i) || !reflect.DeepEqual(done[id], item) {
					t.Fatalf("completed item mismatch: %+v", item)
				}
				if item["type"] == "reasoning" && item["summary"].([]any)[0].(map[string]any)["text"] != "thinking more" {
					t.Fatalf("lost cumulative reasoning extension: %+v", item)
				}
				if item["type"] == "message" && item["phase"] != "commentary" {
					finals++
					if item["content"].([]any)[0].(map[string]any)["text"] != "21" {
						t.Fatal("narration changed final answer")
					}
				} else if item["type"] != "message" && item["type"] != "reasoning" {
					t.Fatalf("narration became tool call: %+v", item)
				}
			}
			if finals != 1 {
				t.Fatalf("final answer count: %d", finals)
			}
		})
	}
}

func TestChatAndAnthropicProgressIsolation(t *testing.T) {
	for _, api := range []string{"chat", "messages"} {
		f := newProgressFixture(t, []string{progressPending, progressFinal}, time.Millisecond)
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", nil)
		if api == "chat" {
			f.handler.streamChat(rec, r, f.request, "chat-id", 1, "test", nil, nil)
		} else {
			f.handler.streamAnthropic(rec, r, f.request, "msg-id", "test")
		}
		events := progressSSE(t, rec.Body.String())
		var text, reasoning, progress strings.Builder
		for _, e := range events {
			var delta map[string]any
			if api == "chat" {
				delta = e["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
			} else if e["type"] == "content_block_delta" {
				delta = e["delta"].(map[string]any)
			}
			for key, target := range map[string]*strings.Builder{"content": &text, "text": &text, "reasoning_content": &reasoning, "progress_content": &progress} {
				if s, ok := delta[key].(string); ok {
					target.WriteString(s)
				}
			}
		}
		if text.String() != "21" {
			t.Fatalf("%s answer: %q", api, text.String())
		}
		if api == "chat" && (reasoning.String() != "thinking" || progress.String() != "working") {
			t.Fatalf("chat lost progress: %s", rec.Body.String())
		}
		if api == "messages" && (strings.Contains(rec.Body.String(), "thinking") || strings.Contains(rec.Body.String(), "working")) {
			t.Fatal("unsigned thinking corrupted Anthropic text")
		}
	}
}

func TestResponsesProgressToolIndices(t *testing.T) {
	for _, kind := range []string{"custom", "function"} {
		t.Run(kind, func(t *testing.T) {
			js := "```codex-exec\nawait tools.exec_command({cmd: \"pwd\"});\n```"
			encoded, _ := json.Marshal(js)
			final := strings.Replace(progressFinal, `"21"`, string(encoded), 1)
			f := newProgressFixture(t, []string{progressPending, final}, time.Millisecond)
			rec := httptest.NewRecorder()
			f.handler.streamResponses(rec, httptest.NewRequest("POST", "/v1/responses", nil), f.request, &responsesTurn{id: "resp-tools", publicModel: "test", bridge: true, isAux: true, execToolName: "exec", execKind: kind})
			events := progressSSE(t, rec.Body.String())
			completed := events[len(events)-1]
			if completed["type"] != "response.completed" {
				t.Fatalf("missing completion: %s", rec.Body.String())
			}
			output := completed["response"].(map[string]any)["output"].([]any)
			if len(output) != 3 {
				t.Fatalf("output: %+v", output)
			}
			call := output[2].(map[string]any)
			if call["type"] != kind+"_tool_call" && !(kind == "function" && call["type"] == "function_call") {
				t.Fatalf("tool type: %+v", call)
			}
			for _, e := range events {
				if e["type"] == "response.output_item.added" || e["type"] == "response.output_item.done" {
					item := e["item"].(map[string]any)
					if item["id"] == call["id"] && (e["output_index"] != float64(2) || !reflect.DeepEqual(item, call)) {
						t.Fatalf("tool index mismatch: %+v", e)
					}
				}
			}
		})
	}
}
