package facade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/metrics"
	"github.com/oai-prism/oaiprism/internal/middleware"
	"github.com/oai-prism/oaiprism/internal/prism"
)

type catalogFixture struct {
	handler *Handler
	pool    *account.Pool
	mux     *http.ServeMux
	other   atomic.Int32
	onOther http.HandlerFunc
}

func newCatalogFixture(t *testing.T, serve http.HandlerFunc, accounts ...config.AccountConfig) *catalogFixture {
	t.Helper()
	f := &catalogFixture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != prism.PathInferenceModels {
			f.other.Add(1)
			if f.onOther != nil {
				f.onOther(w, r)
				return
			}
			http.Error(w, "unexpected inference side effect", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		serve(w, r)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.URL
	cfg.Creds.Accounts = accounts
	cfg.Facade.ModelCatalog.Enabled = true
	cfg.Facade.ModelCatalog.RefreshInterval = time.Hour
	cfg.Facade.DefaultModel = "retired-model"
	cfg.Facade.Models = map[string]config.ModelMapping{
		"retired-model": {Model: "retired-model", Label: "Retired model"},
		"old-alias":     {Model: "retired-model", Label: "Old alias"},
		"current-alias": {Model: "model-a", Label: "Current alias"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var err error
	f.pool, err = account.NewPool(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.pool.Close)
	client := prism.New(f.pool.Client(), prism.UpstreamOptions{}, prism.SchemaOptions{StartPath: prism.PathResponseStart, StatusPath: prism.PathResponseStatus})
	app := metrics.NewApp()
	runner := NewRunner(cfg, log, f.pool, client, app)
	t.Cleanup(runner.Close)
	f.handler = NewHandler(cfg, log, runner, app)
	f.mux = http.NewServeMux()
	f.handler.Register(f.mux)
	return f
}

func catalogAccounts() []config.AccountConfig {
	return []config.AccountConfig{
		{ID: "a", AccessToken: "synthetic-a", MaxConcurrency: 1},
		{ID: "b", AccessToken: "synthetic-b", MaxConcurrency: 1},
	}
}

func serveAccountCatalog(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer synthetic-")
	json.NewEncoder(w).Encode([]prism.UpstreamModel{{ID: "model-" + id, Label: "Model " + id}})
}

func catalogModelIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("catalog status %d: %s", w.Code, w.Body.String())
	}
	var list ModelList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || list.Data == nil {
		t.Fatalf("invalid model list: %s", w.Body.String())
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestDynamicCatalogKeyScopesAndPinnedAccounts(t *testing.T) {
	f := newCatalogFixture(t, serveAccountCatalog, catalogAccounts()...)
	scopes := map[string]account.Scope{
		"key-a":       {Restricted: true, AccountIDs: []string{"a"}},
		"key-b":       {Restricted: true, AccountIDs: []string{"b"}},
		"key-empty":   {Restricted: true},
		"key-deleted": {Restricted: true, AccountIDs: []string{"deleted"}},
	}
	authed := middleware.Auth(middleware.AuthOptions{StaticKeys: []string{"admin"}, DynamicScopes: func() map[string]account.Scope { return scopes }})(f.mux)
	for _, tc := range []struct {
		name, key, pin string
		status         int
		ids            []string
	}{
		{"all", "admin", "", 200, []string{"model-a", "model-b", "current-alias"}},
		{"a only", "key-a", "", 200, []string{"model-a", "current-alias"}},
		{"b only", "key-b", "", 200, []string{"model-b"}},
		{"empty binding", "key-empty", "", 200, []string{}},
		{"deleted binding", "key-deleted", "", 200, []string{}},
		{"pinned", "admin", "b", 200, []string{"model-b"}},
		{"forbidden pin", "key-a", "b", 403, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.Header.Set("Authorization", "Bearer "+tc.key)
			r.Header.Set(HeaderAccount, tc.pin)
			w := httptest.NewRecorder()
			authed.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status == 200 {
				if got := catalogModelIDs(t, w); !reflect.DeepEqual(got, tc.ids) {
					t.Fatalf("model IDs %v, want %v", got, tc.ids)
				}
			}
		})
	}
	for _, id := range []string{"model-b", "retired-model", "old-alias"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/models/"+id, nil)
		r.Header.Set("Authorization", "Bearer key-a")
		w := httptest.NewRecorder()
		authed.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "model_not_found") {
			t.Fatalf("out-of-scope or retired model %q accessible: %d %s", id, w.Code, w.Body.String())
		}
	}
	scopes["key-a"] = account.Scope{Restricted: true}
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer key-a")
	w := httptest.NewRecorder()
	authed.ServeHTTP(w, r)
	if got := catalogModelIDs(t, w); len(got) != 0 {
		t.Fatalf("warm account cache bypassed revoked binding: %v", got)
	}
}

func TestDynamicCatalogBusyAndDisabledAccounts(t *testing.T) {
	disabled := false
	accounts := append(catalogAccounts(), config.AccountConfig{ID: "disabled", AccessToken: "synthetic-disabled", Enabled: &disabled})
	accounts = append(accounts, config.AccountConfig{ID: "auth-failed", AccessToken: "synthetic-auth-failed"}, config.AccountConfig{ID: "cooldown", AccessToken: "synthetic-cooldown"})
	var calls atomic.Int32
	f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); serveAccountCatalog(w, r) }, accounts...)
	if !f.pool.Get("a").Acquire(time.Now()) {
		t.Fatal("could not occupy inference slot")
	}
	defer f.pool.Get("a").Release()
	f.pool.Get("auth-failed").MarkAuthFailed()
	f.pool.Get("cooldown").Cooldown(time.Now(), time.Hour)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if got := catalogModelIDs(t, w); !reflect.DeepEqual(got, []string{"model-a", "model-b", "model-auth-failed", "current-alias"}) || calls.Load() != 3 {
		t.Fatalf("busy/disabled filtering: models=%v fetches=%d", got, calls.Load())
	}
	if f.pool.Get("auth-failed").AuthFailed() {
		t.Fatal("successful catalog reprobe did not clear stale authentication failure")
	}
	if f.pool.Get("b").Stats(time.Now()).Inflight != 0 {
		t.Fatal("catalog occupied an inference slot")
	}
}

