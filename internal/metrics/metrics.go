// Package metrics 是一个零依赖的 Prometheus 文本格式指标库。
//
// 为什么不直接引 prometheus/client_golang：
// 该库会带入 4~5 个间接依赖、注册全局状态、并在每次 WithLabelValues 时
// 做一次 map 查找加锁。我们只需要十几个指标，自己实现反而更小更快，
// 也让"反代服务的依赖面"保持在一个可以人工审计的规模内。
package metrics

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Counter 是单值单调递增计数器。
type Counter struct{ v atomic.Int64 }

// Inc 加一。
func (c *Counter) Inc() { c.v.Add(1) }

// Add 加 n。
func (c *Counter) Add(n int64) { c.v.Add(n) }

// Value 当前值。
func (c *Counter) Value() int64 { return c.v.Load() }

// CounterVec 带标签的计数器。
type CounterVec struct {
	name   string
	help   string
	labels []string
	series sync.Map // key: 拼接后的标签值 -> *Counter
	kind   string
}

// With 取（或创建）一条时间序列。
func (v *CounterVec) With(labelValues ...string) *Counter {
	c, ok := v.series.Load(key(labelValues))
	if ok {
		return c.(*Counter)
	}
	nc := &Counter{}
	actual, _ := v.series.LoadOrStore(key(labelValues), nc)
	return actual.(*Counter)
}

// Inc 便捷方法。
func (v *CounterVec) Inc(labelValues ...string) { v.With(labelValues...).Inc() }

// HistogramVec 是固定桶的直方图。
type HistogramVec struct {
	name    string
	help    string
	labels  []string
	buckets []float64
	series  sync.Map // -> *histogramSeries
}

type histogramSeries struct {
	counts []atomic.Int64 // len = len(buckets)+1（最后是 +Inf）
	sumNs  atomic.Int64
	count  atomic.Int64
}

// Observe 记录一次观测。
//
// 语义要点：一次观测只增加"它落进去的那个最小桶"的计数，
// 而不是所有大于它的桶。累积和是在 Render 时算的。
//
// 如果这里写成"对所有 seconds <= b 的桶都 +1"，再在 Render 里做累积，
// 同一次观测就会被重复计数，分位数会系统性偏大——
// 这是一个很难靠肉眼发现的错误，因为指标看起来"有数"。
func (h *HistogramVec) Observe(seconds float64, labelValues ...string) {
	s := h.seriesFor(labelValues)
	for i, b := range h.buckets {
		if seconds <= b {
			s.counts[i].Add(1)
			s.sumNs.Add(int64(seconds * float64(time.Second)))
			s.count.Add(1)
			return
		}
	}
	// 超出所有显式桶 -> 只进 +Inf。
	s.counts[len(h.buckets)].Add(1)
	s.sumNs.Add(int64(seconds * float64(time.Second)))
	s.count.Add(1)
}

func (h *HistogramVec) seriesFor(labelValues []string) *histogramSeries {
	c, ok := h.series.Load(key(labelValues))
	if ok {
		return c.(*histogramSeries)
	}
	ns := &histogramSeries{counts: make([]atomic.Int64, len(h.buckets)+1)}
	actual, _ := h.series.LoadOrStore(key(labelValues), ns)
	return actual.(*histogramSeries)
}

// GaugeSample 是一次采样结果。
type GaugeSample struct {
	Labels []string
	Value  float64
}

// GaugeFunc 是采集时才求值的仪表盘指标。
//
// 这类指标（账号在途数、goroutine 数）在请求路径上维护会污染热路径，
// 放到抓取时按需计算更划算。
type GaugeFunc struct {
	name   string
	help   string
	labels []string
	fn     func() []GaugeSample
}

// Registry 汇总所有指标。
type Registry struct {
	mu         sync.RWMutex
	startedAt  time.Time
	counters   []*CounterVec
	histograms []*HistogramVec
	gauges     []*GaugeFunc

	// 单值计数器，用于不需要标签的全局量。
	scalars map[string]*scalarEntry
}

type scalarEntry struct {
	help string
	kind string // counter | gauge
	c    *Counter
}

// New 构造注册表。
func New() *Registry {
	return &Registry{
		startedAt: time.Now(),
		scalars:   make(map[string]*scalarEntry, 32),
	}
}

// Counter 定义带标签的计数器。
func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := &CounterVec{name: name, help: help, labels: labels, kind: "counter"}
	r.counters = append(r.counters, v)
	return v
}

// GaugeVec stores current values using the same atomic labeled series API.
func (r *Registry) GaugeVec(name, help string, labels ...string) *CounterVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := &CounterVec{name: name, help: help, labels: labels, kind: "gauge"}
	r.counters = append(r.counters, v)
	return v
}

// Histogram 定义带标签的直方图。
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	if len(buckets) == 0 {
		buckets = DefaultBuckets
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	h := &HistogramVec{name: name, help: help, labels: labels, buckets: buckets}
	r.histograms = append(r.histograms, h)
	return h
}

// Gauge 定义采集时求值的仪表盘。
func (r *Registry) Gauge(name, help string, fn func() []GaugeSample, labels ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges = append(r.gauges, &GaugeFunc{name: name, help: help, labels: labels, fn: fn})
}

