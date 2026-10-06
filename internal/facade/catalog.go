package facade

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/middleware"
	"github.com/oai-prism/oaiprism/internal/prism"
)

// Each account has its own upstream entitlements. Never share a catalog between
// keys or reuse it after an account or its credential has been replaced.
type catalogEntry struct {
	mu         sync.Mutex
	account    *account.Account
	credential *creds.Credential
	models     []prism.UpstreamModel
	fetched    time.Time
	attempted  time.Time
	err        error
	fetching   chan struct{}
}

type modelCatalog struct {
	mu      sync.Mutex
	entries map[string]*catalogEntry
}

var errCatalogUnavailable = errors.New("无法获取上游模型目录，请检查账号凭据和网络后重试")
var errCatalogIncomplete = errors.New("部分账号的模型目录暂不可用")

func (c *modelCatalog) entry(a *account.Account) *catalogEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*catalogEntry)
	}
	e := c.entries[a.ID]
	if e == nil || e.account != a {
		e = &catalogEntry{account: a}
		c.entries[a.ID] = e
	}
	return e
}

func (c *modelCatalog) prune(accounts []*account.Account) {
	c.mu.Lock()
	defer c.mu.Unlock()
	live := make(map[string]*account.Account, len(accounts))
	for _, a := range accounts {
		live[a.ID] = a
	}
	for id, e := range c.entries {
		if live[id] != e.account {
			delete(c.entries, id)
		}
	}
}

func (h *Handler) accountModels(ctx context.Context, a *account.Account) ([]prism.UpstreamModel, error) {
	e := h.catalog.entry(a)
	ttl := h.cfg.Facade.ModelCatalog.RefreshInterval
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	var cred *creds.Credential
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		e.mu.Lock()
		if pending := e.fetching; pending != nil {
			e.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		cred = a.Credential()
		if cred == nil {
			e.mu.Unlock()
			return nil, errCatalogUnavailable
		}
		if e.credential != cred {
			e.models, e.fetched, e.attempted, e.err = nil, time.Time{}, time.Time{}, nil
			e.credential = cred
		}
		if e.err != nil && time.Since(e.attempted) < 15*time.Second {
			err := e.err
			e.mu.Unlock()
			return nil, err
		}
		if !a.AuthFailed() && !e.fetched.IsZero() && time.Since(e.fetched) < ttl {
			models := e.models
			e.mu.Unlock()
			return models, nil
		}
		e.fetching = make(chan struct{})
		e.mu.Unlock()
		break
	}
	fctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	models, err := h.runner.client.InferenceModels(fctx, prism.Principal{
		Client: a.Client, Cred: cred, ExtraHeaders: cred.Headers, AccountID: a.ID,
	})
	e.mu.Lock()
	defer e.mu.Unlock()
	defer func() { close(e.fetching); e.fetching = nil }()
	// A cancelled requester must not poison other callers' cached entitlement.
	if errors.Is(ctx.Err(), context.Canceled) || a.Credential() != cred {
		return nil, errCatalogUnavailable
	}
	e.attempted = time.Now()
	if err != nil {
		// Do not forward an upstream body or auth material in catalog diagnostics.
		if creds.IsAuthError(err) || creds.IsRateLimited(err) {
			var apiErr *creds.APIError
			if errors.As(err, &apiErr) {
				safeErr := &creds.APIError{Op: "model_catalog", Status: apiErr.Status, RetryAfter: apiErr.RetryAfter}
				h.runner.pool.MarkResult(a, safeErr, apiErr.RetryAfter)
			}
		}
		e.err = errCatalogUnavailable
		return nil, e.err
	}
	if a.AuthFailed() && a.CooldownRemaining(time.Now()) == 0 {
		a.MarkAuthenticated()
	}
	e.models, e.fetched, e.err = models, time.Now(), nil
	return models, nil
}

type accountCatalog struct {
	id     string
	models []prism.UpstreamModel
}

type unconfirmedCatalogKey struct{}

func (h *Handler) liveCatalog(r *http.Request) ([]accountCatalog, error) {
	if h.runner == nil || h.runner.pool == nil {
		return nil, errCatalogUnavailable
	}
	all := h.runner.pool.Accounts()
	h.catalog.prune(all)
	selected := strings.TrimSpace(r.Header.Get(HeaderAccount))
	if selected != "" && !account.Allowed(r.Context(), selected) {
		return nil, errCatalogForbidden
	}
	candidates := make([]*account.Account, 0, len(all))
	incomplete := false
	now := time.Now()
	for _, a := range all {
		if !account.Allowed(r.Context(), a.ID) || (selected != "" && selected != a.ID) {
			continue
		}
		cred := a.Credential()
		if !a.IsEnabled() || cred == nil || !cred.Usable() {
			continue
		}
		if a.CooldownRemaining(now) > 0 {
			incomplete = true
			continue
		}
		candidates = append(candidates, a)
	}
	// Four workers bound upstream traffic. A busy inference slot does not hide
	// an otherwise usable account's models or consume its inference quota.
	results := make([]accountCatalog, len(candidates))
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	slots := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, a := range candidates {
		wg.Add(1)
		go func(i int, a *account.Account) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			models, err := h.accountModels(ctx, a)
			if err == nil && !a.AuthFailed() && (a.Available(time.Now()) || a.Busy(time.Now())) {
				results[i] = accountCatalog{id: a.ID, models: models}
			}
		}(i, a)
	}
	wg.Wait()
	current := make(map[string]*account.Account)
	for _, a := range h.runner.pool.Accounts() {
		current[a.ID] = a
	}
	out := make([]accountCatalog, 0, len(results))
	for i, s := range results {
		if s.id != "" && current[s.id] == candidates[i] && !current[s.id].AuthFailed() &&
			(current[s.id].Available(time.Now()) || current[s.id].Busy(time.Now())) {
			out = append(out, s)
		} else {
			incomplete = true
		}
	}
	if incomplete && len(out) == 0 {
		return nil, errCatalogUnavailable
	}
	if incomplete {
		return out, errCatalogIncomplete
	}
	return out, nil
}

