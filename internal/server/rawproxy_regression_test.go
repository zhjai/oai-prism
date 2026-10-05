package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

func TestRawProxyStripsGatewaySecretsAndDoesNotReplayMutation(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		for _, k := range []string{"X-Api-Key", "Api-Key", "X-Oaiprism-Account", "X-Oaiprism-Model", "X-Local-Workspace", "X-Test-Strip"} {
			if r.Header.Get(k) != "" {
				t.Errorf("forwarded protected header %s", k)
			}
		}
		if r.Header.Get("Authorization") != "Bearer upstream-synthetic" {
			t.Error("missing upstream credential")
		}
		w.Header().Set("Set-Cookie", "never-expose=synthetic")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Upstream.BaseURL = upstream.URL
	cfg.Upstream.MaxRetries = 3
	cfg.Creds.File = filepath.Join(t.TempDir(), "accounts.json")
	cfg.Creds.Accounts = []config.AccountConfig{{ID: "raw", AccessToken: "upstream-synthetic"}}
	cfg.RawProxy.AllowPaths = nil
	cfg.RawProxy.StripHeaders = []string{"X-Test-Strip"}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	code, _ := doLocal(t, http.MethodPost, ts.URL+"/prism/api/projects", `{"name":"synthetic"}`, map[string]string{"X-Api-Key": "local-gateway-secret", "Api-Key": "local-other-secret", "X-Oaiprism-Account": "raw", "X-Oaiprism-Model": "spoof", "X-Local-Workspace": "/tmp", "X-Test-Strip": "remove"})
	if code != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d", code, calls.Load())
	}
}