// Scalar 定义无标签的计数器/仪表盘。
func (r *Registry) Scalar(name, help string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := &Counter{}
	r.scalars[name] = &scalarEntry{help: help, kind: "counter", c: c}
	return c
}

// DefaultBuckets 面向 HTTP 延迟，单位秒。
var DefaultBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120,
}

// Render 以 Prometheus 文本格式输出。
//
// 刻意不叫 WriteTo：那个签名会被 go vet 误判为想实现 io.WriterTo，
// 而我们要的是 (error) 而不是 (int64, error)。
func (r *Registry) Render(w io.Writer) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var b strings.Builder
	b.Grow(16 << 10)
	now := time.Now()

	// 进程级指标。
	b.WriteString("# HELP oaiprism_uptime_seconds 进程运行时长\n")
	b.WriteString("# TYPE oaiprism_uptime_seconds gauge\n")
	b.WriteString("oaiprism_uptime_seconds ")
	b.WriteString(strconv.FormatFloat(now.Sub(r.startedAt).Seconds(), 'f', 3, 64))
	b.WriteString("\n")

	b.WriteString("# HELP oaiprism_goroutines 当前 goroutine 数\n")
	b.WriteString("# TYPE oaiprism_goroutines gauge\n")
	b.WriteString("oaiprism_goroutines ")
	b.WriteString(strconv.Itoa(runtime.NumGoroutine()))
	b.WriteString("\n")

	// 内存：只在抓取时读，避免把 ReadMemStats 放进热路径（它会 STW）。
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	writeSimple(&b, "oaiprism_memory_alloc_bytes", "已分配堆内存", float64(ms.Alloc))
	writeSimple(&b, "oaiprism_memory_sys_bytes", "向系统申请的内存", float64(ms.Sys))
	writeSimple(&b, "oaiprism_gc_cycles_total", "GC 轮次", float64(ms.NumGC))

	// 标量。
	names := make([]string, 0, len(r.scalars))
	for k := range r.scalars {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		e := r.scalars[n]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", n, e.help, n, e.kind)
		fmt.Fprintf(&b, "%s %d\n", n, e.c.Value())
	}

	// 计数器。
	for _, cv := range r.counters {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", cv.name, cv.help, cv.name, cv.kind)
		cv.series.Range(func(k, v any) bool {
			fmt.Fprintf(&b, "%s%s %d\n", cv.name, formatLabels(cv.labels, k.(string)), v.(*Counter).Value())
			return true
		})
	}

	// 直方图。
	for _, hv := range r.histograms {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", hv.name, hv.help, hv.name)
		hv.series.Range(func(k, v any) bool {
			s := v.(*histogramSeries)
			lbl := k.(string)
			cumulative := int64(0)
			for i, ub := range hv.buckets {
				cumulative += s.counts[i].Load()
				fmt.Fprintf(&b, "%s_bucket%s %d\n", hv.name,
					formatLabels(hv.labels, lbl, strconv.FormatFloat(ub, 'g', -1, 64)), cumulative)
			}
			cumulative += s.counts[len(hv.buckets)].Load()
			fmt.Fprintf(&b, "%s_bucket%s %d\n", hv.name, formatLabels(hv.labels, lbl, "+Inf"), cumulative)
			fmt.Fprintf(&b, "%s_sum%s %s\n", hv.name, formatLabels(hv.labels, lbl),
				strconv.FormatFloat(float64(s.sumNs.Load())/float64(time.Second), 'f', 6, 64))
			fmt.Fprintf(&b, "%s_count%s %d\n", hv.name, formatLabels(hv.labels, lbl), s.count.Load())
			return true
		})
	}

	// 采集型仪表盘。
	for _, g := range r.gauges {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		for _, smp := range g.fn() {
			fmt.Fprintf(&b, "%s%s %s\n", g.name, formatLabels(g.labels, key(smp.Labels)),
				strconv.FormatFloat(smp.Value, 'f', -1, 64))
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func writeSimple(b *strings.Builder, name, help string, v float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n",
		name, help, name, name, strconv.FormatFloat(v, 'f', -1, 64))
}

// key 把标签值拼成 map key。
//
// 用 \x00 而不是逗号分隔：标签值里出现逗号是常见情况（如 URL path），
// 用逗号会产生键碰撞，进而把两个不同标签的序列合并，指标静默失真。
func key(values []string) string {
	if len(values) == 0 {
		return ""
	}
	if len(values) == 1 {
		return values[0]
	}
	var b strings.Builder
	for i, v := range values {
		if i > 0 {
			b.WriteByte(0)
		}
		b.WriteString(v)
	}
	return b.String()
}

// formatLabels 输出 Prometheus 标签串。
//
// le 非空时会追加一个 le="..." 标签（histogram bucket 需要）。
func formatLabels(names []string, rawKey string, le ...string) string {
	var parts []string
	if len(names) > 0 {
		values := splitKey(rawKey)
		for i, n := range names {
			if i >= len(values) {
				break
			}
			parts = append(parts, n+"="+quote(values[i]))
		}
	}
	if len(le) > 0 && le[0] != "" {
		parts = append(parts, "le="+quote(le[0]))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// splitKey 拆回标签值。key() 在只有一个标签时不做拼接，
// 因此这里对空串要单独处理，否则会得到 [""] 这样的伪标签值。
func splitKey(k string) []string {
	if k == "" {
		return nil
	}
	return strings.Split(k, "\x00")
}

func quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
