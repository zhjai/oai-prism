package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
)

func newOAuthTestStore() *oauthSessionStore {
	return &oauthSessionStore{byState: make(map[string]*oauthSession), byID: make(map[string]*oauthSession)}
}

func TestOAuthSessionMultiplePendingAndTerminalRetention(t *testing.T) {
	store := newOAuthTestStore()
	first := &oauthSession{ID: "first", State: "first-state", ClientID: "client", CreatedAt: time.Now()}
	second := &oauthSession{ID: "second", State: "second-state", ClientID: "client", CreatedAt: time.Now()}
	store.put(first)
	store.put(second)
	if store.getByID(first.ID) == nil || store.getByState(second.State) == nil {
		t.Fatal("same-client pending sessions replaced each other")
	}
	claimed, err := store.claim(first)
	if err != nil {
		t.Fatal(err)
	}
	store.finish(claimed, "account", nil)
	status := store.getByID(first.ID)
	if status == nil || !status.done || status.account != "account" || store.getByState(first.State) == nil {
		t.Fatal("successful status was not retained")
	}
	status.account = "mutated-snapshot"
	if got := store.getByID(first.ID); got.account != "account" {
		t.Fatal("polling exposed mutable session state")
	}
	if _, err := store.claim(first); err == nil {
		t.Fatal("completed authorization was claimed again")
	}
	claimed, err = store.claim(second)
	if err != nil {
		t.Fatal(err)
	}
	store.finish(claimed, "", fmt.Errorf("synthetic failure"))
	if got := store.getByID(second.ID); got == nil || !got.done || got.errMsg != "synthetic failure" {
		t.Fatal("failed status was not retained")
	}
	store.put(&oauthSession{ID: "expired", State: "expired-state", CreatedAt: time.Now().Add(-oauthSessionTTL - time.Second)})
	if store.getByID("expired") != nil || store.getByState("expired-state") != nil {
		t.Fatal("expired status remained accessible")
	}
}

func TestOAuthSessionConcurrentClaimAndPolling(t *testing.T) {
	store := newOAuthTestStore()
	sess := &oauthSession{ID: "session", State: "state", CreatedAt: time.Now()}
	store.put(sess)
	var claims atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				copy := store.getByID(sess.ID)
				if copy == nil {
					t.Error("active session disappeared")
					return
				}
				_ = copy.done
				if claimed, err := store.claim(copy); err == nil {
					claims.Add(1)
					store.finish(claimed, "account", nil)
				}
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("exchange claims=%d", claims.Load())
	}
}

func TestOAuthStatusRetainsTerminalResults(t *testing.T) {
	original := oauthSessions
	oauthSessions = newOAuthTestStore()
	t.Cleanup(func() { oauthSessions = original })
	sess := &oauthSession{ID: "status-session", State: "status-state", CreatedAt: time.Now()}
	oauthSessions.put(sess)
	claimed, err := oauthSessions.claim(sess)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	server.finishOAuthSession(claimed, "account", nil)
	for _, query := range []string{"session_id=status-session", "state=status-state"} {
		response := httptest.NewRecorder()
		server.handleOAuthStatus(response, httptest.NewRequest(http.MethodGet, "/admin/oauth/status?"+query, nil))
		var result map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || result["status"] != "success" || result["account_id"] != "account" {
			t.Fatalf("terminal poll failed: %d %s", response.Code, response.Body.String())
		}
	}
}

func TestOAuthMissingDatabaseReportsFailureAndPreventsRetry(t *testing.T) {
	original := oauthSessions
	oauthSessions = newOAuthTestStore()
	t.Cleanup(func() { oauthSessions = original })
	sess := &oauthSession{ID: "session", State: "state", CreatedAt: time.Now()}
	oauthSessions.put(sess)
	server := &Server{}
	if id, err := server.completeOAuthLogin(context.Background(), sess, "synthetic-code"); err == nil || id != "" {
		t.Fatal("missing database reported OAuth success")
	}
	if got := oauthSessions.getByID(sess.ID); got == nil || !got.done || got.errMsg == "" {
		t.Fatal("missing database failure was not retained")
	}
	if _, err := oauthSessions.claim(sess); err == nil {
		t.Fatal("failed exchange can be replayed")
	}
}

