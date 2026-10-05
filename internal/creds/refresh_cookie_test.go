package creds

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

func newCookieRefreshTestRefresher(t *testing.T, baseURL string) *Refresher {
	t.Helper()
	settings := config.Default()
	settings.Upstream.BaseURL = baseURL
	settings.Upstream.ForceHTTP2 = false
	settings.Creds.OAuthTokenURL = baseURL + "/oauth/token"
	refresher, err := NewRefresher(settings.Creds, settings.Upstream, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(refresher.client.CloseIdle)
	return refresher
}

func TestFetchSession_SynchronizesRotatedCookieTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.SetCookie(writer, &http.Cookie{Name: CookiePrismSessionToken, Value: "session-new"})
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/auth/session" {
			_, _ = writer.Write([]byte(`{}`))
			return
		}
		http.SetCookie(writer, &http.Cookie{Name: CookiePrismRefreshToken, Value: "refresh-new"})
		_ = json.NewEncoder(writer).Encode(map[string]string{"accessToken": "access-new"})
	}))
	defer upstream.Close()
	refresher := newCookieRefreshTestRefresher(t, upstream.URL)
	original := &Credential{
		AccessToken: "access-old", RefreshToken: "refresh-old", SessionToken: "session-old",
		SessionCookieName: CookiePrismSessionToken,
		CookieHeader:      "cf_clearance=synthetic; " + CookiePrismAccessToken + "=access-old; " + CookiePrismRefreshToken + "=refresh-old; " + CookiePrismSessionToken + "=session-old",
	}
	updated, err := refresher.FetchSession(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	for cookieName, expected := range map[string]string{
		CookiePrismAccessToken: "access-new", CookiePrismRefreshToken: "refresh-new",
		CookiePrismSessionToken: "session-new", "cf_clearance": "synthetic",
	} {
		if actual := CookieValue(updated.EffectiveCookie(), cookieName); actual != expected {
			t.Fatalf("cookie %s = %q, want %q", cookieName, actual, expected)
		}
	}
	if updated.SessionToken != "session-new" || updated.RefreshToken != "refresh-new" {
		t.Fatal("rotated cookie fields were not updated")
	}
	if original.AccessToken != "access-old" || CookieValue(original.CookieHeader, CookiePrismAccessToken) != "access-old" {
		t.Fatal("refresh mutated the original credential")
	}
}

func TestRefreshOAuth_SynchronizesCookiesWithoutSynthesizingHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"access_token": "access-new", "refresh_token": "refresh-new", "expires_in": 3600,
		})
	}))
	defer upstream.Close()
	refresher := newCookieRefreshTestRefresher(t, upstream.URL)
	for _, withHeader := range []bool{false, true} {
		original := &Credential{AccessToken: "access-old", RefreshToken: "refresh-old"}
		if withHeader {
			original.CookieHeader = "cf_clearance=synthetic; " + CookiePrismAccessToken + "=access-old; " + CookiePrismRefreshToken + "=refresh-old"
		}
		updated, err := refresher.RefreshOAuth(context.Background(), original)
		if err != nil {
			t.Fatal(err)
		}
		if !withHeader {
			if updated.CookieHeader != "" {
				t.Fatal("OAuth refresh synthesized a previously absent cookie header")
			}
			continue
		}
		if CookieValue(updated.CookieHeader, CookiePrismAccessToken) != updated.AccessToken || CookieValue(updated.CookieHeader, CookiePrismRefreshToken) != updated.RefreshToken {
			t.Fatal("cookie tokens disagree with refreshed OAuth fields")
		}
		if CookieValue(updated.CookieHeader, "cf_clearance") != "synthetic" || original.RefreshToken != "refresh-old" {
			t.Fatal("refresh discarded unrelated cookies or mutated its input")
		}
	}
}
