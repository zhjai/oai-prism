package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

func preservedResponseBody(text string) map[string]any {
	body := clientToolBody("responses", false)
	body["input"] = text
	return body
}

func TestPreservedContextE2E_WrappedSizeErrorIsSanitized(t *testing.T) {
	for _, api := range []string{"chat/completions", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", api, stream), func(t *testing.T) {
				up := &fakeUpstream{t: t, preservedContext: true, restoreTooLarge: true}
				ts, _ := newTestServer(t, up, goodAccount(), nil)
				body := clientToolBody(api, stream)
				text := strings.Repeat("record ", 16000)
				if api == "responses" {
					body["input"] = text
				} else {
					body["messages"] = []any{map[string]any{"role": "user", "content": text}}
				}
				code, out := postClientTool(t, ts.URL, api, body)
				want := 502
				if stream {
					want = 200
				}
				if code != want || !strings.Contains(out, "context_restore_failed") || strings.Contains(out, "PRIVATE_SYNTHETIC_BODY") || strings.Contains(out, "context_length_exceeded") || startCount(up) != 1 {
					t.Fatalf("unsanitized nested restore error: %d %.400s", code, out)
				}
			})
		}
	}
}

func assertPreservedFragments(t *testing.T, up *fakeUpstream, first, last int, original string) {
	t.Helper()
	var data strings.Builder
	cid := startConv(t, up, first)
	for i := first; i < last; i++ {
		system, user := requireSystemUser(t, upstreamInput(t, up, i))
		if len(system)+len(user) > 98304 || startConv(t, up, i) != cid {
			t.Fatal("restore escaped transport budget or registered conversation")
		}
		if !strings.HasPrefix(user, "[Client context fragment ") {
			t.Fatal("not a restore fragment")
		}
		_, fragment, found := strings.Cut(user, "\n")
		if !found {
			t.Fatal("fragment missing data")
		}
		data.WriteString(fragment)
	}
	if strings.Count(data.String(), original) != 1 {
		t.Fatal("client transcript was changed, clipped or omitted")
	}
	if startConv(t, up, last) != cid {
		t.Fatal("final inference did not reuse restored conversation")
	}
}

func TestPreservedContextE2E_RestoresBeyondOld768KiBCap(t *testing.T) {
	up := &fakeUpstream{t: t, replyParts: []string{toolBridgeReply}, preservedContext: true}
	accounts := []config.AccountConfig{{ID: "a", AccessToken: "a-token", MaxConcurrency: 2}, {ID: "b", AccessToken: "b-token", MaxConcurrency: 2}}
	ts, _ := newTestServer(t, up, accounts, nil)
	input := "EARLIEST \n" + strings.Repeat("中文 record \"value\"\n", 44000) + "\nLATEST "
	data, _ := json.Marshal(preservedResponseBody(input))
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", string(data), map[string]string{"Content-Type": "application/json", "X-Oaiprism-Account": "b"})
	if code != 200 || !strings.Contains(out, `"type":"function_call"`) {
		t.Fatalf("%d %.300s", code, out)
	}
	n := startCount(up)
	if n < 10 {
		t.Fatalf("900KiB+ context must be restored in full: starts=%d", n)
	}
	assertPreservedFragments(t, up, 0, n-1, input)
	up.mu.Lock()
	for _, auth := range up.startAuths {
		if auth != "Bearer b-token" {
			t.Fatal("restore escaped explicitly pinned account")
		}
	}
	up.mu.Unlock()
	system, user := requireSystemUser(t, upstreamInput(t, up, n-1))
	if !strings.Contains(system, "<client_tools>") || !strings.Contains(system, "exactly one call of lookup") || !strings.Contains(user, "[End of client context restoration]") {
		t.Fatal("final tool catalog/choice not reasserted")
	}
}

func TestPreservedContextE2E_OversizedCurrentResultRetainsExactJSON(t *testing.T) {
	up := &fakeUpstream{t: t, replyParts: []string{toolBridgeReply}, preservedContext: true}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	code, out := postClientTool(t, ts.URL, "responses", clientToolBody("responses", false))
	var first struct {
		ID     string
		Output []struct {
			CallID string `json:"call_id"`
		}
	}
	if code != 200 || json.Unmarshal([]byte(out), &first) != nil || len(first.Output) < 2 {
		t.Fatalf("first tool turn failed: %d %.200s", code, out)
	}
	callID := first.Output[1].CallID
	output := "EARLIEST\n" + strings.Repeat("中文 \"value\" \\ path\n", 8000) + "\nLATEST"
	encoded, _ := json.Marshal(output)
	up.mu.Lock()
	up.replyParts = []string{"result received"}
	up.mu.Unlock()
	body := map[string]any{"model": "gpt-5", "previous_response_id": first.ID, "tool_choice": "none", "input": []any{map[string]any{"type": "function_call_output", "call_id": callID, "output": output}}}
	n := startCount(up)
	code, out = postClientTool(t, ts.URL, "responses", body)
	if code != 200 || !strings.Contains(out, "result received") {
		t.Fatalf("oversized result failed: %d %.200s", code, out)
	}
	last := startCount(up) - 1
	if last-n < 2 || startConv(t, up, last) != startConv(t, up, 0) {
		t.Fatal("oversized result did not continue same registered conversation")
	}
	assertPreservedFragments(t, up, n, last, string(encoded))
}