func TestDynamicCatalogFailureAndEmptyNeverFallback(t *testing.T) {
	for _, tc := range []struct {
		name, body             string
		upstreamStatus, status int
	}{
		{"upstream failure", `{"error":"synthetic-private-detail"}`, 401, 503},
		{"malformed response", `{"error":"synthetic-private-detail"}`, 200, 503},
		{"empty catalog", `[]`, 200, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.upstreamStatus)
				io.WriteString(w, tc.body)
			}, catalogAccounts()[0])
			for i := 0; i < 2; i++ {
				w := httptest.NewRecorder()
				f.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
				if w.Code != tc.status {
					t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "synthetic-private-detail") || strings.Contains(w.Body.String(), "retired-model") {
					t.Fatalf("fallback or private upstream detail leaked: %s", w.Body.String())
				}
				if tc.status == 200 && len(catalogModelIDs(t, w)) != 0 {
					t.Fatal("empty upstream catalog populated from static config")
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("catalog result not cached: %d requests", calls.Load())
			}
		})
	}
}

func TestDynamicCatalogCacheCredentialAndAccountReplacement(t *testing.T) {
	var calls atomic.Int32
	f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); serveAccountCatalog(w, r) }, catalogAccounts()[0])
	fetch := func(want string) {
		t.Helper()
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		ids := catalogModelIDs(t, w)
		if len(ids) == 0 || ids[0] != want {
			t.Fatalf("models=%v, want first %s", ids, want)
		}
	}
	fetch("model-a")
	fetch("model-a")
	if calls.Load() != 1 {
		t.Fatalf("cache missed: %d calls", calls.Load())
	}
	cred := f.pool.Get("a").Credential().Clone()
	cred.AccessToken = "synthetic-replaced"
	f.pool.Get("a").StoreCredential(cred)
	fetch("model-replaced")
	if calls.Load() != 2 {
		t.Fatalf("credential replacement did not refresh: %d calls", calls.Load())
	}
	if err := f.pool.Build([]config.AccountConfig{{ID: "a", AccessToken: "synthetic-rebuilt"}}); err != nil {
		t.Fatal(err)
	}
	fetch("model-rebuilt")
	if calls.Load() != 3 {
		t.Fatalf("account replacement did not refresh: %d calls", calls.Load())
	}
}

