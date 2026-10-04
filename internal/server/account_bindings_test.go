package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
)

func TestAdmin_KeyBindingsAndChatAccountSelection(t *testing.T) {
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, []config.AccountConfig{
		{ID: "a", AccessToken: "test-token-a"},
		{ID: "b", AccessToken: "test-token-b"},
		{ID: "c", AccessToken: "test-token-c"},
	}, func(c *config.Config) {
		c.Facade.APIKeys = []string{"admin-test-key"}
		c.Pool.Strategy = "round_robin"
	})
	admin := map[string]string{"Authorization": "Bearer admin-test-key"}
	call := func(method, path, body string, headers map[string]string, want int) string {
		t.Helper()
		code, out := doLocal(t, method, ts.URL+path, body, headers)
		if code != want {
			t.Fatalf("%s %s status %d want %d: %s", method, path, code, want, out)
		}
		return out
	}
	var key account.APIKeyItem
	out := call(http.MethodPost, "/admin/apikeys", `{"name":"multi","account_ids":["a","b"]}`, admin, 200)
	if err := json.Unmarshal([]byte(out), &key); err != nil || key.Key == "" {
		t.Fatal("key creation failed", err)
	}
	client := map[string]string{"Authorization": "Bearer " + key.Key}
	assertSelection := func(want []string) {
		t.Helper()
		var body struct {
			Accounts []account.Stats `json:"accounts"`
		}
		out := call(http.MethodGet, "/v1/accounts", "", client, 200)
		if err := json.Unmarshal([]byte(out), &body); err != nil || len(body.Accounts) != len(want) {
			t.Fatalf("selection list = %s", out)
		}
		for _, a := range body.Accounts {
			found := false
			for _, id := range want {
				found = found || id == a.ID
			}
			if !found {
				t.Fatal("selection list escaped scope")
			}
		}
	}
	assertSelection([]string{"a", "b"})
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		client["X-Oaiprism-Session"] = fmt.Sprintf("multi-%d", i)
		call(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}]}`, client, 200)
		up.mu.Lock()
		auth := up.startAuths[len(up.startAuths)-1]
		up.mu.Unlock()
		if auth != "Bearer test-token-a" && auth != "Bearer test-token-b" {
			t.Fatal("routing escaped bound accounts")
		}
		seen[auth] = true
	}
	if len(seen) != 2 {
		t.Fatal("multi-account binding did not schedule both accounts")
	}
	client["X-Oaiprism-Account"] = "c"
	call(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-6.1-sol","stream":true,"messages":[{"role":"user","content":"hello"}]}`, client, 403)
	client["X-Oaiprism-Account"] = "b"
	client["X-Oaiprism-Session"] = "pinned-b"
	call(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}]}`, client, 200)
	up.mu.Lock()
	if up.startAuths[len(up.startAuths)-1] != "Bearer test-token-b" {
		t.Error("explicit account selection ignored")
	}
	up.mu.Unlock()
	call(http.MethodPut, "/admin/apikeys/"+key.Key+"/bindings", `{"account_ids":["b"]}`, admin, 200)
	assertSelection([]string{"b"})
	call(http.MethodPut, "/admin/apikeys/"+key.Key+"/bindings", `{"account_ids":["missing"]}`, admin, 400)
	assertSelection([]string{"b"})
	call(http.MethodPut, "/admin/accounts/b", `{"enabled":false}`, admin, 200)
	var accounts struct {
		Accounts []account.Stats `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(call(http.MethodGet, "/admin/accounts", "", admin, 200)), &accounts); err != nil || len(accounts.Accounts) != 3 {
		t.Fatal("disabled account disappeared")
	}
	call(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}]}`, client, 502)
	delete(client, "X-Oaiprism-Account")
	call(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}]}`, client, 502)
	call(http.MethodPut, "/admin/accounts/b", `{"enabled":true}`, admin, 200)
	call(http.MethodPost, "/v1/responses", `{"model":"gpt-6.1-sol","input":"hello"}`, client, 200)
	call(http.MethodPost, "/v1/messages", `{"model":"gpt-6.1-sol","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`, client, 200)
	call(http.MethodGet, "/prism/api/auth/session", "", client, 200)
	up.mu.Lock()
	if up.lastAuth != "Bearer test-token-b" {
		t.Error("raw proxy escaped bound account")
	}
	up.mu.Unlock()
	client["X-Oaiprism-Account"] = "c"
	call(http.MethodGet, "/prism/api/auth/session", "", client, 403)
	delete(client, "X-Oaiprism-Account")
	call(http.MethodPost, "/admin/chat/sessions", `{"id":"selected","title":"test","model":"gpt-6.1-sol","reasoning_effort":"medium","account_id":"b"}`, admin, 200)
	var sessions []account.ChatSessionRecord
	if err := json.Unmarshal([]byte(call(http.MethodGet, "/admin/chat/sessions", "", admin, 200)), &sessions); err != nil || len(sessions) != 1 || sessions[0].AccountID != "b" {
		t.Fatal("chat selection not persisted")
	}
	call(http.MethodDelete, "/admin/accounts/b", "", admin, 200)
	assertSelection([]string{})
	call(http.MethodPost, "/v1/chat/completions", `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello"}]}`, client, 502)
	call(http.MethodPut, "/admin/apikeys/"+key.Key+"/bindings", `{"account_ids":[]}`, admin, 200)
	assertSelection([]string{"a", "c"})
	call(http.MethodDelete, "/admin/apikeys/"+key.Key, "", admin, 200)
	call(http.MethodGet, "/v1/accounts", "", client, 401)
}
