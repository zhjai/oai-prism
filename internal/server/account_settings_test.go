package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
)

func TestAdmin_AccountSettingsPersistAndApply(t *testing.T) {
	cfg := config.Default()
	cfg.Upstream.BaseURL = "http://127.0.0.1:1"
	cfg.Creds.File = filepath.Join(t.TempDir(), "accounts.json")
	cfg.Creds.AutoRefresh = false
	cfg.Pool.HealthCheck = false
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := func() (*Server, *httptest.Server) {
		s, err := New(cfg, log)
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(func() {
			ts.Close()
			_ = s.Close()
		})
		return s, ts
	}
	s, ts := start()
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"prolite"}}`))
	token := "e30." + claims + ".test"
	body, _ := json.Marshal(map[string]any{
		"id": "editable", "name": "account", "plan": "prolite",
		"access_token": token, "refresh_token": "test-refresh",
		"cookies": "prism_session_token=test-session", "max_concurrency": 4,
		"tags": []string{"keep"},
	})
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts", string(body), nil); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, out)
	}
	assertSettings := func(plan string, max int64) {
		t.Helper()
		code, out := doLocal(t, http.MethodGet, ts.URL+"/admin/accounts", "", nil)
		var response struct {
			Accounts []account.Stats `json:"accounts"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(out), &response) != nil || len(response.Accounts) != 1 {
			t.Fatalf("account missing from running pool: %d %s", code, out)
		}
		got := response.Accounts[0]
		if got.Plan != plan || got.MaxConcur != max {
			t.Fatalf("runtime settings = (%q, %d), want (%q, %d)", got.Plan, got.MaxConcur, plan, max)
		}
		stored, err := s.sqlite.Load()
		if err != nil || len(stored) != 1 {
			t.Fatalf("stored account missing: %v", err)
		}
		if stored[0].Plan != plan || int64(stored[0].MaxConcurrency) != max {
			t.Fatalf("stored settings = (%q, %d), want (%q, %d)", stored[0].Plan, stored[0].MaxConcurrency, plan, max)
		}
		if stored[0].AccessToken != token || stored[0].RefreshToken != "test-refresh" || stored[0].Cookies != "prism_session_token=test-session" || len(stored[0].Tags) != 1 || stored[0].Tags[0] != "keep" {
			t.Fatal("editing settings lost credentials or tags")
		}
	}
	assertSettings("prolite", 4)
	if code, out := doLocal(t, http.MethodPut, ts.URL+"/admin/accounts/editable", `{"plan":"enterprise","max_concurrency":7}`, nil); code != http.StatusOK {
		t.Fatalf("update: %d %s", code, out)
	}
	assertSettings("enterprise", 7)
	a := s.pool.Get("editable")
	for i := 0; i < 7; i++ {
		if !a.Acquire(time.Now()) {
			t.Fatalf("slot %d rejected before configured limit", i)
		}
	}
	if a.Acquire(time.Now()) {
		t.Fatal("configured concurrency limit was not enforced")
	}
	for i := 0; i < 7; i++ {
		a.Release()
	}
	// Upstream claims remain distinct from the local plan label, including after refresh.
	c := a.Credential().Clone()
	c.Plan = "free"
	a.StoreCredential(c)
	assertSettings("enterprise", 7)
	if code, out := doLocal(t, http.MethodPut, ts.URL+"/admin/accounts/editable", `{"name":"renamed"}`, nil); code != http.StatusOK {
		t.Fatalf("partial update: %d %s", code, out)
	}
	assertSettings("enterprise", 7)
	// A manual reload must read Dashboard's SQLite settings rather than an old credential file.
	if err := s.store.Persist([]config.AccountConfig{{ID: "editable", AccessToken: token, Plan: "free", MaxConcurrency: 2}}); err != nil {
		t.Fatal(err)
	}
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/reload", "", nil); code != http.StatusOK {
		t.Fatalf("reload: %d %s", code, out)
	}
	assertSettings("enterprise", 7)
	if code, out := doLocal(t, http.MethodPut, ts.URL+"/admin/accounts/editable", `{"max_concurrency":0}`, nil); code != http.StatusOK {
		t.Fatalf("unlimited update: %d %s", code, out)
	}
	assertSettings("enterprise", 0)
	ts.Close()
	_ = s.Close()
	s, ts = start()
	assertSettings("enterprise", 0)
	a = s.pool.Get("editable")
	for i := 0; i < 12; i++ {
		if !a.Acquire(time.Now()) {
			t.Fatalf("unlimited concurrency rejected slot %d", i)
		}
	}
	for i := 0; i < 12; i++ {
		a.Release()
	}
	if code, _ := doLocal(t, http.MethodPut, ts.URL+"/admin/accounts/editable", `{"max_concurrency":-1}`, nil); code != http.StatusBadRequest {
		t.Fatalf("negative concurrency status = %d, want 400", code)
	}
	assertSettings("enterprise", 0)
	if code, _ := doLocal(t, http.MethodPut, ts.URL+"/admin/accounts/missing", `{"name":"missing"}`, nil); code != http.StatusNotFound {
		t.Fatalf("missing account update status = %d, want 404", code)
	}
}

