package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
)

func TestPoolNextRefreshDelayUsesExpiryBeforeConfiguredInterval(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	expires := now.Add(5 * time.Second)
	p := testPool(t, "round_robin", config.AccountConfig{
		ID: "expiring", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: &expires,
	})
	defer p.Close()
	p.creds.RefreshSkew = time.Second

	delay := p.nextRefreshDelay(now, 10*time.Second)
	if delay < 3*time.Second || delay > 4100*time.Millisecond {
		t.Fatalf("next refresh delay = %s, want about 4s", delay)
	}
}

func TestRefreshSuccessShortLifetimeSchedulesBeforeExpiry(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	expires := now.Add(2 * time.Minute)
	p := testPool(t, "round_robin", config.AccountConfig{
		ID: "short", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: &expires,
	})
	defer p.Close()
	a := p.Get("short")
	c := a.Credential()
	a.noteRefreshSuccess(c, now, 10*time.Minute, 5*time.Minute)
	if delay := p.nextRefreshDelay(now, 10*time.Minute); delay != time.Minute {
		t.Fatalf("short-lived refreshed token waits %s, want 1m before expiry", delay)
	}

	stale := c.Clone()
	stale.ExpiresAt = now.Add(-time.Minute)
	a.StoreCredential(stale)
	a.noteRefreshSuccess(stale, now, 10*time.Minute, 5*time.Minute)
	if delay := p.nextRefreshDelay(now, 10*time.Minute); delay != 10*time.Minute {
		t.Fatalf("unchanged expired token not suppressed: %s", delay)
	}
}

func TestAccountRefreshBackoffResetsWhenCredentialIsReplaced(t *testing.T) {
	p := testPool(t, "round_robin", config.AccountConfig{ID: "a", AccessToken: "old", RefreshToken: "old-refresh"})
	defer p.Close()
	a := p.Get("a")
	old := a.Credential()
	now := time.Now()
	a.noteRefreshFailure(old, now, 10*time.Minute)
	first, ok := a.refreshRetryAt(old)
	if !ok || !first.Equal(now.Add(time.Minute)) {
		t.Fatalf("first retry = %v, want %v", first, now.Add(time.Minute))
	}
	a.noteRefreshFailure(old, now, 10*time.Minute)
	second, ok := a.refreshRetryAt(old)
	if !ok || !second.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("second retry = %v, want %v", second, now.Add(2*time.Minute))
	}

	replacement := old.Clone()
	replacement.AccessToken = "new"
	a.StoreCredential(replacement)
	if _, ok := a.refreshRetryAt(replacement); ok {
		t.Fatal("credential replacement retained old refresh backoff")
	}
}

func TestPoolRefreshAllDoesNotBlockHealthyAccount(t *testing.T) {
	blockedStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		if strings.Contains(cookie, "blocked") {
			close(blockedStarted)
			<-r.Context().Done()
			return
		}
		if !strings.Contains(cookie, "successful") {
			http.Error(w, "unexpected account", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access"})
	}))
	defer server.Close()
	expires := time.Now().Add(-time.Minute)
	cfg := config.Default()
	cfg.Upstream.BaseURL = server.URL
	cfg.Upstream.ForceHTTP2 = false
	cfg.Creds.SessionPath = "/session"
	cfg.Creds.Accounts = []config.AccountConfig{
		{ID: "blocked", SessionToken: "blocked", ExpiresAt: &expires},
		{ID: "successful", SessionToken: "successful", ExpiresAt: &expires},
	}
	p, err := NewPool(cfg, nopLog())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	r, err := creds.NewRefresher(p.creds, p.up, p.client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.refreshAll(ctx, r)
		close(done)
	}()
	select {
	case <-blockedStarted:
	case <-time.After(time.Second):
		t.Fatal("blocked refresh did not start")
	}
	deadline := time.Now().Add(time.Second)
	for p.Get("successful").Credential().AccessToken != "new-access" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := p.Get("successful").Credential().AccessToken; got != "new-access" {
		t.Fatalf("successful account was blocked by another refresh: %q", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refreshAll did not stop after context cancellation")
	}
}

func TestRefreshSchedulingSurvivesMetadataAndRejectsStaleCompletion(t *testing.T) {
	p := testPool(t, "round_robin", config.AccountConfig{ID: "a", AccessToken: "old", RefreshToken: "refresh"})
	defer p.Close()
	a := p.Get("a")
	now := time.Now()
	old := a.Credential()
	a.noteRefreshFailure(old, now, 10*time.Minute)
	metadata := old.Clone()
	metadata.Email = "edited@example.invalid"
	a.StoreCredential(metadata)
	if retry, ok := a.refreshRetryAt(metadata); !ok || !retry.Equal(now.Add(time.Minute)) {
		t.Fatal("metadata edit lost retry backoff")
	}
	replacement := metadata.Clone()
	replacement.AccessToken = "replacement"
	a.StoreCredential(replacement)
	a.noteRefreshSuccess(old, now, time.Minute, time.Second)
	if a.refreshBlocked(replacement, now) {
		t.Fatal("stale completion suppressed replacement")
	}
	expires := now.Add(20 * time.Second)
	replacement.ExpiresAt = expires
	a.noteRefreshSuccess(replacement, now, time.Hour, time.Second)
	p.creds.RefreshSkew = time.Second
	if delay := p.nextRefreshDelay(now, time.Hour); delay != 19*time.Second {
		t.Fatalf("healthy success delayed expiry: %s", delay)
	}
	a.MarkAuthFailed()
	if delay := p.nextRefreshDelay(now, time.Hour); delay != 10*time.Second {
		t.Fatalf("auth failure bypassed recent successful refresh: %s", delay)
	}
	if !a.refreshBlocked(replacement, now) || a.refreshBlocked(replacement, now.Add(10*time.Second)) {
		t.Fatal("successful refresh guard must expire before token expiry")
	}
}

