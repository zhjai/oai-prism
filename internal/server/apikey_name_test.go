package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
)

func TestAdmin_APIKeyRename(t *testing.T) {
	testServer, _ := newTestServer(t, &fakeUpstream{t: t}, []config.AccountConfig{
		{ID: "a", AccessToken: "synthetic-rename-token-a"},
		{ID: "b", AccessToken: "synthetic-rename-token-b"},
	}, func(settings *config.Config) {
		settings.Facade.APIKeys = []string{"synthetic-rename-admin-key"}
	})
	adminToken := "synthetic-rename-admin-key"
	call := func(method, path, body, token string, want int) string {
		t.Helper()
		headers := map[string]string{}
		if token != "" {
			headers["Authorization"] = "Bearer " + token
		}
		status, output := doLocal(t, method, testServer.URL+path, body, headers)
		if status != want {
			t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, want, output)
		}
		return output
	}
	var created account.APIKeyItem
	output := call(http.MethodPost, "/admin/apikeys", `{"name":"Original display name","account_ids":["a"]}`, adminToken, http.StatusOK)
	if err := json.Unmarshal([]byte(output), &created); err != nil {
		t.Fatal(err)
	}
	path := "/admin/apikeys/" + created.Key
	baseline := call(http.MethodGet, "/admin/apikeys", "", adminToken, http.StatusOK)
	if !strings.Contains(baseline, `"name":"Original display name"`) {
		t.Fatal("created key is absent from the inventory")
	}
	previousName := "Original display name"
	for _, example := range []struct {
		input string
		want  string
	}{
		{input: "  Renamed display name  ", want: "Renamed display name"},
		{input: strings.Repeat("名", 64), want: strings.Repeat("名", 64)},
		{input: "   ", want: ""},
	} {
		payload, err := json.Marshal(map[string]string{"name": example.input})
		if err != nil {
			t.Fatal(err)
		}
		output = call(http.MethodPatch, path, string(payload), adminToken, http.StatusOK)
		var response struct {
			Status string `json:"status"`
			Name   string `json:"name"`
		}
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			t.Fatal(err)
		}
		if response.Status != "ok" || response.Name != example.want {
			t.Fatalf("unexpected rename response: %+v", response)
		}
		oldNameJSON, _ := json.Marshal(previousName)
		newNameJSON, _ := json.Marshal(example.want)
		expected := strings.Replace(baseline, `"name":`+string(oldNameJSON), `"name":`+string(newNameJSON), 1)
		updated := call(http.MethodGet, "/admin/apikeys", "", adminToken, http.StatusOK)
		if updated != expected {
			t.Fatal("rename changed inventory fields other than the target display name")
		}
		baseline, previousName = updated, example.want
	}
	longName, _ := json.Marshal(map[string]string{"name": strings.Repeat("名", 65)})
	for _, payload := range []string{
		`{}`, `null`, `{"name":null}`, `{"name":42}`, `{"name":[]}`,
		`{"name":"bad\nname"}`, `{"name":"\tname"}`,
		`{"name":"changed","account_ids":["b"]}`,
		`{"name":"changed","key":"replacement"}`,
		`{"name":"first"}{"name":"second"}`, string(longName),
		`{"name":"` + strings.Repeat("x", 4096) + `"}`,
	} {
		call(http.MethodPatch, path, payload, adminToken, http.StatusBadRequest)
	}
	call(http.MethodPatch, path, `{"name":"unauthorized"}`, "", http.StatusUnauthorized)
	if response := remote(t, testServer, http.MethodPatch, path, `{"name":"not an admin"}`, map[string]string{"Authorization": "Bearer " + created.Key}); response.Code != http.StatusForbidden {
		t.Fatalf("remote ordinary key rename: status %d, want 403", response.Code)
	}
	call(http.MethodPatch, "/admin/apikeys/synthetic-missing-key", `{"name":"missing"}`, adminToken, http.StatusNotFound)
	call(http.MethodPatch, "/admin/apikeys/"+adminToken, `{"name":"static key"}`, adminToken, http.StatusNotFound)
	if updated := call(http.MethodGet, "/admin/apikeys", "", adminToken, http.StatusOK); updated != baseline {
		t.Fatal("rejected rename changed the key inventory")
	}
	call(http.MethodGet, "/v1/models", "", created.Key, http.StatusOK)
}
