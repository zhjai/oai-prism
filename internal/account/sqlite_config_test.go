package account

import (
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
)

func openConfigTestStore(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(path, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func loadConfigTestAccount(t *testing.T, s *SQLiteStore) config.AccountConfig {
	t.Helper()
	list, err := s.Load()
	if err != nil || len(list) != 1 {
		t.Fatalf("Load: count=%d err=%v", len(list), err)
	}
	return list[0]
}

func TestSQLiteAccountConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	s := openConfigTestStore(t, path)
	enabled := false
	expires := time.Date(2030, 5, 4, 3, 2, 1, 123456789, time.UTC)
	want := config.AccountConfig{
		ID: "complete", Name: "Full account", Enabled: &enabled,
		Cookies: "other=value", CookieMap: map[string]string{"device": "test-device"},
		SessionToken: "session-test", AccessToken: "access-test", RefreshToken: "refresh-test",
		ExpiresAt: &expires, AccountID: "upstream-id", Email: "test@example.invalid", Plan: "plus",
		Proxy: "http://127.0.0.1:3128", MaxConcurrency: 0, RatePerSecond: 1.25, RateBurst: 7, Weight: 11,
		Headers: map[string]string{"oauth_client_id": "test-client", "X-Test": "value"}, Tags: []string{"one", "two"},
	}
	if err := s.SaveAccount(want); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openConfigTestStore(t, path)
	if got := loadConfigTestAccount(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("configuration changed after reopen: got=%+v want=%+v", got, want)
	}
	want.MaxConcurrency, want.Weight, want.RateBurst = 4, 9, 3
	want.Proxy, want.RatePerSecond = "http://127.0.0.1:8080", 0.5
	want.CookieMap = map[string]string{"changed": "cookie"}
	want.Headers["oauth_client_id"] = "replacement-client"
	if err := s.SaveAccount(want); err != nil {
		t.Fatal(err)
	}
	if got := loadConfigTestAccount(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("configuration changed after update: got=%+v want=%+v", got, want)
	}
}

func TestSQLiteAccountConfigOldSchemaMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE accounts (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, plan TEXT DEFAULT 'pro', email TEXT DEFAULT '',
		cookies TEXT DEFAULT '', access_token TEXT DEFAULT '', refresh_token TEXT DEFAULT '',
		max_concurrency INTEGER DEFAULT 2, tags TEXT DEFAULT '', updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO accounts (id, name, access_token, max_concurrency) VALUES ('legacy', 'Legacy', 'old-access', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openConfigTestStore(t, path)
	a := loadConfigTestAccount(t, s)
	if !a.IsEnabled() || a.MaxConcurrency != 0 || a.AccessToken != "old-access" || a.CookieMap != nil || a.Headers != nil || a.ExpiresAt != nil {
		t.Fatalf("legacy values changed: %+v", a)
	}
	a.SessionToken = "new-session"
	a.Headers = map[string]string{"oauth_client_id": "migrated-client"}
	a.Tags = []string{}
	if err := s.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openConfigTestStore(t, path)
	if got := loadConfigTestAccount(t, s); !reflect.DeepEqual(got, a) {
		t.Fatalf("migrated account changed after reopen: got=%+v want=%+v", got, a)
	}
}

func TestSQLiteAccountConfigSessionForms(t *testing.T) {
	for _, a := range []config.AccountConfig{
		{ID: "session", SessionToken: "session-only"},
		{ID: "map", CookieMap: map[string]string{creds.CookiePrismSessionToken: "map-session", "other": "value"}},
	} {
		t.Run(a.ID, func(t *testing.T) {
			s := openConfigTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
			if err := s.SaveAccount(a); err != nil {
				t.Fatal(err)
			}
			got := loadConfigTestAccount(t, s)
			if got.SessionToken != a.SessionToken || !reflect.DeepEqual(got.CookieMap, a.CookieMap) || !creds.FromAccountConfig(got).CanRefresh() {
				t.Fatalf("session credentials lost: %+v", got)
			}
			expected := creds.FromAccountConfig(a)
			next := expected.Clone()
			next.AccessToken, next.RefreshToken = "new-access", "new-refresh"
			if updated, err := s.UpdateCredential(a.ID, expected, next); err != nil || !updated {
				t.Fatalf("session refresh: updated=%v err=%v", updated, err)
			}
			if updated, err := s.UpdateCredential(a.ID, expected, next); err != nil || updated {
				t.Fatalf("stale session refresh: updated=%v err=%v", updated, err)
			}
		})
	}
}

func TestSQLiteUpdateCredentialPreservesDashboardSettings(t *testing.T) {
	s := openConfigTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
	a := config.AccountConfig{ID: "refresh", Name: "Before", Plan: "pro", AccessToken: "old-access", RefreshToken: "old-refresh",
		Cookies: "device=old", SessionToken: "session-old",
		Headers: map[string]string{"oauth_client_id": "client", "X-Edited": "old", "X-Remove": "remove"}}
	if err := s.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	expected := creds.FromAccountConfig(a).Clone()
	next := expected.Clone()
	next.AccessToken, next.RefreshToken, next.AccountID = "new-access", "new-refresh", "new-account"
	next.ExpiresAt = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	next.CookieHeader, next.SessionToken = "device=stale-refresh", "session-stale-refresh"
	next.Headers["X-Edited"] = "stale-refresh"
	next.Headers["X-New"] = "new"
	delete(next.Headers, "X-Remove")
	enabled := false
	a.Name, a.Proxy, a.MaxConcurrency, a.Weight = "Dashboard", "http://127.0.0.1:1234", 0, 5
	a.Enabled, a.RatePerSecond, a.RateBurst, a.Tags = &enabled, 0.75, 6, []string{"dashboard"}
	a.Headers["X-Edited"], a.Headers["X-Dashboard"] = "dashboard", "keep"
	a.Cookies = "device=dashboard"
	if err := s.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	if updated, err := s.UpdateCredential(a.ID, expected, next); err != nil || !updated {
		t.Fatalf("refresh: updated=%v err=%v", updated, err)
	}
	want := a
	want.AccessToken, want.RefreshToken, want.AccountID, want.ExpiresAt = next.AccessToken, next.RefreshToken, next.AccountID, &next.ExpiresAt
	want.SessionToken = next.SessionToken
	want.Headers["X-New"] = "new"
	delete(want.Headers, "X-Remove")
	if got := loadConfigTestAccount(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("refresh overwrote settings: got=%+v want=%+v", got, want)
	}
	if updated, err := s.UpdateCredential(a.ID, expected, next); err != nil || updated {
		t.Fatalf("stale refresh accepted: updated=%v err=%v", updated, err)
	}
	if err := s.DeleteAccount(a.ID); err != nil {
		t.Fatal(err)
	}
	if updated, err := s.UpdateCredential(a.ID, next, next); err != nil || updated {
		t.Fatalf("deleted account updated: updated=%v err=%v", updated, err)
	}
	if err := s.SaveImportedAccount(want); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Load(); err != nil || len(list) != 0 {
		t.Fatalf("deleted account resurrected: count=%d err=%v", len(list), err)
	}
}

func TestSQLiteUpdateCredentialRejectsReplacedCookie(t *testing.T) {
	s := openConfigTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
	a := config.AccountConfig{ID: "cookie", CookieMap: map[string]string{creds.CookiePrismSessionToken: "old-session", "other": "old"}}
	if err := s.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	expected := creds.FromAccountConfig(a)
	a.CookieMap[creds.CookiePrismSessionToken] = "changed-session"
	if err := s.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	next := expected.Clone()
	next.AccessToken = "stale-access"
	if updated, err := s.UpdateCredential(a.ID, expected, next); err != nil || updated {
		t.Fatalf("replaced cookie accepted: updated=%v err=%v", updated, err)
	}
}

func TestSQLiteUpdateCredentialReplacesCookieMapWithRefreshedHeader(t *testing.T) {
	s := openConfigTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
	a := config.AccountConfig{ID: "cookie", CookieMap: map[string]string{creds.CookiePrismSessionToken: "old-session", "other": "value"}}
	if err := s.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	expected := creds.FromAccountConfig(a)
	next := expected.Clone()
	next.CookieHeader = creds.CookiePrismSessionToken + "=new-session; other=value"
	next.SessionToken = "new-session"
	next.AccessToken = "new-access"
	if updated, err := s.UpdateCredential(a.ID, expected, next); err != nil || !updated {
		t.Fatalf("cookie refresh: updated=%v err=%v", updated, err)
	}
	got := loadConfigTestAccount(t, s)
	if got.CookieMap != nil || got.Cookies != next.CookieHeader || got.SessionToken != next.SessionToken || creds.FromAccountConfig(got).CookieHeader != next.CookieHeader {
		t.Fatalf("cookie refresh duplicated or lost credentials: %+v", got)
	}
}

func TestSQLitePassiveImportRetainsRotatedCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	s := openConfigTestStore(t, path)
	a := config.AccountConfig{ID: "file", Name: "File", AccessToken: "file-access", RefreshToken: "file-refresh",
		Headers: map[string]string{"oauth_client_id": "client"}, MaxConcurrency: 2}
	if err := s.SaveImportedAccount(a); err != nil {
		t.Fatal(err)
	}
	old := creds.FromAccountConfig(a)
	next := old.Clone()
	next.AccessToken, next.RefreshToken = "rotated-access", "rotated-refresh"
	next.ExpiresAt = time.Now().UTC().Add(time.Hour)
	if updated, err := s.UpdateCredential(a.ID, old, next); err != nil || !updated {
		t.Fatalf("refresh: updated=%v err=%v", updated, err)
	}
	dashboard := loadConfigTestAccount(t, s)
	disabled := false
	dashboard.Enabled, dashboard.Name, dashboard.MaxConcurrency, dashboard.Proxy = &disabled, "Dashboard", 0, "http://127.0.0.1:3128"
	if err := s.SaveAccount(dashboard); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openConfigTestStore(t, path)
	a.Weight = 20 // A scheduling-only file edit must not replay its credentials.
	if err := s.SaveImportedAccount(a); err != nil {
		t.Fatal(err)
	}
	if got := loadConfigTestAccount(t, s); !reflect.DeepEqual(got, dashboard) {
		t.Fatalf("passive import changed rotated account: got=%+v want=%+v", got, dashboard)
	}
	a.AccessToken, a.RefreshToken = "replaced-access", "replaced-refresh"
	if err := s.SaveImportedAccount(a); err != nil {
		t.Fatal(err)
	}
	got := loadConfigTestAccount(t, s)
	if got.AccessToken != a.AccessToken || got.RefreshToken != a.RefreshToken || got.Name != dashboard.Name || got.IsEnabled() || got.MaxConcurrency != 0 || got.Proxy != dashboard.Proxy || got.Weight != dashboard.Weight {
		t.Fatalf("changed source did not preserve settings: %+v", got)
	}
	if err := s.DeleteAccount(a.ID); err != nil {
		t.Fatal(err)
	}
	a.AccessToken = "changed-after-delete"
	if err := s.SaveImportedAccount(a); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Load(); err != nil || len(list) != 0 {
		t.Fatalf("passive import resurrected deletion: count=%d err=%v", len(list), err)
	}
	if err := s.SaveAccounts([]config.AccountConfig{a}); err != nil {
		t.Fatal(err)
	}
	if got := loadConfigTestAccount(t, s); got.AccessToken != a.AccessToken {
		t.Fatal("deliberate reimport did not restore account")
	}
}

func TestSQLitePassiveLegacyImportMergesMissingFields(t *testing.T) {
	s := openConfigTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
	newExpiry := time.Now().UTC().Add(2 * time.Hour)
	oldExpiry := newExpiry.Add(-time.Hour)
	disabled := false
	a := config.AccountConfig{ID: "legacy", Name: "Dashboard", AccessToken: "new-access", RefreshToken: "new-refresh",
		ExpiresAt: &newExpiry, Enabled: &disabled, MaxConcurrency: 0, Tags: []string{"dashboard"},
		Headers: map[string]string{"X-Dashboard": "keep"}}
	if err := s.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	file := config.AccountConfig{ID: a.ID, Name: "File", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: &oldExpiry,
		SessionToken: "file-session", Proxy: "http://127.0.0.1:8080", RatePerSecond: 0.5, RateBurst: 3, Weight: 7,
		CookieMap: map[string]string{"other": "cookie"}, Headers: map[string]string{"oauth_client_id": "file-client"}, MaxConcurrency: 9}
	if err := s.SaveImportedAccount(file); err != nil {
		t.Fatal(err)
	}
	got := loadConfigTestAccount(t, s)
	if got.AccessToken != a.AccessToken || got.RefreshToken != a.RefreshToken || !got.ExpiresAt.Equal(newExpiry) || got.Name != a.Name || got.IsEnabled() || got.MaxConcurrency != 0 || !reflect.DeepEqual(got.Tags, a.Tags) {
		t.Fatalf("legacy import overwrote newer credential/settings: %+v", got)
	}
	if got.SessionToken != file.SessionToken || !reflect.DeepEqual(got.CookieMap, file.CookieMap) || got.Proxy != file.Proxy || got.RatePerSecond != file.RatePerSecond || got.RateBurst != file.RateBurst || got.Weight != file.Weight || got.Headers["oauth_client_id"] != "file-client" || got.Headers["X-Dashboard"] != "keep" {
		t.Fatalf("legacy import lost missing supported fields: %+v", got)
	}
}

func TestSQLiteSaveAccountsRollsBackBatchAndTombstones(t *testing.T) {
	s := openConfigTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
	a := config.AccountConfig{ID: "deleted"}
	if err := s.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAccounts([]config.AccountConfig{a, {ID: "invalid", MaxConcurrency: -1}}); err == nil {
		t.Fatal("invalid batch succeeded")
	}
	if err := s.SaveImportedAccount(a); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Load(); err != nil || len(list) != 0 {
		t.Fatalf("failed batch changed accounts or tombstone: count=%d err=%v", len(list), err)
	}
}

func TestSQLitePrivatePermissionsAndAutomaticPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := openConfigTestStore(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions=%o", info.Mode().Perm())
	}
	if err := s.SaveAccount(config.AccountConfig{ID: "auto"}); err != nil {
		t.Fatal(err)
	}
	if got := loadConfigTestAccount(t, s); got.Plan != "" {
		t.Fatalf("automatic plan became %q", got.Plan)
	}
}

