package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

func TestResponsesCommentaryDoesNotEnterNextTurn(t *testing.T) {
	for _, bridge := range []bool{false, true} {
		name := "ordinary"
		if bridge {
			name = "bridge"
		}
		t.Run(name, func(t *testing.T) {
			up := &fakeUpstream{t: t, replyParts: []string{"Synthetic ", "reply."}, finalOnlyPayload: true, actionFails: true}
			handler := up.handler()
			narration := "PROGRESS_MUST_NOT_REPLAY ```codex-exec\nawait tools.exec_command({cmd: \"PROGRESS_COMMAND_MUST_NOT_REPLAY\"});\n```"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, r)
				body := rec.Body.Bytes()
				if r.URL.Path == "/api/llm/response_with_tools_status" {
					var envelope map[string]any
					if json.Unmarshal(body, &envelope) == nil && envelope["status"] == "pending" {
						envelope["codex_live_progress"] = map[string]any{"eventPreviews": []any{
							map[string]any{"payload_type": "agent_message", "line_index": 1, "raw": map[string]any{"payload": map[string]any{"message": narration}}},
						}}
						body, _ = json.Marshal(envelope)
					}
				}
				for key, values := range rec.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(rec.Code)
				_, _ = w.Write(body)
			}))
			defer upstream.Close()
			ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) { c.Upstream.BaseURL = upstream.URL })
			input := []any{map[string]any{"type": "message", "role": "user", "content": "First request."}}
			body := map[string]any{"model": "gpt-5", "stream": true, "input": input}
			if bridge {
				body["tools"] = []any{map[string]any{"type": "custom", "name": "exec_command"}}
			}
			raw, _ := json.Marshal(body)
			code, stream := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", string(raw), map[string]string{"Content-Type": "application/json"})
			if code != http.StatusOK {
				t.Fatalf("first request: %d", code)
			}
			var output []any
			for _, line := range strings.Split(stream, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event["type"] == "response.completed" {
					output, _ = event["response"].(map[string]any)["output"].([]any)
				}
			}
			commentary := 0
			for _, item := range output {
				if item.(map[string]any)["phase"] == "commentary" {
					commentary++
				}
			}
			if commentary != 1 {
				t.Fatalf("first turn omitted commentary: %s", stream)
			}
			body["stream"] = false
			body["input"] = append(append(input, output...), map[string]any{"type": "message", "role": "user", "content": "Continue."})
			raw, _ = json.Marshal(body)
			code, result := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", string(raw), map[string]string{"Content-Type": "application/json"})
			if code != http.StatusOK {
				t.Fatalf("second request: %d %s", code, result)
			}
			up.mu.Lock()
			second, _ := json.Marshal(up.startBodies[len(up.startBodies)-1]["input"])
			up.mu.Unlock()
			if strings.Contains(string(second), "PROGRESS_MUST_NOT_REPLAY") || strings.Contains(string(second), "PROGRESS_COMMAND_MUST_NOT_REPLAY") || !strings.Contains(string(second), "Synthetic reply.") {
				t.Fatalf("commentary replayed or final history lost: %s", second)
			}
		})
	}
}
