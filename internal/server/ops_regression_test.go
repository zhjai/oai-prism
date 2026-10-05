package server

import (
	"github.com/oai-prism/oaiprism/internal/config"
	"net/http"
	"testing"
)

func TestReadinessRejectsAllDisabledAccounts(t *testing.T) {
	disabled := false
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, []config.AccountConfig{{ID: "disabled", AccessToken: "synthetic", Enabled: &disabled}}, nil)
	if code, out := doLocal(t, http.MethodGet, ts.URL+"/readyz", "", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("readiness %d %s", code, out)
	}
	if code, out := doLocal(t, http.MethodGet, ts.URL+"/healthz", "", nil); code != http.StatusOK {
		t.Fatalf("liveness %d %s", code, out)
	}
}

func TestMetricsRequiresGatewayAuthentication(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, nil, func(c *config.Config) { c.Facade.APIKeys = []string{"synthetic-metrics-key"} })
	if code, _ := doLocal(t, http.MethodGet, ts.URL+"/metrics", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous metrics: %d", code)
	}
	if code, out := doLocal(t, http.MethodGet, ts.URL+"/metrics", "", map[string]string{"Authorization": "Bearer synthetic-metrics-key"}); code != http.StatusOK {
		t.Fatalf("authenticated metrics %d %s", code, out)
	}
}