func TestDynamicCatalogRejectsRetiredModelsBeforeInference(t *testing.T) {
	f := newCatalogFixture(t, serveAccountCatalog, catalogAccounts()[0])
	for _, tc := range []struct{ path, fields string }{
		{"/v1/chat/completions", `"messages":[{"role":"user","content":"answer"}]`},
		{"/v1/responses", `"input":"answer"`},
		{"/v1/messages", `"messages":[{"role":"user","content":"answer"}],"max_tokens":8`},
		{"/v1/completions", `"prompt":"answer"`},
	} {
		for _, stream := range []bool{false, true} {
			for _, model := range []string{"retired-model", "old-alias"} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", tc.path, stream, model), func(t *testing.T) {
					body := fmt.Sprintf(`{"model":%q,"stream":%v,%s}`, model, stream, tc.fields)
					w := httptest.NewRecorder()
					f.mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body)))
					if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "model_not_found") {
						t.Fatalf("retired model response: %d %s", w.Code, w.Body.String())
					}
					if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
						t.Fatalf("stream started before model validation: %v", w.Header())
					}
					if f.other.Load() != 0 {
						t.Fatalf("inference/project/sandbox started: %d calls", f.other.Load())
					}
				})
			}
		}
	}
}

func TestDynamicCatalogPrepareModelNarrowsSchedulingScope(t *testing.T) {
	f := newCatalogFixture(t, serveAccountCatalog, catalogAccounts()...)
	for _, tc := range []struct {
		name, model, pin string
		scope            account.Scope
		want             string
		ok               bool
	}{
		{"account entitlement", "model-a", "", account.Scope{}, "a", true},
		{"preserve restriction", "model-b", "", account.Scope{Restricted: true, AccountIDs: []string{"b"}}, "b", true},
		{"cannot expand restriction", "model-a", "", account.Scope{Restricted: true, AccountIDs: []string{"b"}}, "", false},
		{"empty binding", "model-a", "", account.Scope{Restricted: true}, "", false},
		{"pinned account lacks model", "model-a", "b", account.Scope{}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			r = r.WithContext(account.WithScope(r.Context(), tc.scope))
			r.Header.Set(HeaderAccount, tc.pin)
			w := httptest.NewRecorder()
			model, effort := tc.model, ""
			prepared, ok := f.handler.prepareModel(w, r, &model, &effort, false, false)
			if ok != tc.ok {
				t.Fatalf("prepareModel=%v: %d %s", ok, w.Code, w.Body.String())
			}
			if !ok {
				return
			}
			scope := account.ScopeFromContext(prepared.Context())
			if !scope.Restricted || !reflect.DeepEqual(scope.AccountIDs, []string{tc.want}) {
				t.Fatalf("incorrect scheduling scope: %+v", scope)
			}
		})
	}
}

func TestDynamicCatalogConcurrentFetchAndCancelledWaiter(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-release
		serveAccountCatalog(w, r)
	}, catalogAccounts()[0])
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	first := make(chan error, 1)
	go func() { _, err := f.handler.accountModels(context.Background(), f.pool.Get("a")); first <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream fetch did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter := make(chan error, 1)
	go func() { _, err := f.handler.accountModels(ctx, f.pool.Get("a")); waiter <- err }()
	select {
	case err := <-waiter:
		t.Fatalf("waiter returned before fetch completed or cancellation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-waiter:
		if err != context.Canceled {
			t.Fatalf("cancelled waiter: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("cancelled catalog waiter remained blocked behind upstream fetch")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if _, err := f.handler.accountModels(context.Background(), f.pool.Get("a")); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent/cache requests made %d upstream calls", calls.Load())
	}
}

func TestDynamicCatalogExpiredCacheDoesNotReturnRetiredModels(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			http.Error(w, "synthetic failure", http.StatusServiceUnavailable)
			return
		}
		serveAccountCatalog(w, r)
	}, catalogAccounts()[0])
	a := f.pool.Get("a")
	if _, err := f.handler.accountModels(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	e := f.handler.catalog.entry(a)
	e.mu.Lock()
	e.fetched = time.Now().Add(-2 * time.Hour)
	e.mu.Unlock()
	fail.Store(true)
	for i := 0; i < 2; i++ {
		if models, err := f.handler.accountModels(context.Background(), a); err == nil || len(models) != 0 {
			t.Fatalf("failed refresh served stale models: %+v, %v", models, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("failed refresh was not backed off: %d requests", calls.Load())
	}

	fail.Store(false)
	cred := a.Credential().Clone()
	cred.AccessToken = "synthetic-recovered"
	a.StoreCredential(cred)
	models, err := f.handler.accountModels(context.Background(), a)
	if err != nil || len(models) != 1 || models[0].ID != "model-recovered" {
		t.Fatalf("replacement credential retained failed/stale cache: %+v, %v", models, err)
	}
}

func TestDynamicCatalogCredentialReplacedDuringFetch(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer synthetic-a" {
			close(started)
			<-release
		}
		serveAccountCatalog(w, r)
	}, catalogAccounts()[0])
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	a := f.pool.Get("a")
	done := make(chan error, 1)
	go func() {
		models, err := f.handler.accountModels(context.Background(), a)
		if err == nil || len(models) > 0 {
			done <- fmt.Errorf("replaced credential returned old entitlement: %+v, %v", models, err)
			return
		}
		done <- nil
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream fetch did not start")
	}
	cred := a.Credential().Clone()
	cred.AccessToken = "synthetic-replaced"
	a.StoreCredential(cred)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	models, err := f.handler.accountModels(context.Background(), a)
	if err != nil || len(models) != 1 || models[0].ID != "model-replaced" {
		t.Fatalf("replacement credential did not fetch its own catalog: %+v, %v", models, err)
	}
}

