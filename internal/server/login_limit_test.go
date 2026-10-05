package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

func TestAdminLoginBoundedAttempts(t *testing.T) {
	cfg := config.Default()
	cfg.Server.AdminPassword = "synthetic-password"
	s := &Server{cfg: cfg, admins: newAdminSessions()}
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest(http.MethodPost, "http://localhost/admin/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
		r.RemoteAddr = "192.0.2.4:54321"
		r.Header.Set("X-Forwarded-For", "198.51.100."+string(rune('1'+i)))
		w := httptest.NewRecorder()
		s.handleAdminLogin(w, r)
		want := http.StatusUnauthorized
		if i == 5 {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("attempt %d: %d, want %d", i, w.Code, want)
		}
	}
	now := time.Now()
	if !s.admins.allowLogin("different-peer", now) {
		t.Fatal("independent peer blocked")
	}
	if !s.admins.allowLogin("192.0.2.4", now.Add(11*time.Second)) {
		t.Fatal("rate limit did not recover")
	}
}
