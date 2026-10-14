package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/logx"
	"github.com/oai-prism/oaiprism/internal/tokens"
)

func plainContextBody(api string, stream bool, text string) map[string]any {
	body := map[string]any{"model": "gpt-5", "stream": stream, "tools": []any{}, "tool_choice": "none"}
	if api == "responses" {
		body["input"] = text
	} else {
		body["messages"] = []any{map[string]any{"role": "user", "content": text}}
	}
	if api == "messages" {
		delete(body, "tool_choice")
		body["max_tokens"] = 1024
	}
	return body
}

func TestPlainContextE2E_UnrestorablePrefillRejectsBeforeSideEffects(t *testing.T) {
	for _, api := range []string{"chat/completions", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", api, stream), func(t *testing.T) {
				up := &fakeUpstream{t: t}
				ts, _ := newTestServer(t, up, goodAccount(), nil)
				body := plainContextBody(api, stream, "")
				messages := []any{map[string]any{"role": "user", "content": strings.Repeat("record ", 16000)}, map[string]any{"role": "assistant", "content": "prefill"}}
				if api == "responses" {
					body["input"] = messages
				} else {
					body["messages"] = messages
				}
				code, out := postClientTool(t, ts.URL, api, body)
				if code != 400 || startCount(up) != 0 || strings.Contains(out, "response.created") {
					t.Fatalf("%d %.300s", code, out)
				}
				up.mu.Lock()
				defer up.mu.Unlock()
				if up.projectCount != 0 {
					t.Fatal("unsupported prefill created project")
				}
			})
		}
	}
}

func TestPlainContextE2E_RestoreAuthAndRateFailuresUpdateHealth(t *testing.T) {
	for _, status := range []int{401, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			up := &fakeUpstream{t: t, preservedContext: true, restoreHTTP: status}
			upstream := httptest.NewServer(up.handler())
			defer upstream.Close()
			cfg := config.Default()
			cfg.Upstream.BaseURL = upstream.URL
			cfg.Upstream.MaxRetries = 0
			cfg.Creds.Accounts = goodAccount()
			cfg.Creds.AutoRefresh = false
			cfg.Creds.Mode = "static"
			cfg.Facade.ModelCatalog.Enabled = false
			cfg.Pool.HealthCheck = false
			srv, err := New(cfg, logx.Setup("error", "text"))
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			code, out := postClientTool(t, ts.URL, "chat/completions", plainContextBody("chat/completions", false, strings.Repeat("record ", 16000)))
			if code != 502 || !strings.Contains(out, "context_restore_failed") || startCount(up) != 1 {
				t.Fatalf("%d %.300s", code, out)
			}
			a := srv.pool.Get("main")
			if status == 401 && !a.AuthFailed() {
				t.Fatal("restore auth failure did not mark account unhealthy")
			}
			if a.CooldownRemaining(time.Now()) <= 0 {
				t.Fatal("restore failure did not cool account")
			}
		})
	}
}

func TestPlainContextE2E_FinalFailureNeverReplaysRestore(t *testing.T) {
	up := &fakeUpstream{t: t, preservedContext: true, finalRestoreHTTP: 502}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	code, out := postClientTool(t, ts.URL, "chat/completions", plainContextBody("chat/completions", false, strings.Repeat("record ", 16000)))
	if code != 502 || !strings.Contains(out, "context_restore_failed") || startCount(up) != 3 {
		t.Fatalf("final inference was replayed: %d %.300s starts=%d", code, out, startCount(up))
	}
}

func TestPlainContextE2E_LargeHistoryAndNativeDelta(t *testing.T) {
	up := &fakeUpstream{t: t, preservedContext: true, replyParts: []string{"complete answer"}}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	initial := "FIRST\n" + strings.Repeat("data record\n", 12000) + "\nLAST"
	body := plainContextBody("chat/completions", false, "continue")
	body["messages"] = []any{map[string]any{"role": "user", "content": initial}, map[string]any{"role": "assistant", "content": "ack"}, map[string]any{"role": "user", "content": "continue"}}
	code, out := postClientTool(t, ts.URL, "chat/completions", body)
	if code != 200 {
		t.Fatalf("%d %.300s", code, out)
	}
	n := startCount(up)
	assertPreservedFragments(t, up, 0, n-1, initial)
	delta := "DELTA_BEGIN\n" + strings.Repeat("new record\n", 12000) + "\nDELTA_END"
	body["messages"] = append(body["messages"].([]any), map[string]any{"role": "assistant", "content": "complete answer"}, map[string]any{"role": "user", "content": delta}, map[string]any{"role": "assistant", "content": "extra"}, map[string]any{"role": "user", "content": "finish"})
	code, out = postClientTool(t, ts.URL, "chat/completions", body)
	if code != 200 {
		t.Fatalf("delta %d %.300s", code, out)
	}
	last := startCount(up) - 1
	assertPreservedFragments(t, up, n, last, delta)
	if startConv(t, up, last) != startConv(t, up, 0) {
		t.Fatal("delta did not continue restored conversation")
	}
}

func TestPlainContextE2E_RejectsLogicalBudgetBeforeStream(t *testing.T) {
	for _, api := range []string{"chat/completions", "responses", "messages"} {
		t.Run(api, func(t *testing.T) {
			up := &fakeUpstream{t: t}
			ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) { c.Facade.MaxContextBytes = 12000 })
			code, out := postClientTool(t, ts.URL, api, plainContextBody(api, true, strings.Repeat("record ", 2000)))
			if code != 400 || startCount(up) != 0 || strings.Contains(out, "response.created") {
				t.Fatalf("%d %.300s", code, out)
			}
		})
	}
}

func TestPlainContextE2E_RestoreFailureAllProtocols(t *testing.T) {
	for _, api := range []string{"chat/completions", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", api, stream), func(t *testing.T) {
				up := &fakeUpstream{t: t, preservedContext: true, restoreBadAck: true}
				ts, _ := newTestServer(t, up, goodAccount(), nil)
				code, out := postClientTool(t, ts.URL, api, plainContextBody(api, stream, strings.Repeat("record ", 16000)))
				want := 502
				if stream {
					want = 200
				}
				if code != want || !strings.Contains(out, "context_restore_failed") || strings.Contains(out, "response.completed") || strings.Contains(out, "message_stop") || startCount(up) != 1 {
					t.Fatalf("%d %.400s", code, out)
				}
			})
		}
	}
}

func TestPlainContextE2E_LargeCurrentAllProtocols(t *testing.T) {
	for _, api := range []string{"chat/completions", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", api, stream), func(t *testing.T) {
				up := &fakeUpstream{t: t, preservedContext: true, replyParts: []string{"complete answer"}}
				ts, _ := newTestServer(t, up, goodAccount(), nil)
				text := "EARLY\n" + strings.Repeat("中文🙂 record \"value\"\n", 40000) + "\nLATE"
				code, out := postClientTool(t, ts.URL, api, plainContextBody(api, stream, text))
				if code != 200 || !strings.Contains(out, "complete answer") {
					t.Fatalf("plain context rejected: %d %.300s", code, out)
				}
				n := startCount(up)
				if n < 10 {
					t.Fatalf("near-limit input not restored: starts=%d", n)
				}
				assertPreservedFragments(t, up, 0, n-1, text)
				if !stream && api == "chat/completions" {
					var response struct {
						Usage struct {
							PromptTokens int `json:"prompt_tokens"`
						}
					}
					if json.Unmarshal([]byte(out), &response) != nil || response.Usage.PromptTokens < tokens.Count(text) {
						t.Fatal("cold restore usage omitted full logical context")
					}
				}
			})
		}
	}
}
