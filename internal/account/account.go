package account

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/httpc"
	"golang.org/x/time/rate"
)

// Account 是账号池里的一个成员。
//
// 并发模型说明：本结构是"热点读写"结构——每个请求都要读 inflight / cooldown，
// 因此这些字段全部用 atomic；只有限流桶用了 mutex（临界区仅几纳秒，
// 且只在账号显式配置了速率限制时才存在）。
type Account struct {
	ID     string
	Name   string
	Tags   []string
	Weight int
	plan   string // Local plan label; refreshed upstream claims remain in the credential.

	// Client 是该账号专属的上游客户端（可能走独立出口代理）。
	Client *httpc.Client
	*accountRuntime
}

// Leases and refreshes keep sharing state when configuration snapshots change.
type accountRuntime struct {
	enabled atomic.Bool
	maxConc atomic.Int64

	// cred 是不可变快照，刷新时整体替换。用 atomic.Pointer 实现无锁读。
	cred atomic.Pointer[creds.Credential]

	// 并发闸门。
	inflight atomic.Int64

	// 限流器，nil 表示不限流。使用官方标准库 rate.Limiter。
	limiterMu sync.Mutex
	limiter   *rate.Limiter

	// 冷却：命中 401/429 后进入冷却，调度器跳过。
	cooldownUntil atomic.Int64 // unix nano
	failStreak    atomic.Int64

	// authFailed 标记"这次冷却是因为凭据失效（401/403）而不是被限流（429）"。
	//
	// 这个区分很关键：限流等一会儿就好，值得等；凭据失效等多久都没用，
	// 应该立刻报错让人去换凭据。少了这个标记，一个配错的 token
	// 会让每个请求都空等一整个冷却周期。
	authFailed atomic.Bool

	// 统计。
	total        atomic.Int64
	failures     atomic.Int64
	lastUsedUnix atomic.Int64

	// refreshing 保证同一账号同一时刻只有一次刷新在飞。
	refreshMu   sync.Mutex
	refreshCall *refreshCall

	// Refresh scheduling state is kept with the runtime so metadata-only
	// configuration reloads do not reset an in-flight refresh lease or its
	// backoff. Pointers identify immutable credential snapshots.
	refreshFailureCred  *creds.Credential
	refreshFailureUntil time.Time
	refreshFailureCount int
	refreshSuccessCred  *creds.Credential
	refreshSuccessUntil time.Time
}

type refreshCall struct {
	done       chan struct{}
	credential *creds.Credential
	err        error
}

// Credential 返回当前凭据快照（无锁）。
func (a *Account) Credential() *creds.Credential { return a.cred.Load() }

// StoreCredential 原子替换凭据。
func (a *Account) StoreCredential(c *creds.Credential) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	old := a.cred.Swap(c)
	if credentialMaterialChanged(old, c) {
		a.refreshFailureCred = nil
		a.refreshFailureUntil = time.Time{}
		a.refreshFailureCount = 0
		a.refreshSuccessCred = nil
		a.refreshSuccessUntil = time.Time{}
		return
	}
	// A metadata-only reload replaces the immutable snapshot pointer. Carry
	// scheduling state forward so it still applies to the equivalent material.
	if a.refreshFailureCred == old {
		a.refreshFailureCred = c
	}
	if a.refreshSuccessCred == old {
		a.refreshSuccessCred = c
	}
}

func credentialMaterialChanged(old, next *creds.Credential) bool {
	if old == nil || next == nil {
		return old != next
	}
	return old.AccessToken != next.AccessToken ||
		old.RefreshToken != next.RefreshToken ||
		old.SessionToken != next.SessionToken ||
		old.SessionCookieName != next.SessionCookieName ||
		old.Headers["oauth_client_id"] != next.Headers["oauth_client_id"] ||
		credentialIdentityCookies(old.CookieHeader) != credentialIdentityCookies(next.CookieHeader)
}

func credentialIdentityCookies(header string) string {
	var result strings.Builder
	for _, name := range []string{creds.CookiePrismAccessToken, creds.CookiePrismRefreshToken, creds.CookiePrismSessionToken, creds.CookieSessionToken, creds.CookieSessionTokenLoose, creds.CookieAuthSession} {
		result.WriteString(name)
		result.WriteByte('=')
		result.WriteString(creds.CookieValue(header, name))
		result.WriteByte(';')
	}
	return result.String()
}

