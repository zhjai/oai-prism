package metrics

import "time"

// App 汇总本服务用到的全部指标句柄。
//
// 集中定义的好处：指标名只在编译期出现一次，
// 各包通过注入使用，不会出现"同一个含义两个名字"的漂移。
type App struct {
	Reg *Registry

	// HTTP 入站。
	HTTPRequests *CounterVec // path, method, code
	HTTPInflight *CounterVec // path
	HTTPDuration *HistogramVec

	// 上游出站。
	UpstreamRequests *CounterVec // path, code
	UpstreamDuration *HistogramVec
	UpstreamRetries  *CounterVec // path

	// 门面业务。
	FacadeRuns      *CounterVec // api, model, status
	FacadeLatency   *HistogramVec
	FacadeFirstByte *HistogramVec // 首字延迟——流式体验的核心指标
	SSEDeltas       *CounterVec   // api
	Tokens          *CounterVec   // api, direction

	// 上游协议轮询。
	PollRounds      *CounterVec // result
	PollEmpty       *CounterVec
	ProjectOps      *CounterVec // op, status
	SandboxOps      *CounterVec // op, status
	ConversationOps *CounterVec // op, status

	// 账号池。
	AccountPick *CounterVec // result

	// 其它。
	CaptureWritten *Counter
	ClientAborts   *Counter
	Panics         *Counter
	AuthFailed     *Counter
	RateLimited    *Counter
}

// NewApp 注册全部指标。
func NewApp() *App {
	r := New()
	return &App{
		Reg: r,

		HTTPRequests: r.Counter("oaiprism_http_requests_total", "入站 HTTP 请求数", "path", "method", "code"),
		HTTPInflight: r.GaugeVec("oaiprism_http_inflight_total", "当前入站请求并发数", "path"),
		HTTPDuration: r.Histogram("oaiprism_http_request_duration_seconds", "入站请求耗时", DefaultBuckets, "path", "method"),

		UpstreamRequests: r.Counter("oaiprism_upstream_requests_total", "上游请求数", "path", "code"),
		UpstreamDuration: r.Histogram("oaiprism_upstream_request_duration_seconds", "上游请求耗时", DefaultBuckets, "path"),
		UpstreamRetries:  r.Counter("oaiprism_upstream_retries_total", "上游重试次数", "path"),

		FacadeRuns:      r.Counter("oaiprism_facade_runs_total", "兼容门面调用数", "api", "model", "status"),
		FacadeLatency:   r.Histogram("oaiprism_facade_latency_seconds", "兼容门面端到端耗时", DefaultBuckets, "api"),
		FacadeFirstByte: r.Histogram("oaiprism_facade_first_delta_seconds", "首字延迟", []float64{0.1, 0.25, 0.5, 1, 2, 3, 5, 8, 15, 30, 60}, "api"),
		SSEDeltas:       r.Counter("oaiprism_sse_deltas_total", "SSE 增量事件数", "api"),
		Tokens:          r.Counter("oaiprism_tokens_total", "token 用量", "api", "direction"),

		PollRounds:      r.Counter("oaiprism_poll_rounds_total", "轮询轮次", "result"),
		PollEmpty:       r.Counter("oaiprism_poll_empty_total", "空轮询次数（无新内容的轮次）", "kind"),
		ProjectOps:      r.Counter("oaiprism_project_ops_total", "项目操作", "op", "status"),
		SandboxOps:      r.Counter("oaiprism_sandbox_ops_total", "沙箱操作", "op", "status"),
		ConversationOps: r.Counter("oaiprism_conversation_ops_total", "会话操作", "op", "status"),

		AccountPick: r.Counter("oaiprism_account_pick_total", "账号调度结果", "result"),

		CaptureWritten: r.Scalar("oaiprism_capture_records_total", "抓包记录条数"),
		ClientAborts:   r.Scalar("oaiprism_client_aborts_total", "客户端提前断开次数"),
		Panics:         r.Scalar("oaiprism_panics_total", "被恢复的 panic 次数"),
		AuthFailed:     r.Scalar("oaiprism_auth_failed_total", "API Key 鉴权失败次数"),
		RateLimited:    r.Scalar("oaiprism_rate_limited_total", "被限流请求数"),
	}
}

// ObserveUpstream 实现 prism.MetricsHook。
func (a *App) ObserveUpstream(path string, status int, d time.Duration, err error) {
	if a == nil || a.Reg == nil {
		return
	}
	code := status
	if err != nil && code == 0 {
		code = -1
	}
	a.UpstreamRequests.Inc(path, itoa(code))
	a.UpstreamDuration.Observe(d.Seconds(), path)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [12]byte
	p := len(buf)
	for i > 0 {
		p--
		buf[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		buf[p] = '-'
	}
	return string(buf[p:])
}