func TestDynamicCatalogDefaultAndTTLRefresh(t *testing.T) {
	var calls atomic.Int32
	var replacement atomic.Bool
	f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		id := "model-current"
		if replacement.Load() {
			id = "model-new"
		}
		json.NewEncoder(w).Encode([]prism.UpstreamModel{{ID: id, Label: id}})
	}, catalogAccounts()[0])
	prepare := func(requested string, useDefault bool, want string, accepted bool) {
		t.Helper()
		model, effort := f.handler.resolveModel(requested, "")
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		prepared, ok := f.handler.prepareModel(w, r, &model, &effort, useDefault, false)
		if ok != accepted || (ok && model != want) {
			t.Fatalf("prepare %q: model=%q accepted=%v; want model=%q accepted=%v; body=%s", requested, model, ok, want, accepted, w.Body.String())
		}
		if ok && !reflect.DeepEqual(account.ScopeFromContext(prepared.Context()).AccountIDs, []string{"a"}) {
			t.Fatalf("selected model has incorrect account scope: %+v", account.ScopeFromContext(prepared.Context()))
		}
		if !ok && (w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "model_not_found")) {
			t.Fatalf("retired model returned %d: %s", w.Code, w.Body.String())
		}
	}
	prepare("", true, "model-current", true)
	replacement.Store(true)
	prepare("model-current", false, "model-current", true)
	if calls.Load() != 1 {
		t.Fatalf("unexpired catalog fetched %d times", calls.Load())
	}
	e := f.handler.catalog.entry(f.pool.Get("a"))
	e.mu.Lock()
	e.fetched = time.Now().Add(-2 * f.handler.cfg.Facade.ModelCatalog.RefreshInterval)
	e.mu.Unlock()
	prepare("model-new", false, "model-new", true)
	prepare("model-current", false, "", false)
	prepare("", true, "model-new", true)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if ids := catalogModelIDs(t, w); !reflect.DeepEqual(ids, []string{"model-new"}) {
		t.Fatalf("TTL refresh did not update visible directory: %v", ids)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected one refresh, got %d total calls", calls.Load())
	}
}

func TestDynamicCatalogAccountSpecificEfforts(t *testing.T) {
	f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		efforts := []string{"low"}
		if r.Header.Get("Authorization") == "Bearer synthetic-b" {
			efforts = []string{"low", "high"}
		}
		json.NewEncoder(w).Encode([]prism.UpstreamModel{{ID: "shared", Label: "Shared", Efforts: efforts, DefaultEffort: "low"}})
	}, catalogAccounts()...)
	f.handler.cfg.Facade.Models["shared-high"] = config.ModelMapping{Model: "shared", ReasoningEffort: "high"}
	f.handler.cfg.Facade.Models["shared-unsupported"] = config.ModelMapping{Model: "shared", ReasoningEffort: "unsupported"}
	for _, effort := range []string{"high", "unsupported"} {
		model := "shared"
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		prepared, ok := f.handler.prepareModel(w, r, &model, &effort, false, true)
		if effort == "unsupported" {
			if ok || w.Code != http.StatusNotFound {
				t.Fatalf("unsupported effort accepted: %d %s", w.Code, w.Body.String())
			}
			continue
		}
		if !ok || !reflect.DeepEqual(account.ScopeFromContext(prepared.Context()).AccountIDs, []string{"b"}) {
			t.Fatalf("high effort scheduled outside supporting account: ok=%v scope=%+v", ok, account.ScopeFromContext(prepared.Context()))
		}
	}
	for _, ids := range [][]string{{"a"}, {"b"}, {"a", "b"}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r = r.WithContext(account.WithScope(r.Context(), account.Scope{Restricted: true, AccountIDs: ids}))
		f.mux.ServeHTTP(w, r)
		got := catalogModelIDs(t, w)
		want := []string{"shared"}
		if ids[len(ids)-1] == "b" {
			want = append(want, "shared-high")
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("accounts %v advertised unsupported alias: got %v, want %v", ids, got, want)
		}
	}
}