func TestRefreshWithConcurrentSQLiteEdit(t *testing.T) {
	for _, edit := range []string{"metadata", "access", "refresh", "session", "cookie-session", "oauth-client", "delete"} {
		t.Run(edit, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 3600})
			}))
			defer server.Close()
			defer unblock()
			expires := time.Now().Add(-time.Minute)
			ac := config.AccountConfig{ID: "a", AccessToken: "old-access", RefreshToken: "old-refresh", SessionToken: "old-session", ExpiresAt: &expires,
				Cookies: "prism_oai_access_token=old-access; prism_oai_refresh_token=old-refresh; prism_session_token=old-session; device=old", Headers: map[string]string{"oauth_client_id": "old-client", "X-Meta": "old"}}
			s := openConfigTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
			if err := s.SaveAccount(ac); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.Upstream.BaseURL, cfg.Upstream.ForceHTTP2 = server.URL, false
			cfg.Creds.OAuthTokenURL, cfg.Creds.Accounts = server.URL+"/oauth", []config.AccountConfig{ac}
			p, err := NewPool(cfg, nopLog())
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			p.SetCredentialPersister(s.UpdateCredential)
			r, err := creds.NewRefresher(p.creds, p.up, p.client)
			if err != nil {
				t.Fatal(err)
			}
			old := p.Get("a")
			done := make(chan error, 1)
			go func() { _, err := p.RefreshAccount(context.Background(), r, old); done <- err }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("refresh did not start")
			}
			ac.Headers = map[string]string{"oauth_client_id": "old-client", "X-Meta": "edited"}
			switch edit {
			case "metadata":
				ac.Email, ac.AccountID, ac.Cookies = "edited@example.invalid", "edited-id", strings.Replace(ac.Cookies, "device=old", "device=edited", 1)
				editedExpiry := time.Now().Add(2 * time.Hour)
				ac.ExpiresAt = &editedExpiry
			case "access":
				ac.AccessToken = "replacement-access"
			case "refresh":
				ac.RefreshToken = "replacement-refresh"
			case "session":
				ac.SessionToken = "replacement-session"
			case "cookie-session":
				ac.SessionToken = ""
				ac.Cookies = strings.Replace(ac.Cookies, "prism_session_token=old-session", "prism_session_token=replacement-session", 1)
			case "oauth-client":
				ac.Headers["oauth_client_id"] = "replacement-client"
			}
			if edit == "delete" {
				err = s.DeleteAccount(ac.ID)
			} else {
				err = s.SaveAccount(ac)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Reload(s.Load); err != nil {
				t.Fatal(err)
			}
			before := old.Credential()
			unblock()
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("refresh did not finish")
			}
			if edit != "metadata" {
				if err == nil {
					t.Fatal("refresh accepted identity replacement/deletion")
				}
				if old.Credential() != before {
					t.Fatal("rejected refresh changed runtime credential")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			stored := creds.FromAccountConfig(loadConfigTestAccount(t, s))
			got := p.Get("a").Credential()
			if got.AccessToken != "rotated-access" || got.RefreshToken != "rotated-refresh" || got.Email != ac.Email || got.AccountID != ac.AccountID || !got.ExpiresAt.Equal(*ac.ExpiresAt) || !reflect.DeepEqual(got.Headers, ac.Headers) {
				t.Fatal("rotation lost metadata or new credentials")
			}
			if got.AccessToken != stored.AccessToken || got.RefreshToken != stored.RefreshToken || got.CookieHeader != stored.CookieHeader || !got.ExpiresAt.Equal(stored.ExpiresAt) || !reflect.DeepEqual(got.Headers, stored.Headers) {
				t.Fatal("runtime and SQLite diverged")
			}
			if creds.CookieValue(stored.CookieHeader, "device") != "edited" || creds.CookieValue(stored.CookieHeader, creds.CookiePrismAccessToken) != "rotated-access" || creds.CookieValue(stored.CookieHeader, creds.CookiePrismRefreshToken) != "rotated-refresh" || creds.CookieValue(stored.CookieHeader, creds.CookiePrismSessionToken) != "old-session" {
				t.Fatal("rotated cookies or edited device cookie lost")
			}
		})
	}
}

func TestPoolHealthResultDistinguishesAuthAndNetworkFailures(t *testing.T) {
	p := testPool(t, "round_robin", config.AccountConfig{ID: "a", AccessToken: "access", RefreshToken: "refresh"})
	defer p.Close()
	a := p.Get("a")
	p.MarkResult(a, errors.New("dial tcp: connection refused"), 0)
	if a.AuthFailed() {
		t.Fatal("network health failure marked credentials as invalid")
	}
	p.MarkResult(a, &creds.APIError{Op: "health", Status: http.StatusUnauthorized, Body: "expired"}, 0)
	if !a.AuthFailed() {
		t.Fatal("401 health result did not mark credentials as invalid")
	}
	if creds.IsAuthError(&creds.APIError{Op: "health", Status: http.StatusServiceUnavailable}) {
		t.Fatal("503 health result classified as authentication failure")
	}
}