// mergeRefreshCredential preserves operator edits while applying refresh changes
// to fields that still match the snapshot used by the upstream request.
func mergeRefreshCredential(current, expected, next *creds.Credential) *creds.Credential {
	merged := current.Clone()
	merged.AccessToken, merged.RefreshToken = next.AccessToken, next.RefreshToken
	merged.Source, merged.UpdatedAt = next.Source, next.UpdatedAt
	for _, field := range []struct {
		dst        *string
		old, value string
	}{
		{&merged.SessionToken, expected.SessionToken, next.SessionToken},
		{&merged.SessionCookieName, expected.SessionCookieName, next.SessionCookieName},
		{&merged.AccountID, expected.AccountID, next.AccountID},
		{&merged.Email, expected.Email, next.Email},
		{&merged.Plan, expected.Plan, next.Plan},
		{&merged.UserID, expected.UserID, next.UserID},
	} {
		if *field.dst == field.old {
			*field.dst = field.value
		}
	}
	if current.ExpiresAt.Equal(expected.ExpiresAt) {
		merged.ExpiresAt = next.ExpiresAt
	}
	for key, old := range expected.Headers {
		if value, ok := merged.Headers[key]; ok && value == old {
			if value, ok := next.Headers[key]; ok {
				merged.Headers[key] = value
			} else {
				delete(merged.Headers, key)
			}
		}
	}
	for key, value := range next.Headers {
		if _, existed := expected.Headers[key]; existed {
			continue
		}
		if _, exists := merged.Headers[key]; !exists {
			if merged.Headers == nil {
				merged.Headers = make(map[string]string)
			}
			merged.Headers[key] = value
		}
	}
	if canonicalCredentialCookies(current.CookieHeader) == canonicalCredentialCookies(expected.CookieHeader) {
		merged.CookieHeader = next.CookieHeader
	} else {
		request := &http.Request{Header: http.Header{"Cookie": []string{next.CookieHeader}}}
		for _, cookie := range request.Cookies() {
			old := creds.CookieValue(expected.CookieHeader, cookie.Name)
			if cookie.Value != old && creds.CookieValue(current.CookieHeader, cookie.Name) == old {
				merged.CookieHeader = creds.MergeCookie(merged.CookieHeader, cookie)
			}
		}
	}
	return merged
}

func (a *Account) refreshBlocked(c *creds.Credential, now time.Time) bool {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	if a.refreshFailureCred == c && now.Before(a.refreshFailureUntil) {
		return true
	}
	return a.refreshSuccessCred == c && now.Before(a.refreshSuccessUntil)
}

func (a *Account) refreshRetryAt(c *creds.Credential) (time.Time, bool) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	if a.refreshFailureCred != c || a.refreshFailureUntil.IsZero() {
		return time.Time{}, false
	}
	return a.refreshFailureUntil, true
}

func (a *Account) refreshSuccessAt(c *creds.Credential) (time.Time, bool) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	if a.refreshSuccessCred != c || a.refreshSuccessUntil.IsZero() {
		return time.Time{}, false
	}
	return a.refreshSuccessUntil, true
}

func (a *Account) noteRefreshFailure(c *creds.Credential, now time.Time, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	if a.Credential() != c {
		return
	}
	if a.refreshFailureCred != c {
		a.refreshFailureCred = c
		a.refreshFailureCount = 0
	}
	a.refreshFailureCount++
	delay := time.Minute
	for i := 1; i < a.refreshFailureCount && delay < interval; i++ {
		delay *= 2
	}
	if delay > interval {
		delay = interval
	}
	a.refreshFailureUntil = now.Add(delay)
	a.refreshSuccessCred = nil
	a.refreshSuccessUntil = time.Time{}
}

func (a *Account) noteRefreshSuccess(c *creds.Credential, now time.Time, interval, skew time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	a.refreshMu.Lock()
	if a.Credential() != c {
		a.refreshMu.Unlock()
		return
	}
	a.refreshFailureCred = nil
	a.refreshFailureUntil = time.Time{}
	a.refreshFailureCount = 0
	a.refreshSuccessCred = c
	delay := interval
	if c.Usable() && !c.Expired(now) {
		if !c.NeedsRefresh(now, skew) && delay > 30*time.Second {
			delay = 30 * time.Second
		}
		if remaining := c.ExpiresAt.Sub(now); remaining > 0 && remaining/2 < delay {
			delay = remaining / 2
		}
	}
	a.refreshSuccessUntil = now.Add(delay)
	a.refreshMu.Unlock()
}

