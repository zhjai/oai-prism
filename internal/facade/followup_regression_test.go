package facade

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/metrics"
	"github.com/oai-prism/oaiprism/internal/prism"
)

func TestExecToolKindStructuredJSON(t *testing.T) {
	for _, tc := range []struct{ name, tools, input, want string }{
		{"reordered", `[{"name":"exec_command","parameters":{},"type":"function"}]`, "", "function"},
		{"whitespace", "[ {\n\t\"name\": \"exec\",\n\"type\" : \"function\" } ]", "", "function"},
		{"custom", `[{"name":"exec_command","type":"custom"}]`, "", "custom"},
		{"unrelated", `[{"type":"function","name":"read_file"}]`, "", "custom"},
		{"nested_description", `[{"type":"custom","name":"exec","schema":{"type":"function","name":"exec_command"}}]`, "", "custom"},
		{"input_fallback", " null ", `[{"name":"exec","type":"function"}]`, "function"},
		{"malformed", `[{"type":"function","name":"exec_command"}`, "", "custom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExecToolKind(map[string]json.RawMessage{"tools": json.RawMessage(tc.tools), "input": json.RawMessage(tc.input)})
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAuxRequiresCodexMetadata(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("User-Agent", "openai-python/1.0")
	for _, tc := range []struct {
		metadata string
		want     bool
	}{
		{`{"trace_id":"synthetic"}`, false},
		{`{}`, false},
		{`null`, false},
		{`{"x-codex-turn-metadata":"invalid"}`, false},
		{`{"x-codex-turn-metadata":"{\"request_kind\":\"title\"}"}`, true},
	} {
		raw := map[string]json.RawMessage{"input": json.RawMessage(`"hello"`), "client_metadata": json.RawMessage(tc.metadata)}
		if got := isCodexAuxRequest(r, raw, false, false); got != tc.want {
			t.Errorf("metadata=%s: got %v, want %v", tc.metadata, got, tc.want)
		}
	}
}

func TestRunnerCloseJoinsCleanup(t *testing.T) {
	r := NewRunner(config.Default(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, metrics.NewApp())
	done := make(chan struct{})
	go func() {
		var callers sync.WaitGroup
		for range 4 {
			callers.Add(1)
			go func() { defer callers.Done(); r.Close() }()
		}
		callers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup workers did not stop")
	}
	r.Close()
}

func TestNativeRestoredBindingSelectsOwningAccount(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.Strategy = "round_robin"
	cfg.Creds.Accounts = []config.AccountConfig{{ID: "first", AccessToken: "synthetic-first"}, {ID: "owner", AccessToken: "synthetic-owner"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := account.NewPool(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	r := NewRunner(cfg, log, pool, nil, metrics.NewApp())
	defer r.Close()
	key := "k:restart-routing|h:" + t.Name()
	r.UseNativeStore(&memNativeStore{recs: map[string]account.NativeBindingRecord{key: {Key: key, Account: "owner", Project: "project", CID: "conversation", Updated: time.Now()}}})
	req := &RunRequest{StickyKey: key}
	(&Handler{}).attachNative(req, &nativeTurn{key: key, strong: true, conv: nativeConv("", "continue")})
	if req.BoundAccountID != "owner" || req.ProjectID != "project" {
		t.Fatalf("binding not restored: %+v", req)
	}
	lease, err := r.acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Account.ID != "owner" {
		t.Fatalf("selected %q instead of restored owner", lease.Account.ID)
	}
	lease.Release()
	req.AccountID = "first"
	lease, err = r.acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Account.ID != "first" {
		t.Fatal("explicit selection must take precedence")
	}
	lease.Release()
	req.AccountID = ""
	pool.Get("owner").Cooldown(time.Now(), time.Minute)
	lease, err = r.acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Account.ID != "first" {
		t.Fatal("unavailable bound account must allow normal fallback")
	}
}

func TestSessionChainDeltaFilterPreservesAlias(t *testing.T) {
	key := "k:alias-filter|h:" + t.Name()
	sessionChainRecord(key, &RunResult{AccountID: "owner", ProjectID: "project", ResponseID: "response-alias-filter", ConversationID: "conversation-alias-filter"}, "model")
	alias := chainAlias(tenantOfKey(key), "prev", "response-alias-filter")
	file := prism.CodexDeltaFile{FilePath: "file.txt", Status: "added", Diff: diffRaw("@@ -0,0 +1 @@\n+text\n")}
	if len(sessionChainFilterNewDeltaFiles(alias, []prism.CodexDeltaFile{file})) != 1 {
		t.Fatal("initial file was filtered")
	}
	if len(sessionChainFilterNewDeltaFiles(key, []prism.CodexDeltaFile{file})) != 0 {
		t.Fatal("alias and primary key did not share file state")
	}
	sessionChain.mu.RLock()
	same := sessionChain.entries[key] == sessionChain.entries[alias]
	sessionChain.mu.RUnlock()
	if !same {
		t.Fatal("filter replaced alias binding")
	}
}