func TestDynamicCatalogSharedModelEffortMetadata(t *testing.T) {
	f := newCatalogFixture(t, serveAccountCatalog, catalogAccounts()[0])
	infos := f.handler.liveModelInfos([]accountCatalog{
		{id: "a", models: []prism.UpstreamModel{{ID: "shared", Label: "Shared", Efforts: []string{"low"}, DefaultEffort: "low"}}},
		{id: "b", models: []prism.UpstreamModel{{ID: "shared", Label: "Shared", Efforts: []string{"low", "high"}, DefaultEffort: "low"}}},
	}, true)
	if len(infos) == 0 || !reflect.DeepEqual(infos[0].ReasoningEfforts, []string{"low", "high"}) {
		t.Fatalf("shared model metadata omits usable effort from later account: %+v", infos)
	}
}

func TestDynamicCatalogDefaultEffortOverridesOutdatedBaseMapping(t *testing.T) {
	f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]prism.UpstreamModel{{ID: "model-a", Label: "A", Efforts: []string{"high"}, DefaultEffort: "high"}})
	}, catalogAccounts()[0])
	f.handler.cfg.Facade.Models["model-a"] = config.ModelMapping{Model: "model-a", ReasoningEffort: "medium"}
	model, effort := f.handler.resolveModel("model-a", "")
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	w := httptest.NewRecorder()
	if _, ok := f.handler.prepareModel(w, r, &model, &effort, false, false); !ok || effort != "high" {
		t.Fatalf("available base model rejected by outdated effort mapping: effort=%s code=%d", effort, w.Code)
	}
	model, effort = f.handler.resolveModel("model-a", "medium")
	w = httptest.NewRecorder()
	if _, ok := f.handler.prepareModel(w, r, &model, &effort, false, true); ok {
		t.Fatal("explicit unsupported effort was silently replaced")
	}
}

