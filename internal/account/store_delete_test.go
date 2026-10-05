package account

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

func TestStore_DeletePreservesDocument(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		name := "array"
		if wrapped {
			name = "document"
		}
		t.Run(name, func(t *testing.T) {
			entries := `[ {"accessToken":"remove"}, {"access-token":"keep","enabled":false,"proxy":"http://proxy:8080","headers":{"custom":"keep"},"unknown":{"nested":[1,2]}} ]`
			raw := entries
			if wrapped {
				raw = `{"version":4,"metadata":{"keep":true},"accounts":` + entries + `}`
			}
			path := filepath.Join(t.TempDir(), "accounts.json")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			s := NewStore(path, nopLog())
			if _, err := s.Load(); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteAccount("file-1"); err != nil {
				t.Fatal(err)
			}
			list, err := s.Load()
			if err != nil || len(list) != 1 {
				t.Fatalf("remaining list=%d err=%v", len(list), err)
			}
			a := list[0]
			if a.ID != "file-2" || a.AccessToken != "keep" || a.IsEnabled() || a.Proxy != "http://proxy:8080" || a.Headers["custom"] != "keep" {
				t.Fatal("remaining identity, credentials or settings changed")
			}
			out, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var actual any
			var expected any
			expectedRaw := `[{"id":"file-2","access-token":"keep","enabled":false,"proxy":"http://proxy:8080","headers":{"custom":"keep"},"unknown":{"nested":[1,2]}}]`
			if wrapped {
				expectedRaw = `{"version":4,"metadata":{"keep":true},"accounts":` + expectedRaw + `}`
			}
			if json.Unmarshal(out, &actual) != nil || json.Unmarshal([]byte(expectedRaw), &expected) != nil || !reflect.DeepEqual(actual, expected) {
				t.Fatal("deletion lost document metadata or unknown credential fields")
			}
			if err := s.DeleteAccount("missing"); err != nil {
				t.Fatal(err)
			}
			unchanged, _ := os.ReadFile(path)
			if string(out) != string(unchanged) {
				t.Fatal("missing ID rewrote the document")
			}
			if err := s.DeleteAccount("file-2"); err != nil {
				t.Fatal(err)
			}
			list, err = NewStore(path, nopLog()).Load()
			if err != nil || len(list) != 0 {
				t.Fatalf("last account deletion failed: count=%d err=%v", len(list), err)
			}
		})
	}
}

func TestStore_DeleteDuringWatch(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "accounts.json"), nopLog())
	initial := []config.AccountConfig{{ID: "removed", AccessToken: "old"}, {ID: "kept", AccessToken: "keep"}}
	if err := s.Persist(initial); err != nil {
		t.Fatal(err)
	}
	// External change triggers the real watcher; pause its callback at the old snapshot.
	raw, _ := json.Marshal(map[string]any{"accounts": initial, "external": true})
	if err := os.WriteFile(s.Path(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan []config.AccountConfig, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Watch(ctx, time.Millisecond, func(list []config.AccountConfig) {
			entered <- list
			<-release
		})
	}()
	defer func() { cancel(); close(release); <-done }()
	select {
	case list := <-entered:
		if len(list) != 2 {
			t.Fatal("watcher did not capture initial list")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher callback did not run")
	}
	if err := s.DeleteAccount("removed"); err != nil {
		t.Fatal(err)
	}
	if list := s.Accounts(); len(list) != 1 || list[0].ID != "kept" {
		t.Fatal("watcher overwrote deletion in the store cache")
	}
}

func TestStore_PersistPreservesMetadataAndUntouchedAccounts(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		raw := `[{"id":"a","accessToken":"old","unknown":{"a":1}},{"id":"b","access-token":"kept","custom":true}]`
		if wrapped {
			raw = `{"metadata":{"version":8},"accounts":` + raw + `}`
		}
		path := filepath.Join(t.TempDir(), "accounts.json")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		s := NewStore(path, nopLog())
		list, err := s.Load()
		if err != nil {
			t.Fatal(err)
		}
		list[0].AccessToken = "new"
		list = append(list, config.AccountConfig{ID: "c", AccessToken: "added"})
		if err := s.Persist(list); err != nil {
			t.Fatal(err)
		}
		out, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var entries []map[string]json.RawMessage
		if wrapped {
			var doc map[string]json.RawMessage
			if json.Unmarshal(out, &doc) != nil || string(doc["metadata"]) == "" {
				t.Fatal("wrapper metadata lost")
			}
			out = doc["accounts"]
		}
		if json.Unmarshal(out, &entries) != nil || len(entries) != 3 {
			t.Fatal("invalid persisted document")
		}
		if string(entries[0]["access_token"]) != `"new"` || entries[0]["accessToken"] != nil || entries[0]["unknown"] == nil {
			t.Fatal("updated account lost metadata or retained stale token alias")
		}
		if string(entries[1]["access-token"]) != `"kept"` || string(entries[1]["custom"]) != "true" {
			t.Fatal("untouched entry was rewritten")
		}
	}
}
