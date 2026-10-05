package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
)

func accountRefreshConfig(t *testing.T, handler http.HandlerFunc) *config.Config {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	cfg := config.Default()
	cfg.Upstream.BaseURL = upstream.URL
	cfg.Creds.OAuthTokenURL = upstream.URL + "/oauth/token"
	cfg.Creds.AutoRefresh = false
	cfg.Pool.HealthCheck = false
	cfg.Creds.File = filepath.Join(t.TempDir(), "accounts.json")
	store := account.NewStore(cfg.Creds.File, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := store.Persist([]config.AccountConfig{{
		ID: "refresh", Name: "Original", AccessToken: "synthetic-old-access",
		RefreshToken: "synthetic-old-refresh", MaxConcurrency: 2,
	}}); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func startAccountRefreshServer(t *testing.T, cfg *config.Config) (*Server, *httptest.Server) {
	t.Helper()
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); _ = s.Close() })
	return s, ts
}

func writeAccountRefreshResponse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "synthetic-new-access", "refresh_token": "synthetic-new-refresh", "expires_in": 3600,
	})
}

func assertAccountRefreshPersisted(t *testing.T, s *Server, name string, maxConcurrency int64) {
	t.Helper()
	list, err := s.sqlite.Load()
	if err != nil || len(list) != 1 {
		t.Fatalf("stored inventory: count=%d err=%v", len(list), err)
	}
	stored := list[0]
	if stored.AccessToken != "synthetic-new-access" || stored.RefreshToken != "synthetic-new-refresh" || stored.ExpiresAt == nil || !stored.ExpiresAt.After(time.Now()) {
		t.Fatal("refreshed credential was not persisted")
	}
	if stored.Name != name || int64(stored.MaxConcurrency) != maxConcurrency {
		t.Fatalf("stored settings: name=%q concurrency=%d", stored.Name, stored.MaxConcurrency)
	}
	a := s.pool.Get("refresh")
	if a == nil || a.Credential().AccessToken != stored.AccessToken || a.Credential().RefreshToken != stored.RefreshToken || !a.Credential().ExpiresAt.Equal(*stored.ExpiresAt) {
		t.Fatal("runtime credential differs from persisted credential")
	}
	if a.Name != name || a.MaxConcurrency() != maxConcurrency {
		t.Fatalf("runtime settings: name=%q concurrency=%d", a.Name, a.MaxConcurrency())
	}
}

func TestAccountRefreshPersistsThroughSettingsReloadAndRestart(t *testing.T) {
	cfg := accountRefreshConfig(t, writeAccountRefreshResponse)
	s, ts := startAccountRefreshServer(t, cfg)
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts/refresh/refresh", "", nil); code != http.StatusOK {
		t.Fatalf("refresh: %d %s", code, out)
	}
	assertAccountRefreshPersisted(t, s, "Original", 2)
	if code, out := doLocal(t, http.MethodPut, ts.URL+"/admin/accounts/refresh", `{"name":"Dashboard","max_concurrency":7}`, nil); code != http.StatusOK {
		t.Fatalf("settings edit: %d %s", code, out)
	}
	assertAccountRefreshPersisted(t, s, "Dashboard", 7)
	file, err := s.store.Load()
	if err != nil || len(file) != 1 || file[0].AccessToken != "synthetic-old-access" {
		t.Fatalf("test requires unchanged stale JSON credentials: count=%d err=%v", len(file), err)
	}
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/reload", "", nil); code != http.StatusOK {
		t.Fatalf("reload: %d %s", code, out)
	}
	assertAccountRefreshPersisted(t, s, "Dashboard", 7)
	ts.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ = startAccountRefreshServer(t, cfg)
	assertAccountRefreshPersisted(t, s, "Dashboard", 7)
}

func TestAccountRefreshInFlightSettingsRetainOldLease(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	cfg := accountRefreshConfig(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		writeAccountRefreshResponse(w, r)
	})
	s, ts := startAccountRefreshServer(t, cfg)
	t.Cleanup(unblock)
	lease, err := s.pool.Acquire(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/admin/accounts/refresh/refresh", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Host = "127.0.0.1"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		done <- rec
	}()
	select {
	case <-entered:
	case rec := <-done:
		t.Fatalf("refresh returned before reaching upstream: %d %s", rec.Code, rec.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach mock upstream")
	}
	if code, out := doLocal(t, http.MethodPut, ts.URL+"/admin/accounts/refresh", `{"name":"During refresh","max_concurrency":3}`, nil); code != http.StatusOK {
		t.Fatalf("settings edit: %d %s", code, out)
	}
	current := s.pool.Get("refresh")
	if current == lease.Account || current.Inflight() != 1 || lease.Account.MaxConcurrency() != 3 {
		t.Fatal("configuration rebuild lost the active lease or shared scheduling state")
	}
	unblock()
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not complete after settings edit")
	}
	assertAccountRefreshPersisted(t, s, "During refresh", 3)
	if lease.Account.Credential().AccessToken != "synthetic-new-access" {
		t.Fatal("old lease did not observe refreshed shared credential")
	}
	lease.Release()
	if current.Inflight() != 0 {
		t.Fatal("releasing old lease did not release current account slot")
	}
}

func TestAccountRefreshCompletionDoesNotResurrectDeletedAccount(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	cfg := accountRefreshConfig(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		writeAccountRefreshResponse(w, r)
	})
	s, ts := startAccountRefreshServer(t, cfg)
	t.Cleanup(unblock)
	old := s.pool.Get("refresh")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/admin/accounts/refresh/refresh", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Host = "127.0.0.1"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		done <- rec
	}()
	select {
	case <-entered:
	case rec := <-done:
		t.Fatalf("refresh returned before reaching upstream: %d %s", rec.Code, rec.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach mock upstream")
	}
	if code, out := doLocal(t, http.MethodDelete, ts.URL+"/admin/accounts/refresh", "", nil); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, out)
	}
	unblock()
	select {
	case rec := <-done:
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("stale refresh completion: status=%d want=502", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not complete after deletion")
	}
	if old.Available(time.Now()) || old.Credential().AccessToken != "synthetic-old-access" {
		t.Fatal("deleted runtime snapshot remained schedulable or accepted refresh")
	}
	assertDeleted := func() {
		t.Helper()
		list, err := s.sqlite.Load()
		if err != nil || len(list) != 0 || s.pool.Size() != 0 {
			t.Fatalf("deleted account resurrected: stored=%d pool=%d err=%v", len(list), s.pool.Size(), err)
		}
	}
	assertDeleted()
	if err := s.syncPoolFromFile([]config.AccountConfig{{ID: "refresh", AccessToken: "synthetic-old-access", RefreshToken: "synthetic-old-refresh"}}); err != nil {
		t.Fatal(err)
	}
	assertDeleted()
	ts.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ = startAccountRefreshServer(t, cfg)
	assertDeleted()
}
