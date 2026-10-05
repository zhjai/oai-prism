package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

func TestResponsesPublicIDStableAndContinues(t *testing.T) {
	for _, mode := range []string{"ordinary", "bridge_text", "bridge_custom", "bridge_function"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				up := &fakeUpstream{t: t, replyParts: []string{"Synthetic reply."}}
				if mode == "bridge_custom" || mode == "bridge_function" {
					up.replyParts = []string{"```codex-exec\nconst result = await tools.exec_command({cmd: \"printf synthetic\"}); text(result);\n```"}
				}
				handler := up.handler()
				upstreamID := "upstream-" + t.Name()
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, r)
					body := rec.Body.Bytes()
					var envelope map[string]any
					if json.Unmarshal(body, &envelope) == nil {
						if response, ok := envelope["response"].(map[string]any); ok {
							if payload, ok := response["payload"].(map[string]any); ok {
								payload["id"] = upstreamID
								body, _ = json.Marshal(envelope)
							}
						}
					}
					for k, values := range rec.Header() {
						w.Header()[k] = values
					}
					w.WriteHeader(rec.Code)
					_, _ = w.Write(body)
				}))
				defer upstream.Close()
				ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) { c.Upstream.BaseURL = upstream.URL })
				body := map[string]any{
					"model": "gpt-5", "stream": stream,
					"input": []any{map[string]any{"role": "user", "content": "Remember: " + t.Name()}},
				}
				if mode != "ordinary" {
					kind := "custom"
					if mode == "bridge_function" {
						kind = "function"
					}
					body["tools"] = []any{map[string]any{"type": kind, "name": "exec_command"}}
				}
				raw, _ := json.Marshal(body)
				code, output := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", string(raw), map[string]string{"Content-Type": "application/json"})
				if code != http.StatusOK {
					t.Fatalf("first response: %d: %s", code, output)
				}
				var publicID string
				var result map[string]any
				added := map[string]int{}
				completedItems := map[string]map[string]any{}
				if stream {
					seenCreated, seenCompleted := false, false
					for _, line := range strings.Split(output, "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event map[string]any
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
							t.Fatalf("invalid SSE: %v", err)
						}
						if event["type"] == "response.output_item.added" || event["type"] == "response.output_item.done" {
							item, ok := event["item"].(map[string]any)
							if !ok {
								t.Fatalf("missing stream item: %#v", event)
							}
							itemID, _ := item["id"].(string)
							index, ok := event["output_index"].(float64)
							if itemID == "" || !ok || index < 0 || index != float64(int(index)) {
								t.Fatalf("invalid stream item identity: %#v", event)
							}
							if event["type"] == "response.output_item.added" {
								if _, exists := added[itemID]; exists {
									t.Fatalf("duplicate item addition: %q", itemID)
								}
								added[itemID] = int(index)
							} else {
								if prior, exists := added[itemID]; !exists || prior != int(index) {
									t.Fatalf("item identity changed: %#v", event)
								}
								completedItems[itemID] = item
							}
						}
						response, ok := event["response"].(map[string]any)
						if !ok {
							continue
						}
						id, _ := response["id"].(string)
						if publicID == "" {
							publicID = id
						}
						if id == "" || id != publicID || id == upstreamID {
							t.Fatalf("response identity changed: first=%q current=%q", publicID, id)
						}
						seenCreated = seenCreated || event["type"] == "response.created"
						if event["type"] == "response.completed" {
							seenCompleted, result = true, response
						}
					}
					if !seenCreated || !seenCompleted {
						t.Fatalf("missing lifecycle events: %s", output)
					}
				} else {
					if err := json.Unmarshal([]byte(output), &result); err != nil {
						t.Fatal(err)
					}
					publicID, _ = result["id"].(string)
					if !strings.HasPrefix(publicID, "resp_") || publicID == upstreamID {
						t.Fatalf("expected gateway public ID: %q", publicID)
					}
				}
				items, _ := result["output"].([]any)
				wantType := "message"
				if mode == "bridge_custom" {
					wantType = "custom_tool_call"
				} else if mode == "bridge_function" {
					wantType = "function_call"
				}
				finalCount, reasoningCount := 0, 0
				for index, rawItem := range items {
					item, ok := rawItem.(map[string]any)
					if !ok {
						t.Fatalf("invalid output item: %#v", rawItem)
					}
					if stream {
						itemID, _ := item["id"].(string)
						if streamedIndex, exists := added[itemID]; !exists || streamedIndex != index || !reflect.DeepEqual(completedItems[itemID], item) {
							t.Fatalf("completed output differs from stream: %#v", item)
						}
					}
					if item["type"] == "reasoning" {
						reasoningCount++
						summary, _ := item["summary"].([]any)
						if len(summary) == 0 {
							t.Fatalf("empty reasoning item: %#v", item)
						}
						for _, rawPart := range summary {
							part, ok := rawPart.(map[string]any)
							if !ok || part["type"] != "summary_text" {
								t.Fatalf("invalid reasoning part: %#v", rawPart)
							}
							if text, ok := part["text"].(string); !ok || text == "" {
								t.Fatalf("empty reasoning text: %#v", part)
							}
						}
						continue
					}
					finalCount++
					if item["type"] != wantType || item["phase"] == "commentary" {
						t.Fatalf("wrong final output branch: %#v", item)
					}
				}
				if finalCount != 1 || (stream && reasoningCount == 0) || (!stream && reasoningCount != 0) {
					t.Fatalf("expected one final item with streamed reasoning: %#v", result)
				}
				if stream && (len(added) != len(items) || len(completedItems) != len(items)) {
					t.Fatalf("streamed items missing from completed output: %#v", result)
				}
				cid := startConv(t, up, 0)
				followup := fmt.Sprintf(`{"model":"gpt-5","stream":false,"input":"Continue the remembered conversation.","previous_response_id":%q}`, publicID)
				code, output = doLocal(t, http.MethodPost, ts.URL+"/v1/responses", followup, map[string]string{"Content-Type": "application/json"})
				if code != http.StatusOK || cid == "" || startConv(t, up, 1) != cid {
					t.Fatalf("public ID did not resume original conversation: %d, %s", code, output)
				}
			})
		}
	}
}