func TestDynamicCatalogHeaderModelEffortAtProtocolEntries(t *testing.T) {
	for _, tc := range []struct{ path, fields string }{
		{"/v1/chat/completions", `"messages":[{"role":"user","content":"answer"}]`},
		{"/v1/responses", `"input":"answer"`},
		{"/v1/messages", `"messages":[{"role":"user","content":"answer"}],"max_tokens":8`},
		{"/v1/completions", `"prompt":"answer"`},
	} {
		for _, headerEffort := range []string{"", "low"} {
			t.Run(tc.path+"/effort="+headerEffort, func(t *testing.T) {
				f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
					json.NewEncoder(w).Encode([]prism.UpstreamModel{{ID: "model-a", Label: "A", Efforts: []string{"low", "high"}}})
				}, catalogAccounts()[0])
				f.handler.cfg.Facade.Models["body-low"] = config.ModelMapping{Model: "model-a", ReasoningEffort: "low"}
				f.handler.cfg.Facade.Models["header-high"] = config.ModelMapping{Model: "model-a", ReasoningEffort: "high"}
				captured := make(chan map[string]any, 2)
				f.onOther = func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == prism.PathResponseStart {
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Errorf("decode start body: %v", err)
						}
						captured <- body
					}
					http.Error(w, "synthetic stop before inference", http.StatusBadRequest)
				}
				f.handler.runner.sandboxes.Put("a", &prism.Sandbox{URL: f.handler.cfg.Upstream.BaseURL, Token: "synthetic"})
				f.handler.runner.sandboxes.MarkSynced("a", "project", time.Now().Add(time.Hour))
				body := `{"model":"body-low",` + tc.fields + `}`
				r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
				r.Header.Set(HeaderModel, "header-high")
				r.Header.Set(HeaderEffort, headerEffort)
				r.Header.Set(HeaderProject, "project")
				r.Header.Set(HeaderSession, t.Name())
				w := httptest.NewRecorder()
				f.mux.ServeHTTP(w, r)
				select {
				case started := <-captured:
					metadata, _ := started["metadata"].(map[string]any)
					wantEffort := "high"
					if headerEffort != "" {
						wantEffort = headerEffort
					}
					if metadata["model"] != "model-a" || metadata["reasoning_effort"] != wantEffort {
						t.Fatalf("header alias did not resolve model/effort before start: metadata=%v, want model-a/%s", metadata, wantEffort)
					}
				default:
					t.Fatalf("no upstream start captured: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestDynamicCatalogIncompleteKnowledgeIsRetryable(t *testing.T) {
	for _, cause := range []string{"cooldown", "catalog failure"} {
		t.Run(cause, func(t *testing.T) {
			var aCalls atomic.Int32
			f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer synthetic-a" {
					aCalls.Add(1)
					if cause == "catalog failure" {
						http.Error(w, "temporary directory outage", http.StatusBadGateway)
						return
					}
				}
				serveAccountCatalog(w, r)
			}, catalogAccounts()...)
			if cause == "cooldown" {
				f.pool.Get("a").Cooldown(time.Now(), time.Hour)
			}
			w := httptest.NewRecorder()
			f.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			if ids := catalogModelIDs(t, w); !reflect.DeepEqual(ids, []string{"model-b"}) {
				t.Fatalf("partial directory should retain confirmed account: %v", ids)
			}
			for _, tc := range []struct {
				model string
				ok    bool
			}{{"model-a", false}, {"model-b", true}} {
				model, effort := tc.model, ""
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				prepared, ok := f.handler.prepareModel(w, r, &model, &effort, false, false)
				if ok != tc.ok {
					t.Fatalf("prepare %s: ok=%v, response=%d %s", tc.model, ok, w.Code, w.Body.String())
				}
				if !ok && (w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "model_catalog_unavailable") || strings.Contains(w.Body.String(), "model_not_found")) {
					t.Fatalf("unknown entitlement treated as permanent retirement: %d %s", w.Code, w.Body.String())
				}
				if ok && !reflect.DeepEqual(account.ScopeFromContext(prepared.Context()).AccountIDs, []string{"b"}) {
					t.Fatalf("confirmed model incorrectly scoped: %+v", account.ScopeFromContext(prepared.Context()))
				}
			}
			if cause == "cooldown" && aCalls.Load() != 0 {
				t.Fatal("cooling account was queried")
			}
			if f.other.Load() != 0 {
				t.Fatal("catalog validation caused inference side effects")
			}
		})
	}
}

func TestDynamicCatalogAuthAndRateLimitUpdatePool(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "30")
				http.Error(w, "synthetic rejection", status)
			}, catalogAccounts()[0])
			for i := 0; i < 2; i++ {
				w := httptest.NewRecorder()
				f.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("catalog failure status=%d: %s", w.Code, w.Body.String())
				}
			}
			a := f.pool.Get("a")
			if a.AuthFailed() != (status == http.StatusUnauthorized) || a.CooldownRemaining(time.Now()) <= 0 || a.Available(time.Now()) {
				t.Fatalf("pool ignored catalog rejection: authFailed=%v cooldown=%s available=%v", a.AuthFailed(), a.CooldownRemaining(time.Now()), a.Available(time.Now()))
			}
			if status == http.StatusTooManyRequests && a.CooldownRemaining(time.Now()) < 25*time.Second {
				t.Fatalf("Retry-After not applied: %s", a.CooldownRemaining(time.Now()))
			}
			if calls.Load() != 1 {
				t.Fatalf("cooldown failed to suppress repeated catalog fetch: %d", calls.Load())
			}
		})
	}
}

