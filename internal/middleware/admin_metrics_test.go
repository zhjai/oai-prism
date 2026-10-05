package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminRouteLabelsExcludeCredentialsAndIDs(t *testing.T) {
	for _, path := range []string{"/admin/apikeys/synthetic-secret", "/admin/apikeys/synthetic-secret/bindings", "/admin/accounts/private-id/refresh", "/admin/chat/sessions/private-id/messages"} {
		label := routeLabel(path)
		if strings.Contains(label, "synthetic-secret") || strings.Contains(label, "private-id") {
			t.Fatalf("sensitive route label: %q", label)
		}
	}
}

func TestCORS_SupportedMutationsAndRoutingHeaders(t *testing.T) {
	handler := CORS("https://console.example")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("preflight reached handler") }))
	r := httptest.NewRequest(http.MethodOptions, "/admin/accounts/a", nil)
	r.Header.Set("Origin", "https://console.example")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	for _, method := range []string{"PUT", "PATCH", "DELETE"} {
		if !strings.Contains(w.Header().Get("Access-Control-Allow-Methods"), method) {
			t.Fatalf("missing method %s", method)
		}
	}
	for _, name := range []string{"x-api-key", "x-oaiprism-account", "anthropic-version"} {
		if !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), name) {
			t.Fatalf("missing header %s", name)
		}
	}
}
