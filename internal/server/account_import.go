package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/httpc"
)

func (s *Server) handleAccountImport(w http.ResponseWriter, r *http.Request) {
	if s.sqlite == nil {
		writeAdminErr(w, http.StatusServiceUnavailable, "SQLite 账号存储不可用")
		return
	}
	var body struct {
		Accounts []json.RawMessage `json:"accounts"`
		Verify   *bool             `json:"verify"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, min(s.cfg.Server.MaxBodyBytes, 4<<20))
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&body); err != nil {
		writeAdminErr(w, http.StatusBadRequest, "导入数据不是有效的 JSON 或超过大小限制")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeAdminErr(w, http.StatusBadRequest, "请求只能包含一份 JSON 数据")
		return
	}
	if len(body.Accounts) == 0 || len(body.Accounts) > 100 {
		writeAdminErr(w, http.StatusBadRequest, "每次需导入 1 至 100 个账号")
		return
	}
	verify := body.Verify == nil || *body.Verify
	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	defer cancel()
	batch := make([]config.AccountConfig, 0, len(body.Accounts))
	for i, raw := range body.Accounts {
		a := config.AccountConfig{MaxConcurrency: 2}
		if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(string(raw)) == "null" {
			writeAdminErr(w, http.StatusBadRequest, fmt.Sprintf("第 %d 个账号格式无效", i+1))
			return
		}
		if a.MaxConcurrency < 0 || a.RatePerSecond < 0 || a.RateBurst < 0 || a.Weight < 0 {
			writeAdminErr(w, http.StatusBadRequest, "并发数、速率、突发量和权重不能为负数")
			return
		}
		c := creds.FromAccountConfig(a)
		if !c.Usable() && !c.CanRefresh() {
			writeAdminErr(w, http.StatusBadRequest, fmt.Sprintf("第 %d 个账号缺少 Cookie 或 Token", i+1))
			return
		}
		client, err := httpc.New(s.cfg.Upstream, httpc.Options{Proxy: a.Proxy})
		if err != nil {
			writeAdminErr(w, http.StatusBadRequest, "账号代理地址无效")
			return
		}
		if verify {
			refresher, _ := creds.NewRefresher(s.cfg.Creds, s.cfg.Upstream, client)
			verifyCtx, stop := context.WithTimeout(ctx, 30*time.Second)
			c, err = refresher.Refresh(verifyCtx, c)
			stop()
			if err != nil {
				client.CloseIdle()
				writeAdminErr(w, http.StatusBadGateway, importCredentialError(i+1, err))
				return
			}
		}
		client.CloseIdle()
		a.AccessToken, a.RefreshToken, a.SessionToken = c.AccessToken, c.RefreshToken, c.SessionToken
		if c.CookieHeader != "" {
			a.Cookies, a.CookieMap = c.CookieHeader, nil
		}
		a.Email, a.AccountID = c.Email, c.AccountID
		if a.Plan == "" {
			a.Plan = c.Plan
		}
		if !c.ExpiresAt.IsZero() {
			expiry := c.ExpiresAt
			a.ExpiresAt = &expiry
		}
		batch = append(batch, a)
	}
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	stored, err := s.sqlite.Load()
	if err != nil {
		writeAdminErr(w, http.StatusInternalServerError, "读取账号存储失败")
		return
	}
	seen := make(map[string]bool, len(batch))
	existing := make(map[string]bool, len(stored))
	for _, a := range stored {
		existing[a.ID] = true
	}
	created, updated := 0, 0
	for i := range batch {
		id, err := account.ResolveImportID(batch[i], creds.FromAccountConfig(batch[i]), stored)
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "生成账号 ID 失败")
			return
		}
		if seen[id] {
			writeAdminErr(w, http.StatusBadRequest, "同一批次包含重复账号 ID 或登录身份")
			return
		}
		seen[id] = true
		batch[i].ID = id
		if existing[id] {
			updated++
		} else {
			created++
		}
		if batch[i].Name == "" {
			batch[i].Name = batch[i].Email
			if batch[i].Name == "" {
				batch[i].Name = id
			}
		}
	}
	if err := s.sqlite.SaveAccounts(batch); err != nil {
		writeAdminErr(w, http.StatusInternalServerError, "保存账号失败")
		return
	}
	if err := s.syncPoolFromSQLiteLocked(); err != nil {
		writeAdminErr(w, http.StatusInternalServerError, "账号已保存，但调度池重建失败")
		return
	}
	items := make([]map[string]string, 0, len(batch))
	for _, a := range batch {
		items = append(items, map[string]string{"id": a.ID, "name": a.Name, "email": a.Email, "plan": a.Plan})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "created": created, "updated": updated, "total": s.pool.Size(), "accounts": items})
}

func importCredentialError(index int, err error) string {
	var apiErr *creds.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Status == http.StatusUnauthorized {
			return fmt.Sprintf("第 %d 个账号登录凭据已失效或被撤销，请重新登录并复制最新 Cookie；订阅等级不会延长登录有效期", index)
		}
		if apiErr.Status == http.StatusForbidden {
			return fmt.Sprintf("第 %d 个账号校验被上游拒绝（HTTP 403），请检查代理出口和最新 Cookie", index)
		}
		return fmt.Sprintf("第 %d 个账号校验失败（上游 HTTP %d）", index, apiErr.Status)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Sprintf("第 %d 个账号校验超时或已取消", index)
	}
	return fmt.Sprintf("第 %d 个账号校验失败，请检查网络、代理和登录凭据", index)
}
