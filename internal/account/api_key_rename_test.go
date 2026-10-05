package account

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

func TestSQLiteRenameAPIKeyPreservesMetadataAndBindings(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "accounts.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, id := range []string{"account-a", "account-b"} {
		if err := store.SaveAccount(config.AccountConfig{ID: id, AccessToken: "test-token"}); err != nil {
			t.Fatal(err)
		}
	}
	const key = "sk-rename-preserve"
	if err := store.SaveAPIKey(APIKeyItem{Key: key, Name: "before", AccountIDs: []string{"account-a", "account-b"}, AccountRestricted: true}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListAPIKeys()
	if err != nil || len(before) != 1 {
		t.Fatalf("before: %v, %+v", err, before)
	}
	if err := store.RenameAPIKey(key, "after"); err != nil {
		t.Fatal(err)
	}
	after, err := store.ListAPIKeys()
	if err != nil || len(after) != 1 {
		t.Fatalf("after: %v, %+v", err, after)
	}
	got := after[0]
	if got.Key != before[0].Key || !got.CreatedAt.Equal(before[0].CreatedAt) || got.AccountRestricted != before[0].AccountRestricted {
		t.Fatalf("rename changed metadata: before=%+v after=%+v", before[0], got)
	}
	if got.Name != "after" || len(got.AccountIDs) != 2 || got.AccountIDs[0] != "account-a" || got.AccountIDs[1] != "account-b" {
		t.Fatalf("rename changed name/bindings: %+v", got)
	}
}

func TestSQLiteRenameAPIKeyNotFound(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "accounts.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RenameAPIKey("missing", "name"); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("RenameAPIKey missing = %v", err)
	}
}
