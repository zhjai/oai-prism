package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
)

func TestAdmin_DeleteSurvivesReloadAndRestart(t *testing.T) {
	for _, static := range []bool{false, true} {
		name := "file"
		if static {
			name = "static"
		}
		t.Run(name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Upstream.BaseURL = "http://127.0.0.1:1"
			cfg.Creds.File = filepath.Join(t.TempDir(), "accounts.json")
			cfg.Creds.AutoRefresh = false
			cfg.Pool.HealthCheck = false
			accounts := []config.AccountConfig{
				{ID: "deleted", AccessToken: "test-deleted"},
				{ID: "kept", AccessToken: "test-kept"},
			}
			if static {
				cfg.Creds.Accounts = accounts
			} else {
				raw, _ := json.Marshal(map[string]any{"accounts": accounts})
				if err := os.WriteFile(cfg.Creds.File, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			start := func() (*Server, *httptest.Server) {
				s, err := New(cfg, log)
				if err != nil {
					t.Fatal(err)
				}
				ts := httptest.NewServer(s.Handler())
				t.Cleanup(func() { ts.Close(); _ = s.Close() })
				return s, ts
			}
			s, ts := start()
			assertIDs := func(ids ...string) {
				t.Helper()
				stored, err := s.sqlite.Load()
				if err != nil || len(stored) != len(ids) || s.pool.Size() != len(ids) {
					t.Fatalf("account count: stored=%d pool=%d want=%d err=%v", len(stored), s.pool.Size(), len(ids), err)
				}
				for _, id := range ids {
					if s.pool.Get(id) == nil {
						t.Fatalf("missing account %s", id)
					}
				}
			}
			assertIDs("deleted", "kept")
			key := account.APIKeyItem{Key: "test-restricted", Name: "restricted", AccountIDs: []string{"deleted"}}
			if err := s.sqlite.SaveAPIKey(key); err != nil {
				t.Fatal(err)
			}
			if code, out := doLocal(t, http.MethodDelete, ts.URL+"/admin/accounts/deleted", "", map[string]string{"Authorization": "Bearer " + key.Key}); code != http.StatusOK {
				t.Fatalf("delete: %d %s", code, out)
			}
			assertIDs("kept")
			if !static {
				file, err := s.store.Load()
				if err != nil || len(file) != 1 || file[0].ID != "kept" {
					t.Fatalf("file deletion not persisted: count=%d err=%v", len(file), err)
				}
			}
			// Simulate a watcher callback holding a snapshot taken before deletion.
			if err := s.syncPoolFromFile(accounts); err != nil {
				t.Fatal(err)
			}
			assertIDs("kept")
			// A restricted key whose last account was deleted must stay restricted.
			scope, ok := s.keys.GetScopes()[key.Key]
			if !ok || !scope.Restricted || len(scope.AccountIDs) != 0 {
				t.Fatalf("deletion widened key scope: %+v", scope)
			}
			headers := map[string]string{"Authorization": "Bearer " + key.Key}
			if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/reload", "", headers); code != http.StatusOK {
				t.Fatalf("reload: %d %s", code, out)
			}
			assertIDs("kept")
			ts.Close()
			_ = s.Close()
			s, ts = start()
			assertIDs("kept")
			// Deleting the last account must not trigger startup migration again.
			if code, out := doLocal(t, http.MethodDelete, ts.URL+"/admin/accounts/kept", "", headers); code != http.StatusOK {
				t.Fatalf("delete last: %d %s", code, out)
			}
			assertIDs()
			ts.Close()
			_ = s.Close()
			s, ts = start()
			assertIDs()
			// An explicit Dashboard import can add the same ID back.
			if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts", `{"id":"deleted","access_token":"new-token"}`, headers); code != http.StatusCreated {
				t.Fatalf("re-add: %d %s", code, out)
			}
			assertIDs("deleted")
		})
	}
}

func TestAdmin_DeleteFileFailureKeepsAccount(t *testing.T) {
	cfg := config.Default()
	cfg.Upstream.BaseURL = "http://127.0.0.1:1"
	cfg.Creds.File = filepath.Join(t.TempDir(), "accounts.json")
	cfg.Creds.Accounts = []config.AccountConfig{{ID: "kept", AccessToken: "test"}}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	// A directory at the file path reliably fails, even when tests run as root.
	if err := os.Mkdir(cfg.Creds.File, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, _ := doLocal(t, http.MethodDelete, ts.URL+"/admin/accounts/kept", "", nil); code != http.StatusInternalServerError {
		t.Fatalf("file write/read failure status=%d want=500", code)
	}
	stored, err := s.sqlite.Load()
	if err != nil || len(stored) != 1 || s.pool.Get("kept") == nil {
		t.Fatal("failed file deletion removed the database/runtime account")
	}
}

func TestAdmin_DeleteConcurrentWithReload(t *testing.T) {
	cfg := config.Default()
	cfg.Upstream.BaseURL = "http://127.0.0.1:1"
	cfg.Creds.File = filepath.Join(t.TempDir(), "accounts.json")
	cfg.Creds.Accounts = []config.AccountConfig{{ID: "deleted", AccessToken: "test"}}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.syncPoolFromFile(cfg.Creds.Accounts); err != nil {
				t.Error(err)
			}
		}()
	}
	code, out := doLocal(t, http.MethodDelete, ts.URL+"/admin/accounts/deleted", "", nil)
	wg.Wait()
	if code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, out)
	}
	stored, err := s.sqlite.Load()
	if err != nil || len(stored) != 0 || s.pool.Size() != 0 {
		t.Fatalf("concurrent reload restored deletion: stored=%d pool=%d err=%v", len(stored), s.pool.Size(), err)
	}
}