func TestSQLiteCreatesPrivateDirectoryWithoutChangingExistingParent(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "private", "database")
	openConfigTestStore(t, filepath.Join(dir, "accounts.db"))
	for _, path := range []string{filepath.Dir(dir), dir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("new directory permissions=%o", info.Mode().Perm())
		}
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("existing directory permissions changed: %o", info.Mode().Perm())
	}
}

func TestSQLiteExplicitPrunePreservesCutoffAndNewerRows(t *testing.T) {
	s := openConfigTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
	cutoff := time.Date(2030, 5, 4, 12, 30, 0, 0, time.FixedZone("UTC+8", 8*3600))
	for i, id := range []string{"old", "cutoff", "new"} {
		stamp := cutoff.Add(time.Duration(i-1) * time.Second)
		if err := s.SaveNativeBinding(NativeBindingRecord{Key: id, Updated: stamp}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordRequestLog(RequestLogItem{ID: id, Timestamp: stamp}); err != nil {
			t.Fatal(err)
		}
	}
	if _, count, err := s.QueryRequestLogs(RequestLogFilter{}); err != nil || count != 3 {
		t.Fatalf("logs pruned without explicit request: count=%d err=%v", count, err)
	}
	if err := s.PruneNativeBindings(cutoff); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneRequestLogs(cutoff); err != nil {
		t.Fatal(err)
	}
	bindings, err := s.LoadNativeBindings(time.Time{})
	if err != nil || len(bindings) != 2 {
		t.Fatalf("retained bindings=%d err=%v", len(bindings), err)
	}
	for _, binding := range bindings {
		if binding.Key == "old" {
			t.Fatal("expired binding retained")
		}
	}
	logs, count, err := s.QueryRequestLogs(RequestLogFilter{})
	if err != nil || count != 2 || len(logs) != 2 {
		t.Fatalf("retained logs=%d count=%d err=%v", len(logs), count, err)
	}
	for _, log := range logs {
		if log.ID == "old" {
			t.Fatal("expired log retained")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, unavailable := range []*SQLiteStore{nil, {}, s} {
		if err := unavailable.PruneNativeBindings(cutoff); !errors.Is(err, errSQLiteUnavailable) {
			t.Fatalf("unavailable binding prune: %v", err)
		}
		if err := unavailable.PruneRequestLogs(cutoff); !errors.Is(err, errSQLiteUnavailable) {
			t.Fatalf("unavailable log prune: %v", err)
		}
	}
}

func TestSQLiteDeliberateTokenReplacementSynchronizesCookies(t *testing.T) {
	for _, source := range []string{"header", "map"} {
		for _, kind := range []string{"access", "refresh", "session", "all"} {
			t.Run(source+"/"+kind, func(t *testing.T) {
				s := openConfigTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
				a := config.AccountConfig{ID: "same", AccessToken: "old-access", RefreshToken: "old-refresh", SessionToken: "old-session"}
				if source == "header" {
					a.Cookies = "prism_oai_access_token=old-access; prism_oai_refresh_token=old-refresh; prism_session_token=old-session; __Secure-next-auth.session-token=old-session; device=keep"
				} else {
					a.CookieMap = map[string]string{creds.CookiePrismAccessToken: "old-access", creds.CookiePrismRefreshToken: "old-refresh", creds.CookiePrismSessionToken: "old-session", creds.CookieSessionToken: "old-session", "device": "keep"}
				}
				if err := s.SaveAccount(a); err != nil {
					t.Fatal(err)
				}
				replacement := config.AccountConfig{ID: "same"}
				if kind == "access" || kind == "all" {
					replacement.AccessToken = "new-access"
				}
				if kind == "refresh" || kind == "all" {
					replacement.RefreshToken = "new-refresh"
				}
				if kind == "session" || kind == "all" {
					replacement.SessionToken = "new-session"
				}
				if err := s.SaveAccounts([]config.AccountConfig{replacement}); err != nil {
					t.Fatal(err)
				}
				got := creds.FromAccountConfig(loadConfigTestAccount(t, s))
				for _, token := range []struct{ name, value string }{
					{creds.CookiePrismAccessToken, got.AccessToken},
					{creds.CookiePrismRefreshToken, got.RefreshToken},
					{creds.CookiePrismSessionToken, got.SessionToken},
					{creds.CookieSessionToken, got.SessionToken},
				} {
					if value := creds.CookieValue(got.EffectiveCookie(), token.name); value != token.value {
						t.Fatalf("%s cookie and credential disagree", token.name)
					}
				}
				if replacement.AccessToken != "" && got.AccessToken != replacement.AccessToken || replacement.RefreshToken != "" && got.RefreshToken != replacement.RefreshToken || replacement.SessionToken != "" && got.SessionToken != replacement.SessionToken {
					t.Fatal("replacement credential was not saved")
				}
				if creds.CookieValue(got.EffectiveCookie(), "device") != "keep" {
					t.Fatal("unrelated cookie was lost")
				}
			})
		}
	}
}

func TestSQLiteDeliberateCookieReplacementClearsPreviousRepresentation(t *testing.T) {
	for _, source := range []string{"header", "map"} {
		t.Run(source, func(t *testing.T) {
			s := openConfigTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
			old := config.AccountConfig{ID: "same", AccessToken: "old-access", RefreshToken: "old-refresh", SessionToken: "old-session"}
			replacement := config.AccountConfig{ID: "same"}
			if source == "header" {
				old.CookieMap = map[string]string{creds.CookiePrismAccessToken: "old-access", "stale": "discard"}
				replacement.Cookies = "prism_oai_access_token=new-access; prism_oai_refresh_token=new-refresh; prism_session_token=new-session; device=new"
			} else {
				old.Cookies = "prism_oai_access_token=old-access; stale=discard"
				replacement.CookieMap = map[string]string{creds.CookiePrismAccessToken: "new-access", creds.CookiePrismRefreshToken: "new-refresh", creds.CookiePrismSessionToken: "new-session", "device": "new"}
			}
			if err := s.SaveAccount(old); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveAccount(replacement); err != nil {
				t.Fatal(err)
			}
			saved := loadConfigTestAccount(t, s)
			if source == "header" && saved.CookieMap != nil || source == "map" && saved.Cookies != "" {
				t.Fatal("superseded cookie representation retained")
			}
			got := creds.FromAccountConfig(saved)
			if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || got.SessionToken != "new-session" {
				t.Fatal("cookie replacement retained old token fields")
			}
			if creds.CookieValue(got.EffectiveCookie(), "stale") != "" || creds.CookieValue(got.EffectiveCookie(), "device") != "new" {
				t.Fatal("cookie replacement mixed previous and new cookies")
			}
		})
	}
}