func TestDynamicCatalogAuthenticationRecoversAfterCooldown(t *testing.T) {
	var calls atomic.Int32
	f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		serveAccountCatalog(w, r)
	}, catalogAccounts()[0])
	a := f.pool.Get("a")
	a.MarkAuthFailed()
	a.MarkFailure(time.Now().Add(-time.Hour), time.Minute, 2, 10*time.Minute)
	a.MarkFailure(time.Now().Add(-time.Hour), time.Minute, 2, 10*time.Minute)
	prior := a.Stats(time.Now())
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if ids := catalogModelIDs(t, w); !reflect.DeepEqual(ids, []string{"model-a", "current-alias"}) {
		t.Fatalf("expired authentication cooldown blocked revalidation: %v", ids)
	}
	if calls.Load() != 1 || a.AuthFailed() || !a.Available(time.Now()) {
		t.Fatalf("catalog success failed to recover account: calls=%d authFailed=%v available=%v", calls.Load(), a.AuthFailed(), a.Available(time.Now()))
	}
	if after := a.Stats(time.Now()); after.FailStreak != prior.FailStreak || after.Failures != prior.Failures {
		t.Fatalf("catalog authentication erased inference failure accounting: before=%+v after=%+v", prior, after)
	}
}

func TestDynamicCatalogRetiredDefaultDropsOnlyImplicitEffort(t *testing.T) {
	f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]prism.UpstreamModel{{ID: "replacement", Label: "Replacement", Efforts: []string{"low", "high"}, DefaultEffort: "high"}})
	}, catalogAccounts()[0])
	f.handler.cfg.Facade.DefaultModel = "retired-alias"
	f.handler.cfg.Facade.Models["retired-alias"] = config.ModelMapping{Model: "retired-model", ReasoningEffort: "xhigh"}
	for _, tc := range []struct {
		name, requestedEffort, wantEffort string
		explicit, ok                      bool
	}{
		{"implicit stale mapping", "", "high", false, true},
		{"explicit supported", "low", "low", true, true},
		{"explicit unsupported", "xhigh", "xhigh", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model, effort := f.handler.resolveModel("", tc.requestedEffort)
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			_, ok := f.handler.prepareModel(w, r, &model, &effort, true, tc.explicit)
			if ok != tc.ok || model != "replacement" || effort != tc.wantEffort {
				t.Fatalf("default substitution: ok=%v model=%s effort=%s status=%d; want ok=%v replacement/%s", ok, model, effort, w.Code, tc.ok, tc.wantEffort)
			}
			if !ok && w.Code != http.StatusNotFound {
				t.Fatalf("unsupported explicit effort status=%d", w.Code)
			}
		})
	}
}

func TestDynamicCatalogDeadlineBackoffAndCancellationIsolation(t *testing.T) {
	for _, timeout := range []bool{true, false} {
		t.Run(fmt.Sprintf("deadline=%v", timeout), func(t *testing.T) {
			started := make(chan struct{})
			var calls atomic.Int32
			f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(started)
					<-r.Context().Done()
					return
				}
				serveAccountCatalog(w, r)
			}, catalogAccounts()[0])
			ctx, cancel := context.WithCancel(context.Background())
			if timeout {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			first := make(chan error, 1)
			go func() {
				_, err := f.handler.accountModels(ctx, f.pool.Get("a"))
				first <- err
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("first catalog fetch did not start")
			}
			if !timeout {
				cancel()
			}
			select {
			case err := <-first:
				if err == nil {
					t.Fatal("interrupted catalog fetch succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("catalog fetch did not honor request termination")
			}
			secondCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			models, err := f.handler.accountModels(secondCtx, f.pool.Get("a"))
			if timeout {
				if err == nil || len(models) != 0 || calls.Load() != 1 {
					t.Fatalf("deadline failure was not backed off: models=%v err=%v calls=%d", models, err, calls.Load())
				}
			} else if err != nil || len(models) != 1 || models[0].ID != "model-a" || calls.Load() != 2 {
				t.Fatalf("cancelled caller poisoned next fetch: models=%v err=%v calls=%d", models, err, calls.Load())
			}
		})
	}
}