func TestAdmin_AccountCreationConcurrencyDefaults(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, nil, nil)
	for _, tc := range []struct {
		name string
		body string
		max  int64
	}{
		{"omitted", `{"id":"omitted","access_token":"test"}`, 2},
		{"unlimited", `{"id":"unlimited","access_token":"test","max_concurrency":0}`, 0},
		{"batch", `[{"id":"batch","access_token":"test","max_concurrency":5}]`, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts", tc.body, nil); code != http.StatusCreated {
				t.Fatalf("create: %d %s", code, out)
			}
			code, out := doLocal(t, http.MethodGet, ts.URL+"/admin/accounts", "", nil)
			var response struct {
				Accounts []account.Stats `json:"accounts"`
			}
			if code != http.StatusOK || json.Unmarshal([]byte(out), &response) != nil {
				t.Fatalf("read: %d %s", code, out)
			}
			for _, a := range response.Accounts {
				if a.ID == tc.name {
					if !a.HasToken {
						t.Fatal("snake_case access_token was not imported")
					}
					if a.MaxConcur != tc.max {
						t.Fatalf("concurrency = %d, want %d", a.MaxConcur, tc.max)
					}
					return
				}
			}
			t.Fatal("imported access_token account missing from running pool")
		})
	}
}

func TestAdmin_ReloadFileCredentialsKeepsSettings(t *testing.T) {
	cfg := config.Default()
	cfg.Upstream.BaseURL = "http://127.0.0.1:1"
	cfg.Creds.File = filepath.Join(t.TempDir(), "accounts.json")
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts", `{"id":"existing","name":"edited","plan":"enterprise","max_concurrency":9,"access_token":"old"}`, nil); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, out)
	}
	if err := s.store.Persist([]config.AccountConfig{
		{ID: "existing", Name: "old-name", Plan: "free", MaxConcurrency: 2, AccessToken: "renewed"},
		{ID: "imported", Plan: "team", MaxConcurrency: 3, AccessToken: "new"},
	}); err != nil {
		t.Fatal(err)
	}
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/reload", "", nil); code != http.StatusOK {
		t.Fatalf("reload: %d %s", code, out)
	}
	existing := s.pool.Get("existing")
	if existing == nil || existing.Credential().AccessToken != "renewed" {
		t.Fatal("updated file credential was not loaded")
	}
	stats := existing.Stats(time.Now())
	if stats.Name != "edited" || stats.Plan != "enterprise" || stats.MaxConcur != 9 {
		t.Fatalf("file reload overwrote edited settings: %+v", stats)
	}
	imported := s.pool.Get("imported")
	if imported == nil || imported.Credential().AccessToken != "new" || imported.MaxConcurrency() != 3 {
		t.Fatal("new file account was not imported")
	}
}