func TestOAuthResultHTMLEscapesUntrustedValues(t *testing.T) {
	result := oauthResultHTML("<script>title</script>", "<img src=x onerror=alert(1)> & \"detail\"")
	if strings.Contains(result, "<script>") || strings.Contains(result, "<img ") || !strings.Contains(result, "&lt;script&gt;") || !strings.Contains(result, "&amp;") {
		t.Fatalf("untrusted callback values not escaped: %s", result)
	}
}

func newOAuthHTTPTestServer(t *testing.T, tokenURL, proxyURL string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream.BaseURL = "http://upstream.invalid"
	cfg.Upstream.HTTPProxy = proxyURL
	cfg.Creds.OAuthTokenURL = tokenURL
	cfg.Creds.Accounts = nil
	pool, err := account.NewPool(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Server{cfg: cfg, pool: pool, log: slog.Default()}
}

func TestOAuthBrowserClientSelection(t *testing.T) {
	for _, tc := range []struct{ name, requested, configured, redirect, want string }{
		{"empty", "", "", "http://localhost:1455/auth/callback", oauthBrowserClientID},
		{"prism", "", config.DefaultOAuthClientID, "http://localhost:1455/auth/callback", oauthBrowserClientID},
		{"ipv4", "", "", "http://127.0.0.1:1455/auth/callback", oauthBrowserClientID},
		{"ipv6", "", "", "http://[::1]:1455/auth/callback", oauthBrowserClientID},
		{"custom", "", " custom-client ", "http://localhost:1455/auth/callback", "custom-client"},
		{"explicit", " explicit-client ", "custom-client", "http://localhost:1455/auth/callback", "explicit-client"},
		{"explicit-prism", config.DefaultOAuthClientID, "", "http://localhost:1455/auth/callback", config.DefaultOAuthClientID},
		{"remote", "", "", "https://gateway.invalid/callback", config.DefaultOAuthClientID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := oauthAuthorizeClientID(tc.requested, tc.configured, tc.redirect); got != tc.want {
				t.Fatalf("client=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestOAuthTokenExchangeUsesConfiguredProxyAndURL(t *testing.T) {
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.String() != "http://token.invalid/custom/token" || r.Method != http.MethodPost {
			t.Errorf("unexpected token request: %s %s", r.Method, r.URL)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["client_id"] != "selected-client" || payload["code"] != "code" || payload["code_verifier"] != "verifier" || payload["redirect_uri"] != "http://localhost/callback" || payload["grant_type"] != "authorization_code" {
			t.Error("token request lost OAuth parameters")
		}
		_, _ = io.WriteString(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600}`)
	}))
	defer proxy.Close()
	server := newOAuthHTTPTestServer(t, "http://token.invalid/custom/token", proxy.URL)
	token, err := server.oauthExchangeToken(context.Background(), "selected-client", "code", "verifier", "http://localhost/callback")
	if err != nil || token == nil || token.AccessToken != "synthetic-access" || requests.Load() != 1 {
		t.Fatalf("configured proxy exchange failed: token=%v requests=%d err=%v", token, requests.Load(), err)
	}
}

type oauthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f oauthRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOAuthTokenExchangeCancellationAndSanitizedErrors(t *testing.T) {
	server := newOAuthHTTPTestServer(t, "http://token.invalid/token", "")
	started := make(chan struct{})
	server.pool.Client().HTTP.Transport = oauthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := server.oauthExchangeToken(ctx, "client", "code", "verifier", "http://localhost/callback")
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("exchange did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("request cancellation lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("exchange ignored request cancellation")
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
	}{
		{"http-status", 401, `{"access_token":"SENSITIVE"}`, nil},
		{"oauth-error", 200, `{"error":"SENSITIVE","error_description":"SENSITIVE"}`, nil},
		{"malformed", 200, `{"expires_in":"SENSITIVE"}`, nil},
		{"transport", 0, "", errors.New("SENSITIVE")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server.pool.Client().HTTP.Transport = oauthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})
			_, err := server.oauthExchangeToken(context.Background(), "client", "code", "verifier", "http://localhost/callback")
			if err == nil || strings.Contains(err.Error(), "SENSITIVE") {
				t.Fatalf("unsafe exchange error: %v", err)
			}
		})
	}
}

func TestOAuthLoginPersistsJWTMetadataAndSelectedClient(t *testing.T) {
	jwt := func(claims string) string {
		return "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".synthetic"
	}
	for _, tc := range []struct{ name, access, id, email, plan, accountID string }{
		{"access", jwt(`{"email":"access@example.invalid","https://api.openai.com/auth":{"chatgpt_plan_type":"plus","chatgpt_account_id":"access-account"}}`), jwt(`{"email":"id@example.invalid"}`), "access@example.invalid", "plus", "access-account"},
		{"id-fallback", "opaque-access", jwt(`{"email":"id@example.invalid","https://api.openai.com/auth":{"chatgpt_plan_type":"team","chatgpt_account_id":"id-account"}}`), "id@example.invalid", "team", "id-account"},
		{"blank-plan", "opaque-access", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := oauthSessions
			oauthSessions = newOAuthTestStore()
			t.Cleanup(func() { oauthSessions = original })
			var exchanges atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tc.access, "id_token": tc.id, "refresh_token": "synthetic-refresh", "expires_in": 3600})
			}))
			defer endpoint.Close()
			server := newOAuthHTTPTestServer(t, endpoint.URL, "")
			store, err := account.NewSQLiteStore(filepath.Join(t.TempDir(), "accounts.db"), slog.Default())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			server.sqlite = store
			sess := &oauthSession{ID: "session", State: "state", ClientID: "selected-client", CreatedAt: time.Now()}
			oauthSessions.put(sess)
			id, err := server.completeOAuthLogin(context.Background(), sess, "synthetic-code")
			if err != nil {
				t.Fatal(err)
			}
			accounts, err := store.Load()
			if err != nil || len(accounts) != 1 {
				t.Fatalf("saved accounts=%d err=%v", len(accounts), err)
			}
			got := accounts[0]
			if got.ID != id || got.Email != tc.email || got.Plan != tc.plan || got.AccountID != tc.accountID || got.Headers["oauth_client_id"] != "selected-client" || got.ExpiresAt == nil {
				t.Fatalf("OAuth metadata not retained: email=%s plan=%s account=%s", got.Email, got.Plan, got.AccountID)
			}
			if _, err := server.completeOAuthLogin(context.Background(), sess, "synthetic-code"); err == nil || exchanges.Load() != 1 {
				t.Fatalf("authorization code exchanged twice: exchanges=%d err=%v", exchanges.Load(), err)
			}
		})
	}
}

func TestOAuthRandomFailuresNeverCreateSessionOrAccount(t *testing.T) {
	originalReader, originalSessions := rand.Reader, oauthSessions
	t.Cleanup(func() { rand.Reader, oauthSessions = originalReader, originalSessions })
	for _, available := range []int{0, 64, 80} {
		rand.Reader = bytes.NewReader(make([]byte, available))
		oauthSessions = newOAuthTestStore()
		response := httptest.NewRecorder()
		(&Server{}).handleOAuthBegin(response, httptest.NewRequest(http.MethodPost, "/admin/oauth/begin", strings.NewReader(`{}`)))
		if response.Code != http.StatusInternalServerError || len(oauthSessions.byID) != 0 {
			t.Fatalf("entropy failure at byte %d created session: status=%d", available, response.Code)
		}
	}
	store, err := account.NewSQLiteStore(filepath.Join(t.TempDir(), "accounts.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rand.Reader = bytes.NewReader(nil)
	sess := &oauthSession{ID: "session", State: "state", CreatedAt: time.Now()}
	oauthSessions.put(sess)
	if id, err := (&Server{sqlite: store}).completeOAuthLogin(context.Background(), sess, "code"); err == nil || id != "" {
		t.Fatal("entropy failure created account ID")
	}
	if got := oauthSessions.getByID(sess.ID); got == nil || !got.done || got.errMsg == "" {
		t.Fatal("entropy failure was not retained")
	}
}