func TestDynamicCatalogIncompleteDefaultDoesNotSubstitute(t *testing.T) {
	for _, cause := range []string{"cooldown", "fetch failure"} {
		t.Run(cause, func(t *testing.T) {
			f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer synthetic-a" {
					http.Error(w, "temporary directory outage", http.StatusBadGateway)
					return
				}
				serveAccountCatalog(w, r)
			}, catalogAccounts()...)
			f.handler.cfg.Facade.DefaultModel = "model-a"
			if cause == "cooldown" {
				f.pool.Get("a").Cooldown(time.Now(), time.Hour)
			}
			listed := httptest.NewRecorder()
			f.mux.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			if ids := catalogModelIDs(t, listed); !reflect.DeepEqual(ids, []string{"model-b"}) {
				t.Fatalf("partial models response lost known model: %v", ids)
			}
			var list ModelList
			if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			for _, info := range list.Data {
				if info.Default {
					t.Fatalf("partial directory falsely advertised substitute default: %+v", info)
				}
			}
			model, effort := f.handler.resolveModel("", "")
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if _, ok := f.handler.prepareModel(w, r, &model, &effort, true, false); ok || w.Code != http.StatusServiceUnavailable || model != "model-a" {
				t.Fatalf("partial directory substituted configured default: accepted=%v model=%s response=%d %s", ok, model, w.Code, w.Body.String())
			}
			if f.other.Load() != 0 {
				t.Fatal("default validation started inference")
			}
		})
	}
}

func TestDynamicCatalogContinuityGuard(t *testing.T) {
	f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer synthetic-a" {
			http.Error(w, "temporary catalog failure", http.StatusBadGateway)
			return
		}
		serveAccountCatalog(w, r)
	}, catalogAccounts()...)
	for _, tc := range []struct {
		name, bound, pinned string
		ok                  bool
	}{
		{"unknown existing owner", "a", "", false},
		{"confirmed owner", "b", "", true},
		{"new conversation", "", "", true},
		{"explicit account change", "a", "b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			r.Header.Set(HeaderAccount, tc.pinned)
			model, effort := "model-b", ""
			w := httptest.NewRecorder()
			prepared, ok := f.handler.prepareModel(w, r, &model, &effort, false, false)
			if !ok {
				t.Fatalf("confirmed model was unavailable: %d %s", w.Code, w.Body.String())
			}
			req := &RunRequest{BoundAccountID: tc.bound, AccountID: tc.pinned, ProjectID: "old-project", ConversationID: "old-conversation"}
			if ok := verifyCatalogContinuity(w, prepared, req); ok != tc.ok {
				t.Fatalf("continuity guard=%v, want %v: %d %s", ok, tc.ok, w.Code, w.Body.String())
			}
			if !tc.ok && (w.Code != http.StatusServiceUnavailable || req.ProjectID != "old-project" || req.ConversationID != "old-conversation") {
				t.Fatalf("unknown owner lost continuation or nonretryable response: req=%+v status=%d", req, w.Code)
			}
		})
	}
}

func TestDynamicCatalogHandlersPreserveUnconfirmedContinuation(t *testing.T) {
	for _, tc := range []struct{ path, fields string }{
		{"/v1/chat/completions", `"messages":[{"role":"user","content":"continue"}]`},
		{"/v1/messages", `"messages":[{"role":"user","content":"continue"}],"max_tokens":8`},
		{"/v1/responses", `"input":"continue"`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.path, stream), func(t *testing.T) {
				f := newCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") == "Bearer synthetic-a" {
						http.Error(w, "temporary catalog failure", http.StatusBadGateway)
						return
					}
					serveAccountCatalog(w, r)
				}, catalogAccounts()...)
				key := "h:" + t.Name()
				binding := nativeBindingFor(key)
				binding.mu.Lock()
				binding.account, binding.project, binding.cid = "a", "old-project", "old-conversation"
				binding.mu.Unlock()
				t.Cleanup(func() {
					nativeBindings.mu.Lock()
					delete(nativeBindings.m, key)
					nativeBindings.mu.Unlock()
				})
				body := fmt.Sprintf(`{"model":"model-b","stream":%v,%s}`, stream, tc.fields)
				r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
				r.Header.Set(HeaderSession, t.Name())
				w := httptest.NewRecorder()
				f.mux.ServeHTTP(w, r)
				if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "model_catalog_unavailable") || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
					t.Fatalf("unconfirmed continuation started streaming or lost retryability: %d %s", w.Code, w.Body.String())
				}
				if f.other.Load() != 0 {
					t.Fatalf("unconfirmed owner caused project/sandbox/inference traffic: %d", f.other.Load())
				}
				binding.mu.Lock()
				defer binding.mu.Unlock()
				if binding.account != "a" || binding.project != "old-project" || binding.cid != "old-conversation" {
					t.Fatalf("continuation mutated after catalog outage: %+v", binding)
				}
			})
		}
	}
}