// Inflight 当前在途请求数。
func (a *Account) Inflight() int64 { return a.inflight.Load() }

// MaxConcurrency 并发上限，0 表示不限。
func (a *Account) MaxConcurrency() int64 { return a.maxConc.Load() }

// Available 判断账号当前是否可被调度。
// Busy 报告账号是否"仅因并发已满而暂不可用"（未冷却且凭据可用）。
func (a *Account) Busy(now time.Time) bool {
	if !a.enabled.Load() {
		return false
	}
	limit := a.maxConc.Load()
	if limit <= 0 || a.inflight.Load() < limit {
		return false
	}
	if until := a.cooldownUntil.Load(); until > 0 && now.UnixNano() < until {
		return false
	}
	return a.cred.Load().Usable()
}

func (a *Account) Available(now time.Time) bool {
	if !a.enabled.Load() {
		return false
	}
	limit := a.maxConc.Load()
	if limit > 0 && a.inflight.Load() >= limit {
		return false
	}
	if until := a.cooldownUntil.Load(); until > 0 && now.UnixNano() < until {
		return false
	}
	return a.cred.Load().Usable()
}

// CooldownRemaining 返回剩余冷却时间。
func (a *Account) CooldownRemaining(now time.Time) time.Duration {
	until := a.cooldownUntil.Load()
	if until <= 0 {
		return 0
	}
	d := time.Duration(until - now.UnixNano())
	if d < 0 {
		return 0
	}
	return d
}

// Acquire 占用一个并发额度并消耗一个限流令牌。
// 返回 false 表示此刻不可用（调用方应换号或退避）。
func (a *Account) Acquire(now time.Time) bool {
	if !a.Available(now) {
		return false
	}
	if a.maxConc.Load() > 0 {
		for {
			cur := a.inflight.Load()
			limit := a.maxConc.Load()
			if limit > 0 && cur >= limit {
				return false
			}
			if a.inflight.CompareAndSwap(cur, cur+1) {
				break
			}
		}
	} else {
		a.inflight.Add(1)
	}
	a.limiterMu.Lock()
	allowed := a.limiter == nil || a.limiter.AllowN(now, 1)
	a.limiterMu.Unlock()
	if !allowed {
		a.inflight.Add(-1)
		return false
	}
	a.lastUsedUnix.Store(now.UnixNano())
	a.total.Add(1)
	return true
}