func TestPreservedContextE2E_FullReplayUsesSmallDelta(t *testing.T) {
	up := &fakeUpstream{t: t, replyParts: []string{toolBridgeReply}, preservedContext: true}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	initial := "ORIGINAL_TASK " + strings.Repeat("record ", 16000)
	body := clientToolBody("chat/completions", false)
	body["messages"] = []any{map[string]any{"role": "user", "content": initial}}
	code, out := postClientTool(t, ts.URL, "chat/completions", body)
	if code != 200 {
		t.Fatalf("%d %.200s", code, out)
	}
	var response struct {
		Choices []struct{ Message json.RawMessage }
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil || len(response.Choices) == 0 {
		t.Fatal("missing first assistant message")
	}
	var assistant map[string]any
	_ = json.Unmarshal(response.Choices[0].Message, &assistant)
	callID := assistant["tool_calls"].([]any)[0].(map[string]any)["id"].(string)
	n := startCount(up)
	body["messages"] = []any{map[string]any{"role": "user", "content": initial}, assistant, map[string]any{"role": "tool", "tool_call_id": callID, "content": "SMALL_RESULT"}, map[string]any{"role": "user", "content": "Next lookup please"}}
	code, out = postClientTool(t, ts.URL, "chat/completions", body)
	if code != 200 || startCount(up) != n+1 {
		t.Fatalf("large full replay failed instead of one small delta: status=%d starts=%d %.200s", code, startCount(up)-n, out)
	}
	_, user := requireSystemUser(t, upstreamInput(t, up, n))
	if startConv(t, up, n) != startConv(t, up, 0) || strings.Contains(user, initial) || !strings.Contains(user, "SMALL_RESULT") {
		t.Fatal("did not preserve small native continuation")
	}
}

func TestPreservedContextE2E_RestoreFailureDropsBindingAndNeverFallsBack(t *testing.T) {
	up := &fakeUpstream{t: t, replyParts: []string{toolBridgeReply}, preservedContext: true, restoreBadAck: true}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	body := preservedResponseBody("ORIGINAL_TASK " + strings.Repeat("record ", 16000))
	code, out := postClientTool(t, ts.URL, "responses", body)
	if code != 502 || !strings.Contains(out, "context_restore_failed") || startCount(up) != 1 {
		t.Fatalf("restore failure generated a fallback: %d %.200s starts=%d", code, out, startCount(up))
	}
	oldCID := startConv(t, up, 0)
	up.mu.Lock()
	up.restoreBadAck = false
	up.mu.Unlock()
	code, out = postClientTool(t, ts.URL, "responses", body)
	if code != 200 || startConv(t, up, 1) == oldCID {
		t.Fatalf("retry reused partially restored state: %d %.200s", code, out)
	}
}

func TestPreservedContextE2E_RegistrationFailureCannotSendOversize(t *testing.T) {
	up := &fakeUpstream{t: t, actionFails: true, preservedContext: true}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	code, out := postClientTool(t, ts.URL, "responses", preservedResponseBody(strings.Repeat("record ", 16000)))
	if code != http.StatusBadGateway || !strings.Contains(out, "context_restore_failed") || startCount(up) != 0 {
		t.Fatalf("registration failure sent oversize full input: %d %.200s", code, out)
	}
}

func TestPreservedContextE2E_LogicalBudgetFailsBeforeSideEffects(t *testing.T) {
	up := &fakeUpstream{t: t, preservedContext: true}
	ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) { c.Facade.MaxContextBytes = 12000 })
	code, out := postClientTool(t, ts.URL, "responses", preservedResponseBody(strings.Repeat("record ", 2000)))
	if code != 400 || !strings.Contains(out, "max_context_bytes") || startCount(up) != 0 {
		t.Fatalf("logical budget was not enforced before start: %d %.200s", code, out)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.projectCount != 0 {
		t.Fatal(fmt.Sprintf("logical reject created%d projects", up.projectCount))
	}
}

func TestPreservedContextE2E_RestoreFailureAllProtocols(t *testing.T) {
	for _, api := range []string{"chat/completions", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", api, stream), func(t *testing.T) {
				up := &fakeUpstream{t: t, preservedContext: true, restoreBadAck: true}
				ts, _ := newTestServer(t, up, goodAccount(), nil)
				body := clientToolBody(api, stream)
				text := strings.Repeat("record ", 16000)
				if api == "responses" {
					body["input"] = text
				} else {
					body["messages"] = []any{map[string]any{"role": "user", "content": text}}
				}
				code, out := postClientTool(t, ts.URL, api, body)
				want := 502
				if stream {
					want = 200
				}
				if code != want || !strings.Contains(out, "context_restore_failed") || strings.Contains(out, "response.completed") || strings.Contains(out, "message_stop") || startCount(up) != 1 {
					t.Fatalf("incorrect failure terminal: %d %.300s", code, out)
				}
				if api == "messages" && !stream && !strings.Contains(out, `"type":"api_error"`) {
					t.Fatal("Anthropic error shape lost")
				}
			})
		}
	}
}