var errCatalogForbidden = errors.New("指定账号不在 API Key 绑定范围内")

func writeCatalogError(w http.ResponseWriter, err error) {
	if errors.Is(err, errCatalogForbidden) {
		writeErrorCode(w, http.StatusForbidden, "permission_error", "account_not_allowed", err.Error())
	} else {
		writeErrorCode(w, http.StatusServiceUnavailable, "server_error", "model_catalog_unavailable", errCatalogUnavailable.Error())
	}
}

// prepareModel checks the resolved upstream model before projects, sandboxes or
// SSE are started and limits scheduling to accounts that actually offer it.
func (h *Handler) prepareModel(w http.ResponseWriter, r *http.Request, model, effort *string, defaultRequested, effortExplicit bool) (*http.Request, bool) {
	if !h.cfg.Facade.ModelCatalog.Enabled {
		middleware.RecordLogModel(r, *model)
		return r, true
	}
	catalogs, err := h.liveCatalog(r)
	if err != nil && !errors.Is(err, errCatalogIncomplete) {
		writeCatalogError(w, err)
		return r, false
	}
	has := func(id string) bool {
		for _, c := range catalogs {
			for _, m := range c.models {
				if m.ID == id {
					return true
				}
			}
		}
		return false
	}
	if defaultRequested && !has(*model) {
		if errors.Is(err, errCatalogIncomplete) {
			writeCatalogError(w, err)
			return r, false
		}
		if !effortExplicit {
			*effort = ""
		}
		*model = ""
		for _, c := range catalogs {
			if len(c.models) > 0 {
				*model = c.models[0].ID
				break
			}
		}
	}
	// Dynamic effort aliases exist only if the catalog advertises them.
	if !has(*model) {
		for _, info := range h.liveModelInfos(catalogs, !errors.Is(err, errCatalogIncomplete)) {
			if info.ID == *model {
				if *effort == "" {
					*effort = info.ReasoningEffort
				}
				*model = info.UpstreamModel
				break
			}
		}
	}
	if *effort == "" {
		for _, c := range catalogs {
			for _, m := range c.models {
				if m.ID == *model {
					*effort = h.catalogDefaultEffort(m)
					break
				}
			}
			if *effort != "" {
				break
			}
		}
	}
	ids := []string{}
	for _, c := range catalogs {
		for _, m := range c.models {
			if m.ID == *model && modelSupportsEffort(m, *effort) {
				ids = append(ids, c.id)
				break
			}
		}
	}
	if len(ids) == 0 {
		if errors.Is(err, errCatalogIncomplete) {
			writeCatalogError(w, err)
			return r, false
		}
		writeErrorCode(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			"请求的模型当前不可用，请通过 GET /v1/models 查询当前 API Key 可用的模型")
		return r, false
	}
	if errors.Is(err, errCatalogIncomplete) {
		confirmed := make(map[string]bool, len(catalogs))
		for _, c := range catalogs {
			confirmed[c.id] = true
		}
		unconfirmed := make(map[string]bool)
		selected := strings.TrimSpace(r.Header.Get(HeaderAccount))
		for _, a := range h.runner.pool.Accounts() {
			cred := a.Credential()
			if !confirmed[a.ID] && account.Allowed(r.Context(), a.ID) &&
				(selected == "" || selected == a.ID) && a.IsEnabled() &&
				cred != nil && cred.Usable() && a.CooldownRemaining(time.Now()) == 0 {
				unconfirmed[a.ID] = true
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), unconfirmedCatalogKey{}, unconfirmed))
	}
	r = r.WithContext(account.WithScope(r.Context(), account.Scope{Restricted: true, AccountIDs: ids}))
	middleware.RecordLogModel(r, *model)
	return r, true
}

// Catalog outages must not discard a native conversation and seed its history
// on a different account. Explicit account changes remain the caller's choice.
func verifyCatalogContinuity(w http.ResponseWriter, r *http.Request, req *RunRequest) bool {
	if req.AccountID != "" && req.AccountID != req.BoundAccountID {
		return true
	}
	unconfirmed, _ := r.Context().Value(unconfirmedCatalogKey{}).(map[string]bool)
	if unconfirmed[req.BoundAccountID] {
		writeCatalogError(w, errCatalogUnavailable)
		return false
	}
	return true
}

func modelSupportsEffort(m prism.UpstreamModel, effort string) bool {
	if effort == "" || len(m.Efforts) == 0 {
		return true
	}
	for _, e := range m.Efforts {
		if e == effort {
			return true
		}
	}
	return false
}

func (h *Handler) catalogDefaultEffort(m prism.UpstreamModel) string {
	if m.DefaultEffort != "" && modelSupportsEffort(m, m.DefaultEffort) {
		return m.DefaultEffort
	}
	configured := h.cfg.Facade.Models[m.ID].ReasoningEffort
	if configured != "" && modelSupportsEffort(m, configured) {
		return configured
	}
	if len(m.Efforts) > 0 {
		return m.Efforts[0]
	}
	return ""
}