// Release 归还并发额度。
func (a *Account) Release() {
	// CAS 递减：先 Load 再 Add 的写法在并发释放时会把计数减成负数。
	for {
		cur := a.inflight.Load()
		if cur <= 0 || a.inflight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// MarkSuccess 清零失败计数并解除冷却。
func (a *Account) MarkSuccess() {
	a.failStreak.Store(0)
	a.cooldownUntil.Store(0)
	a.authFailed.Store(false)
}

// MarkAuthFailed 记录"凭据失效"。
func (a *Account) MarkAuthFailed() { a.authFailed.Store(true) }

// AuthFailed 报告该账号当前是否处于"凭据失效"状态。
func (a *Account) AuthFailed() bool { return a.authFailed.Load() }

// MarkFailure 记录一次失败并进入指数冷却。
func (a *Account) MarkFailure(now time.Time, base time.Duration, backoff float64, max time.Duration) time.Duration {
	a.failures.Add(1)
	streak := a.failStreak.Add(1)

	d := base
	if backoff > 1 {
		for i := int64(1); i < streak && d < max; i++ {
			d = time.Duration(float64(d) * backoff)
		}
	}
	if max > 0 && d > max {
		d = max
	}
	a.cooldownUntil.Store(now.Add(d).UnixNano())
	return d
}

// Cooldown 显式设置冷却时长（例如收到 Retry-After）。
func (a *Account) Cooldown(now time.Time, d time.Duration) {
	if d <= 0 {
		return
	}
	a.cooldownUntil.Store(now.Add(d).UnixNano())
}

// Stats 是账号的运行态快照，供 /admin 输出。
type Stats struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Enabled      bool     `json:"enabled"`
	Available    bool     `json:"available"`
	Plan         string   `json:"plan"`
	Email        string   `json:"email"`
	HasToken     bool     `json:"has_access_token"`
	HasRefresh   bool     `json:"has_refresh_token"`
	HasSession   bool     `json:"has_session"`
	TokenExpires string   `json:"token_expires,omitempty"`
	ExpiresInSec int64    `json:"expires_in_sec,omitempty"`
	Inflight     int64    `json:"inflight"`
	MaxConcur    int64    `json:"max_concurrency"`
	CooldownSec  float64  `json:"cooldown_sec"`
	FailStreak   int64    `json:"fail_streak"`
	Total        int64    `json:"total_requests"`
	Failures     int64    `json:"failures"`
	LastUsed     string   `json:"last_used,omitempty"`
	Source       string   `json:"source"`
	Tags         []string `json:"tags,omitempty"`
}

// Stats 导出运行态。
func (a *Account) Stats(now time.Time) Stats {
	c := a.cred.Load()
	s := Stats{
		ID:         a.ID,
		Name:       a.Name,
		Plan:       a.plan,
		Enabled:    a.enabled.Load(),
		Available:  a.Available(now),
		Inflight:   a.inflight.Load(),
		MaxConcur:  a.maxConc.Load(),
		FailStreak: a.failStreak.Load(),
		Total:      a.total.Load(),
		Failures:   a.failures.Load(),
		Tags:       a.Tags,
	}
	if d := a.CooldownRemaining(now); d > 0 {
		s.CooldownSec = d.Seconds()
	}
	if c != nil {
		if s.Plan == "" {
			s.Plan = c.Plan
		}
		s.Email = c.Email
		s.Source = c.Source
		s.HasToken = c.AccessToken != ""
		s.HasRefresh = c.RefreshToken != ""
		s.HasSession = c.SessionToken != "" || creds.HasSessionCookie(c.CookieHeader)
		if !c.ExpiresAt.IsZero() {
			s.TokenExpires = c.ExpiresAt.UTC().Format(time.RFC3339)
			s.ExpiresInSec = int64(c.ExpiresAt.Sub(now).Seconds())
		}
	}
	if u := a.lastUsedUnix.Load(); u > 0 {
		s.LastUsed = time.Unix(0, u).UTC().Format(time.RFC3339)
	}
	return s
}

// newAccount 从配置构造账号。client 由调用方注入（可能带独立代理）。
func newAccount(cfg config.AccountConfig, client *httpc.Client, c *creds.Credential) *Account {
	a := &Account{
		ID:             cfg.ID,
		Name:           cfg.Name,
		Tags:           cfg.Tags,
		Weight:         cfg.Weight,
		plan:           cfg.Plan,
		Client:         client,
		accountRuntime: &accountRuntime{},
	}
	if a.Name == "" {
		a.Name = a.ID
	}
	if a.Weight <= 0 {
		a.Weight = 1
	}
	if cfg.RatePerSecond > 0 {
		burst := cfg.RateBurst
		if burst <= 0 {
			burst = int(cfg.RatePerSecond)
			if burst < 1 {
				burst = 1
			}
		}
		a.limiter = rate.NewLimiter(rate.Limit(cfg.RatePerSecond), burst)
	}
	a.cred.Store(c)
	a.maxConc.Store(int64(cfg.MaxConcurrency))
	a.enabled.Store(cfg.IsEnabled())
	return a
}

func (a *Account) configureLimiter(cfg config.AccountConfig) {
	a.limiterMu.Lock()
	defer a.limiterMu.Unlock()
	if cfg.RatePerSecond <= 0 {
		a.limiter = nil
		return
	}
	burst := cfg.RateBurst
	if burst <= 0 {
		burst = max(1, int(cfg.RatePerSecond))
	}
	if a.limiter == nil {
		a.limiter = rate.NewLimiter(rate.Limit(cfg.RatePerSecond), burst)
	} else {
		a.limiter.SetLimit(rate.Limit(cfg.RatePerSecond))
		a.limiter.SetBurst(burst)
	}
}

// Fingerprint 是用于负载均衡的稳定键。
func (a *Account) Fingerprint() string {
	c := a.cred.Load()
	if c != nil && c.AccountID != "" {
		return c.AccountID
	}
	return a.ID
}

// ParseAccountID 把各种形态的账号标识归一化。
func ParseAccountID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if _, err := strconv.Atoi(raw); err == nil {
		return raw
	}
	return raw
}
