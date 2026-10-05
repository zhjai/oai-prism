package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
)

func importTestJWT(t *testing.T, email, marker string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"email": email, "exp": time.Now().Add(time.Hour).Unix(), "synthetic": marker})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".synthetic"
}

func offlineImportArgs(path, token string) []string {
	return []string{"-config", filepath.Join(filepath.Dir(path), "missing-config.yaml"), "-out", path, "-skip-verify", "-access-token", token}
}

func loadImportTestAccounts(t *testing.T, path string) []config.AccountConfig {
	t.Helper()
	list, err := account.NewStore(path, slog.New(slog.NewTextHandler(io.Discard, nil))).Load()
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestImportOfflineDefaultIDsKeepDistinctIdentitiesAndUpdateSameIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	first := importTestJWT(t, "first@example.invalid", "first")
	second := importTestJWT(t, "second@example.invalid", "second")
	for _, token := range []string{first, second} {
		if err := cmdImport(offlineImportArgs(path, token)); err != nil {
			t.Fatal(err)
		}
	}
	list := loadImportTestAccounts(t, path)
	if len(list) != 2 || list[0].ID == "" || list[1].ID == "" || list[0].ID == list[1].ID {
		t.Fatal("default-ID imports overwrote distinct identities")
	}
	firstID := ""
	for _, a := range list {
		if a.Email == "first@example.invalid" && a.AccessToken == first {
			firstID = a.ID
		}
	}
	if firstID == "" {
		t.Fatal("first JWT email and credential were not retained")
	}
	updated := importTestJWT(t, "FIRST@example.invalid", "updated")
	if err := cmdImport(offlineImportArgs(path, updated)); err != nil {
		t.Fatal(err)
	}
	list = loadImportTestAccounts(t, path)
	if len(list) != 2 {
		t.Fatalf("same identity changed account count: %d", len(list))
	}
	for _, a := range list {
		if a.ID == firstID {
			if a.AccessToken != updated {
				t.Fatal("same identity did not replace existing credential")
			}
		} else if a.Email != "second@example.invalid" || a.AccessToken != second {
			t.Fatal("same-identity update modified the other account")
		}
	}
}

func TestImportPreservesCorruptCredentialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	original := []byte(`{"accounts":[{"id":"keep","access_token":"synthetic"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdImport(offlineImportArgs(path, importTestJWT(t, "new@example.invalid", "new"))); err == nil {
		t.Fatal("corrupt credential file was accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("corrupt credential file was modified: err=%v", err)
	}
}

func TestImportRejectsNegativeConcurrencyWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	args := append(offlineImportArgs(path, importTestJWT(t, "new@example.invalid", "new")), "-max-concurrency", "-1")
	if err := cmdImport(args); err == nil {
		t.Fatal("negative concurrency was accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rejected import created credential file: %v", err)
	}
}

func TestImportExplicitReimportClearsSiblingSQLiteTombstone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := account.NewSQLiteStore(filepath.Join(filepath.Dir(path), "accounts.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SaveAccount(config.AccountConfig{ID: "restored", AccessToken: "synthetic-old"}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAccount("restored"); err != nil {
		t.Fatal(err)
	}
	passive := []config.AccountConfig{{ID: "restored", AccessToken: "synthetic-stale"}}
	if visible, err := db.FilterDeletedAccounts(passive); err != nil || len(visible) != 0 {
		t.Fatalf("test requires existing tombstone: count=%d err=%v", len(visible), err)
	}
	token := importTestJWT(t, "restored@example.invalid", "restored")
	if err := cmdImport(append(offlineImportArgs(path, token), "-id", "restored")); err != nil {
		t.Fatal(err)
	}
	stored, err := db.Load()
	if err != nil || len(stored) != 1 || stored[0].ID != "restored" || stored[0].AccessToken != token {
		t.Fatalf("explicit reimport did not restore sibling database account: count=%d err=%v", len(stored), err)
	}
	if visible, err := db.FilterDeletedAccounts(passive); err != nil || len(visible) != 1 {
		t.Fatalf("explicit reimport retained tombstone: count=%d err=%v", len(visible), err)
	}
	file := loadImportTestAccounts(t, path)
	if len(file) != 1 || file[0].ID != "restored" || file[0].AccessToken != token {
		t.Fatal("explicit reimport did not persist matching JSON account")
	}
}
