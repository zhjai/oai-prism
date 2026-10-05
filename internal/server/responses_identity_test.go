package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
				if len(items) != 1 {
					t.Fatalf("unexpected output: %#v", result)
				}
				wantType := "message"
				if mode == "bridge_custom" {
					wantType = "custom_tool_call"
				} else if mode == "bridge_function" {
					wantType = "function_call"
				}
				if items[0].(map[string]any)["type"] != wantType {
					t.Fatalf("wrong output branch: %#v", items[0])
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
