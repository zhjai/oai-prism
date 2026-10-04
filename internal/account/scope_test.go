package account

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

func TestPool_AccountScopes(t *testing.T) {
	for _, strategy := range []string{"round_robin", "least_inflight", "random", "weighted", "sticky_hash"} {
		t.Run(strategy, func(t *testing.T) {
			p := testPool(t, strategy,
				config.AccountConfig{ID: "a", AccessToken: "a"},
				config.AccountConfig{ID: "b", AccessToken: "b"},
				config.AccountConfig{ID: "c", AccessToken: "c"},
			)
			p.Rebind("session", p.Get("c"))
			ctx := WithScope(context.Background(), Scope{Restricted: true, AccountIDs: []string{"a", "b"}})
			for i := 0; i < 10; i++ {
				l, err := p.Acquire(ctx, "session")
				if err != nil {
					t.Fatal(err)
				}
				if l.Account.ID == "c" {
					t.Fatal("sticky selection escaped scope")
				}
				l.Release()
			}
			if _, err := p.AcquirePinned(ctx, "c"); err == nil {
				t.Fatal("pin escaped scope")
			}
			if _, err := p.Acquire(WithScope(ctx, Scope{Restricted: true}), "session"); !errors.Is(err, ErrNoAccount) {
				t.Fatalf("empty restricted scope = %v", err)
			}
			p.Get("a").Cooldown(time.Now(), 30*time.Millisecond)
			p.Get("c").MarkSuccess()
			onlyA := WithScope(ctx, Scope{Restricted: true, AccountIDs: []string{"a"}})
			l, err := p.Acquire(onlyA, "")
			if err != nil || l.Account.ID != "a" {
				t.Fatalf("cooldown wait escaped scope: %v", err)
			}
			l.Release()
			p.Get("a").Cooldown(time.Now(), time.Hour)
			if _, err := p.AcquirePinned(onlyA, "a"); err == nil {
				t.Fatal("pin ignored cooldown")
			}
		})
	}
}

func TestPool_ToggleAndCounters(t *testing.T) {
	configs := []config.AccountConfig{{ID: "a", AccessToken: "a"}}
	p := testPool(t, "round_robin", configs...)
	l, err := p.Acquire(context.Background(), "session")
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	no, yes := false, true
	configs[0].Enabled = &no
	if err := p.Build(configs); err != nil {
		t.Fatal(err)
	}
	stats := p.Get("a").Stats(time.Now())
	if stats.Enabled || stats.Available || stats.Total != 1 || p.Size() != 1 {
		t.Fatalf("disabled stats: %+v", stats)
	}
	if l.Account.Acquire(time.Now()) {
		t.Fatal("old snapshot accepted disabled account")
	}
	if _, err := p.Acquire(context.Background(), "session"); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("disabled acquire = %v", err)
	}
	configs[0].Enabled = &yes
	if err := p.Build(configs); err != nil {
		t.Fatal(err)
	}
	l, err = p.Acquire(context.Background(), "session")
	if err != nil {
		t.Fatal("enable did not restore scheduling:", err)
	}
	l.Release()
}

func TestSQLite_LegacyMigrationAndBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
	CREATE TABLE accounts (id TEXT PRIMARY KEY, name TEXT NOT NULL, plan TEXT DEFAULT 'pro', email TEXT DEFAULT '', cookies TEXT DEFAULT '', access_token TEXT DEFAULT '', refresh_token TEXT DEFAULT '', max_concurrency INTEGER DEFAULT 2, tags TEXT DEFAULT '', updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
	CREATE TABLE api_keys (key TEXT PRIMARY KEY, name TEXT NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
	CREATE TABLE chat_sessions (id TEXT PRIMARY KEY, title TEXT NOT NULL, model TEXT NOT NULL, reasoning_effort TEXT NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
	INSERT INTO accounts (id,name,access_token) VALUES ('a','A','test-a'),('b','B','test-b');
	INSERT INTO api_keys (key,name) VALUES ('test-key','legacy');
	INSERT INTO chat_sessions (id,title,model,reasoning_effort) VALUES ('s','session','m','medium');`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := NewSQLiteStore(path, log)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accounts, err := store.Load()
	if err != nil || len(accounts) != 2 || !accounts[0].IsEnabled() {
		t.Fatalf("legacy accounts: %v", err)
	}
	no := false
	accounts[0].Enabled = &no
	if err := store.SaveAccount(accounts[0]); err != nil {
		t.Fatal(err)
	}
	// Credential imports that omit enabled must preserve the Dashboard setting.
	accounts[0].Enabled = nil
	if err := store.SaveAccount(accounts[0]); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKeyBindings("test-key", []string{"a", "b", "a"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKeyBindings("test-key", []string{"missing"}); !errors.Is(err, ErrInvalidAccountBinding) {
		t.Fatalf("invalid binding: %v", err)
	}
	if err := store.SetAPIKeyBindings("missing-key", []string{}); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	if err := store.SaveAPIKey(APIKeyItem{Key: "invalid-key", AccountIDs: []string{"missing"}}); !errors.Is(err, ErrInvalidAccountBinding) {
		t.Fatalf("invalid creation: %v", err)
	}
	if err := store.SaveChatSession(ChatSessionRecord{ID: "s", Title: "session", Model: "m", ReasoningEffort: "medium", AccountID: "b"}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	store, err = NewSQLiteStore(path, log)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accounts, err = store.Load()
	if err != nil || len(accounts) != 2 {
		t.Fatal("accounts lost on reopen", err)
	}
	for _, a := range accounts {
		if a.ID == "a" && a.IsEnabled() {
			t.Fatal("disabled state lost")
		}
	}
	keys, err := store.ListAPIKeys()
	if err != nil || len(keys) != 1 || !keys[0].AccountRestricted || len(keys[0].AccountIDs) != 2 {
		t.Fatalf("bindings lost: %v", err)
	}
	sessions, err := store.ListChatSessions()
	if err != nil || len(sessions) != 1 || sessions[0].AccountID != "b" {
		t.Fatalf("chat selection lost: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		if err := store.DeleteAccount(id); err != nil {
			t.Fatal(err)
		}
	}
	keys, err = store.ListAPIKeys()
	if err != nil || !keys[0].AccountRestricted || len(keys[0].AccountIDs) != 0 {
		t.Fatal("deleting last binding expanded permissions")
	}
	if err := store.SetAPIKeyBindings("test-key", []string{}); err != nil {
		t.Fatal(err)
	}
	keys, _ = store.ListAPIKeys()
	if keys[0].AccountRestricted {
		t.Fatal("explicit clear did not restore unrestricted scope")
	}
	if err := store.DeleteAPIKey("test-key"); err != nil {
		t.Fatal(err)
	}
	keys, _ = store.ListAPIKeys()
	if len(keys) != 0 {
		t.Fatal("deleted key retained")
	}
}
