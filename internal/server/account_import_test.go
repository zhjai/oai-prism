package server

import (
	"encoding/json"
	"github.com/oai-prism/oaiprism/internal/account"
	"net/http"
	"testing"
)

func TestAdmin_DirectCookieImportVerifiedAndRedacted(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, nil, nil)
	body := `{"accounts":[{"name":"Imported Plus","cookies":"prism_oai_access_token=test-token; prism_session_token=test-session","max_concurrency":5,"plan":"plus","enabled":false}],"verify":true}`
	code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts/import", body, nil)
	if code != http.StatusCreated {
		t.Fatalf("import: %d %s", code, out)
	}
	var response struct {
		Accounts []struct {
			ID string `json:"id"`
		} `json:"accounts"`
	}
	if json.Unmarshal([]byte(out), &response) != nil || len(response.Accounts) != 1 {
		t.Fatalf("invalid response: %s", out)
	}
	code, out = doLocal(t, http.MethodGet, ts.URL+"/admin/accounts", "", nil)
	var inventory struct {
		Accounts []account.Stats `json:"accounts"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(out), &inventory) != nil || len(inventory.Accounts) != 1 {
		t.Fatalf("inventory: %d %s", code, out)
	}
	a := inventory.Accounts[0]
	if a.MaxConcur != 5 || a.Enabled || a.Plan != "plus" {
		t.Fatal("import settings were not applied")
	}
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts/import", body, nil); code != http.StatusCreated {
		t.Fatalf("repeat import: %d %s", code, out)
	}
	_, out = doLocal(t, http.MethodGet, ts.URL+"/admin/accounts", "", nil)
	if json.Unmarshal([]byte(out), &inventory) != nil || len(inventory.Accounts) != 1 {
		t.Fatal("same verified identity was duplicated")
	}
}

func TestAdmin_DirectImportValidationDoesNotPartiallySave(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, nil, nil)
	for _, body := range []string{
		`{"accounts":[],"verify":false}`,
		`{"accounts":[{"id":"a","access_token":"synthetic"},{"id":"b"}],"verify":false}`,
		`{"accounts":[{"id":"a","access_token":"synthetic","max_concurrency":-1}],"verify":false}`,
		`{"accounts":[{"id":"a","access_token":"synthetic"},{"id":"a","access_token":"other"}],"verify":false}`,
	} {
		if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts/import", body, nil); code != http.StatusBadRequest {
			t.Fatalf("invalid import accepted: %d %s", code, out)
		}
		_, out := doLocal(t, http.MethodGet, ts.URL+"/admin/accounts", "", nil)
		var inventory struct {
			Accounts []account.Stats `json:"accounts"`
		}
		if json.Unmarshal([]byte(out), &inventory) != nil || len(inventory.Accounts) != 0 {
			t.Fatal("invalid batch partially imported")
		}
	}
	body := `{"accounts":[{"id":"offline","name":"Offline","access_token":"synthetic","max_concurrency":0}],"verify":false}`
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts/import", body, nil); code != http.StatusCreated {
		t.Fatalf("offline: %d %s", code, out)
	}
	_, out := doLocal(t, http.MethodGet, ts.URL+"/admin/accounts", "", nil)
	var inventory struct {
		Accounts []account.Stats `json:"accounts"`
	}
	if json.Unmarshal([]byte(out), &inventory) != nil || len(inventory.Accounts) != 1 || inventory.Accounts[0].MaxConcur != 0 {
		t.Fatal("unlimited concurrency not preserved")
	}
}
