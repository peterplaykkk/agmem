// metrics.go：进程内 Prometheus 指标端点（GET /metrics，G31 / UC-010 A7）。
//
// 为什么手写而不引 github.com/prometheus/client_golang：
//   - 引入 client_golang 需
//     同步带进 client_model/common/procfs/protobuf 等一串依赖；收益只是
//     text/0.0.4 暴露格式；
//   - 暴露格式是稳定规范，手写可控、可测、零新依赖（go.mod 不变）。
//
// 标签取值全部来自代码内常量/枚举（method、route 模板、status、队列状态、GLM op），
// 绝不含用户输入（kb 名、query 文本、item_id、token、DSN）——避免高基数与敏感泄漏。
// 端点不要求登录 session（集群内 Prometheus 抓取），但同样经 securityHeaders 包装。
package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sre/agmem/internal/cleaner"
	"sre/agmem/internal/store"
	"sre/agmem/internal/wikisync"
)

// httpLatencyBuckets 秒级直方图桶（覆盖 5ms–10s）。
var httpLatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// queueStatuses 清洗队列 gauge 暴露的状态集合（UC-010 A7 要求 pending/embedded/
// cleaning/failed 四态恒在，值缺省 0，保证 `> 0` 告警表达式有序列可判）。
var queueStatuses = []store.TaskStatus{
	store.StatusPending, store.StatusEmbedded, store.StatusCleaning, store.StatusFailed,
}

type httpMetricKey struct{ Method, Route, Status string }
type latencyMetricKey struct{ Method, Route, Status string }
type glmMetricKey struct{ Op, Result string }

type latencyHistogram struct {
	Buckets []uint64 // 累积桶，与 httpLatencyBuckets 等长
	Count   uint64
	Sum     float64
}

// Metrics 进程内指标注册表（并发安全）。
type Metrics struct {
	mu           sync.Mutex
	httpRequests map[httpMetricKey]uint64
	httpLatency  map[latencyMetricKey]*latencyHistogram
	glmCalls     map[glmMetricKey]uint64
	queue        map[string]int64
	wiki         *wikisync.Status
	version      string
	startTime    time.Time
	// 清洗前缀缓存观测（S3）：累计命中/未命中 tokens；最近一次前缀体积、hash 与分层。
	cleanCacheHit       uint64
	cleanCacheMiss      uint64
	cleanPrefixTokens   int64
	cleanPrefixHash     string
	cleanPrefixL1Tokens int64
	cleanPrefixL2Tokens int64
	cleanPrefixDirDepth int64
	// /api/ask 进程内本地缓存口径：hit / miss 计数（result 为有界标签）。
	askCache map[string]uint64
}

// NewMetrics 创建注册表；version 为构建注入的 git short sha（空则 "dev"）。
func NewMetrics(version string) *Metrics {
	if version == "" {
		version = "dev"
	}
	return &Metrics{
		httpRequests: make(map[httpMetricKey]uint64),
		httpLatency:  make(map[latencyMetricKey]*latencyHistogram),
		glmCalls:     make(map[glmMetricKey]uint64),
		queue:        make(map[string]int64),
		askCache:     make(map[string]uint64),
		version:      version,
		startTime:    time.Now(),
	}
}

// ObserveCleanCache 记录一次带前缀清洗的观测（S3）：累计命中/未命中 tokens，并保存
// 最近一次的前缀体积与 hash（供 /metrics 暴露稳态命中率与当前轮前缀）。由
// cleaner.Worker.OnCleanStats 接线。
func (m *Metrics) ObserveCleanCache(st cleaner.CleanStats) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if st.CacheHitTokens > 0 {
		m.cleanCacheHit += uint64(st.CacheHitTokens)
	}
	if st.CacheMissTokens > 0 {
		m.cleanCacheMiss += uint64(st.CacheMissTokens)
	}
	m.cleanPrefixTokens = int64(st.PrefixTokens)
	m.cleanPrefixL1Tokens = int64(st.L1Tokens)
	m.cleanPrefixL2Tokens = int64(st.L2Tokens)
	m.cleanPrefixDirDepth = int64(st.DirDepth)
	if st.PrefixHash != "" {
		m.cleanPrefixHash = st.PrefixHash
	}
}

// ObserveHTTP 记录一次 HTTP 请求（route 为注册时的路由模板，非原始路径）。
// 计数与耗时都按 method/route/status 三维打点（status 精确码，基数有界）。
func (m *Metrics) ObserveHTTP(method, route string, status int, d time.Duration) {
	if m == nil {
		return
	}
	code := strconv.Itoa(status)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.httpRequests[httpMetricKey{method, route, code}]++
	key := latencyMetricKey{method, route, code}
	h := m.httpLatency[key]
	if h == nil {
		h = &latencyHistogram{Buckets: make([]uint64, len(httpLatencyBuckets))}
		m.httpLatency[key] = h
	}
	sec := d.Seconds()
	for i, ub := range httpLatencyBuckets {
		if sec <= ub {
			h.Buckets[i]++
		}
	}
	h.Count++
	h.Sum += sec
}

// ObserveGLM 记录一次 GLM（aigateway）HTTP 调用结果。
func (m *Metrics) ObserveGLM(op, result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.glmCalls[glmMetricKey{op, result}]++
}

// ObserveAskCache 记录一次 /api/ask 进程内本地缓存判定（result 仅 hit/miss，有界标签）。
// 缓存禁用（Cache==nil）时不打点——是否启用由启动日志/部署配置可见。
func (m *Metrics) ObserveAskCache(result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.askCache == nil {
		m.askCache = make(map[string]uint64)
	}
	m.askCache[result]++
}

// SetQueue 设置某状态的队列条目数（由周期采集器调用）。
func (m *Metrics) SetQueue(status string, n int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue[status] = n
}

// ObserveWikiSync 记录 wiki 同步的一轮状态快照（由 wikisync.Syncer.OnStatus 调用；
// 生产在 main 接线）。nil 接收者/未启用时安静返回——指标端点仍输出 enabled=0。
func (m *Metrics) ObserveWikiSync(st wikisync.Status) {
	if m == nil {
		return
	}
	s := st
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wiki = &s
}

// InstrumentHTTPClient 包装 http.Client 的 Transport，统计 GLM 调用与错误。
// op 由请求路径归类（chat/models/other），不记录 URL 原文（无高基数、无敏感）。
// 不改动 cleaner.LLM 接口——worker 对 AnchorCleaner/SourceCleaner 的类型断言
// 依赖具体 *cleaner.GLMClient，包装接口会静默降级。
func (m *Metrics) InstrumentHTTPClient(c *http.Client) *http.Client {
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	nc := *c
	inner := c.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	nc.Transport = &meteredTransport{m: m, inner: inner}
	return &nc
}

type meteredTransport struct {
	m     *Metrics
	inner http.RoundTripper
}

func (t *meteredTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	op := glmOp(r.URL.Path)
	resp, err := t.inner.RoundTrip(r)
	if err != nil {
		t.m.ObserveGLM(op, "transport_error")
		return resp, err
	}
	result := "ok"
	switch {
	case resp.StatusCode >= 500:
		result = "http_5xx"
	case resp.StatusCode >= 400:
		result = "http_4xx"
	}
	t.m.ObserveGLM(op, result)
	return resp, err
}

// glmOp 把 GLM 请求路径归类为有界 op 标签（未知路径归 other，不泄漏原文）。
func glmOp(path string) string {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return "chat"
	case strings.HasSuffix(path, "/models"):
		return "models"
	default:
		return "other"
	}
}

// MetricsRegistry 返回指标注册表（nil → 按 Cfg.Version 惰性创建）。
func (s *Server) MetricsRegistry() *Metrics {
	if s.Metrics == nil {
		s.Metrics = NewMetrics(s.Cfg.Version)
	}
	return s.Metrics
}

// MetricsHandler GET /metrics（无鉴权，集群内 Prometheus 抓取；仍走安全头）。
func (s *Server) MetricsHandler() http.Handler {
	return s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		s.MetricsRegistry().WritePrometheus(w)
	}))
}

// RootHandler 组装对外根 handler：GET /metrics → 指标端点，其余交给 fallback。
// 独立于 Routes() 的原因：webui 静态处理器只把 /api/* 透传给后端（web/embed.go），
// /metrics 不在 /api 前缀下，必须在 SPA 兜底之前注册，否则会被 index.html 壳吞掉。
func (s *Server) RootHandler(fallback http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", s.MetricsHandler())
	mux.Handle("/", fallback)
	return mux
}

// instrument 按注册路由模板打点（模板常量，无用户输入 → 标签基数有界）。
func (s *Server) instrument(route string, h http.Handler) http.Handler {
	m := s.MetricsRegistry()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		m.ObserveHTTP(r.Method, route, sw.status, time.Since(start))
	})
}

// statusWriter 捕获响应状态码（未显式 WriteHeader 时为 200）。
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(status int) {
	if !w.wrote {
		w.status = status
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
}

// queueStatter 清洗队列 gauge 数据源（*store.Store 满足；测试注入 fake 免 MySQL）。
type queueStatter interface {
	GetKB(ctx context.Context, name string) (*store.KB, error)
	ListItemsByStatusPage(ctx context.Context, kbID int64, status store.TaskStatus, page, pageSize int) (*store.PageResult, error)
}

func (s *Server) queueStatsSource() queueStatter {
	if s.QueueStats != nil {
		return s.QueueStats
	}
	if s.Store != nil {
		return s.Store
	}
	return nil
}

// RefreshQueueMetrics 采样一次清洗队列计数（失败只记日志，不阻断抓取——
// 指标端点本身不查库，抓取延迟与 DB 健康解耦）。
func (s *Server) RefreshQueueMetrics(ctx context.Context) {
	st := s.queueStatsSource()
	if st == nil {
		return
	}
	kb, err := st.GetKB(ctx, s.Cfg.KB)
	if err != nil {
		log.Printf("api: metrics queue: resolve kb %s: %v", s.Cfg.KB, err)
		return
	}
	for _, status := range queueStatuses {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		res, err := st.ListItemsByStatusPage(cctx, kb.ID, status, 1, 1)
		cancel()
		if err != nil {
			log.Printf("api: metrics queue %s: %v", status, err)
			continue
		}
		s.MetricsRegistry().SetQueue(string(status), res.Total)
	}
}

// RunQueueMetrics 周期采样（interval<=0 → 30s），直到 ctx 取消。
func (s *Server) RunQueueMetrics(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	s.RefreshQueueMetrics(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RefreshQueueMetrics(ctx)
		}
	}
}

// —— text/0.0.4 暴露格式 ——

// WritePrometheus 输出 Prometheus text exposition（确定性排序，便于测试与 diff）。
func (m *Metrics) WritePrometheus(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := &strings.Builder{}

	b.WriteString("# HELP agmem_build_info Build info; version label is the deployed git short sha.\n")
	b.WriteString("# TYPE agmem_build_info gauge\n")
	writeSample(b, "agmem_build_info", []string{labelPair("version", m.version)}, "1")

	// 重启告警（替代 ksm kube_pod_container_status_restarts_total，CI 集群未部署 ksm）：
	// 进程重启后 start_time 变化，用 changes(agmem_process_start_time_seconds[15m]) > 0
	// 或 uptime 回退判定。计数器无法跨重启保留，故用启动时刻而非累计次数。
	b.WriteString("# HELP agmem_process_start_time_seconds Start time of the process since unix epoch in seconds.\n")
	b.WriteString("# TYPE agmem_process_start_time_seconds gauge\n")
	writeSample(b, "agmem_process_start_time_seconds", nil, formatMetricFloat(float64(m.startTime.Unix())))
	b.WriteString("# HELP agmem_process_uptime_seconds Seconds since process start.\n")
	b.WriteString("# TYPE agmem_process_uptime_seconds gauge\n")
	writeSample(b, "agmem_process_uptime_seconds", nil, formatMetricFloat(time.Since(m.startTime).Seconds()))

	b.WriteString("# HELP agmem_clean_queue_items Clean pipeline items by status (periodic store sample).\n")
	b.WriteString("# TYPE agmem_clean_queue_items gauge\n")
	for _, status := range queueStatuses {
		writeSample(b, "agmem_clean_queue_items",
			[]string{labelPair("status", string(status))}, strconv.FormatInt(m.queue[string(status)], 10))
	}

	b.WriteString("# HELP agmem_clean_cache_hit_tokens_total Cumulative prompt tokens served from the cache (DeepSeek prefix cache).\n")
	b.WriteString("# TYPE agmem_clean_cache_hit_tokens_total counter\n")
	writeSample(b, "agmem_clean_cache_hit_tokens_total", nil, strconv.FormatUint(m.cleanCacheHit, 10))
	b.WriteString("# HELP agmem_clean_cache_miss_tokens_total Cumulative prompt tokens that missed the cache.\n")
	b.WriteString("# TYPE agmem_clean_cache_miss_tokens_total counter\n")
	writeSample(b, "agmem_clean_cache_miss_tokens_total", nil, strconv.FormatUint(m.cleanCacheMiss, 10))
	b.WriteString("# HELP agmem_clean_prefix_tokens Estimated tokens of the most recent cleaning prefix.\n")
	b.WriteString("# TYPE agmem_clean_prefix_tokens gauge\n")
	writeSample(b, "agmem_clean_prefix_tokens", nil, strconv.FormatInt(m.cleanPrefixTokens, 10))
	b.WriteString("# HELP agmem_clean_prefix_l1_tokens L1 segment (L0 contract + global knowledge) tokens of the most recent cleaning prefix.\n")
	b.WriteString("# TYPE agmem_clean_prefix_l1_tokens gauge\n")
	writeSample(b, "agmem_clean_prefix_l1_tokens", nil, strconv.FormatInt(m.cleanPrefixL1Tokens, 10))
	b.WriteString("# HELP agmem_clean_prefix_l2_tokens L2 segment (directory context: target dir + ancestor chain) tokens of the most recent cleaning prefix.\n")
	b.WriteString("# TYPE agmem_clean_prefix_l2_tokens gauge\n")
	writeSample(b, "agmem_clean_prefix_l2_tokens", nil, strconv.FormatInt(m.cleanPrefixL2Tokens, 10))
	b.WriteString("# HELP agmem_clean_prefix_dir_depth Directories actually packed into the L2 context (target dir counts as 1; ascent is budget-bounded, not capped).\n")
	b.WriteString("# TYPE agmem_clean_prefix_dir_depth gauge\n")
	writeSample(b, "agmem_clean_prefix_dir_depth", nil, strconv.FormatInt(m.cleanPrefixDirDepth, 10))
	b.WriteString("# HELP agmem_clean_prefix_hash Current cleaning prefix hash (info-style; value always 1).\n")
	b.WriteString("# TYPE agmem_clean_prefix_hash gauge\n")
	writeSample(b, "agmem_clean_prefix_hash", []string{labelPair("hash", m.cleanPrefixHash)}, "1")

	b.WriteString("# HELP agmem_glm_calls_total GLM (aigateway) HTTP calls by operation and result.\n")
	b.WriteString("# TYPE agmem_glm_calls_total counter\n")
	glmKeys := make([]glmMetricKey, 0, len(m.glmCalls))
	for k := range m.glmCalls {
		glmKeys = append(glmKeys, k)
	}
	sort.Slice(glmKeys, func(i, j int) bool {
		if glmKeys[i].Op != glmKeys[j].Op {
			return glmKeys[i].Op < glmKeys[j].Op
		}
		return glmKeys[i].Result < glmKeys[j].Result
	})
	for _, k := range glmKeys {
		writeSample(b, "agmem_glm_calls_total",
			[]string{labelPair("op", k.Op), labelPair("result", k.Result)},
			strconv.FormatUint(m.glmCalls[k], 10))
	}
	b.WriteString("# HELP agmem_glm_errors_total GLM calls that failed (transport error or HTTP >= 400) by operation.\n")
	b.WriteString("# TYPE agmem_glm_errors_total counter\n")
	errByOp := map[string]uint64{}
	for k, v := range m.glmCalls {
		if k.Result != "ok" {
			errByOp[k.Op] += v
		}
	}
	ops := make([]string, 0, len(errByOp))
	for op := range errByOp {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		writeSample(b, "agmem_glm_errors_total", []string{labelPair("op", op)}, strconv.FormatUint(errByOp[op], 10))
	}

	b.WriteString("# HELP agmem_http_requests_total HTTP requests by method, route pattern and status.\n")
	b.WriteString("# TYPE agmem_http_requests_total counter\n")
	reqKeys := make([]httpMetricKey, 0, len(m.httpRequests))
	for k := range m.httpRequests {
		reqKeys = append(reqKeys, k)
	}
	sort.Slice(reqKeys, func(i, j int) bool {
		a, c := reqKeys[i], reqKeys[j]
		if a.Method != c.Method {
			return a.Method < c.Method
		}
		if a.Route != c.Route {
			return a.Route < c.Route
		}
		return a.Status < c.Status
	})
	for _, k := range reqKeys {
		writeSample(b, "agmem_http_requests_total",
			[]string{labelPair("method", k.Method), labelPair("route", k.Route), labelPair("status", k.Status)},
			strconv.FormatUint(m.httpRequests[k], 10))
	}

	b.WriteString("# HELP agmem_http_request_duration_seconds HTTP request latency in seconds by method, route pattern and status.\n")
	b.WriteString("# TYPE agmem_http_request_duration_seconds histogram\n")
	latKeys := make([]latencyMetricKey, 0, len(m.httpLatency))
	for k := range m.httpLatency {
		latKeys = append(latKeys, k)
	}
	sort.Slice(latKeys, func(i, j int) bool {
		a, c := latKeys[i], latKeys[j]
		if a.Method != c.Method {
			return a.Method < c.Method
		}
		if a.Route != c.Route {
			return a.Route < c.Route
		}
		return a.Status < c.Status
	})
	for _, k := range latKeys {
		h := m.httpLatency[k]
		base := []string{labelPair("method", k.Method), labelPair("route", k.Route), labelPair("status", k.Status)}
		for i, ub := range httpLatencyBuckets {
			writeSample(b, "agmem_http_request_duration_seconds_bucket",
				append(base, labelPair("le", formatMetricFloat(ub))), strconv.FormatUint(h.Buckets[i], 10))
		}
		writeSample(b, "agmem_http_request_duration_seconds_bucket",
			append(base, labelPair("le", "+Inf")), strconv.FormatUint(h.Count, 10))
		writeSample(b, "agmem_http_request_duration_seconds_sum", base, formatMetricFloat(h.Sum))
		writeSample(b, "agmem_http_request_duration_seconds_count", base, strconv.FormatUint(h.Count, 10))
	}

	m.writeWikiSync(b)

	// /api/ask 本地缓存命中/未命中（hit/miss 序列恒在，便于 rate() 告警有对象）。
	b.WriteString("# HELP agmem_ask_cache_total /api/ask in-process local cache lookups by result (hit/miss).\n")
	b.WriteString("# TYPE agmem_ask_cache_total counter\n")
	for _, result := range []string{"hit", "miss"} {
		writeSample(b, "agmem_ask_cache_total",
			[]string{labelPair("result", result)}, strconv.FormatUint(m.askCache[result], 10))
	}

	_, _ = io.WriteString(w, b.String())
}

// writeWikiSync 输出 wiki 同步指标（m.mu 已由调用方持有）。未启用/从未运行时
// enabled=0 且计数器为 0——序列恒在，便于 `agmem_wikisync_enabled == 1` 告警有对象。
func (m *Metrics) writeWikiSync(b *strings.Builder) {
	var st wikisync.Status
	if m.wiki != nil {
		st = *m.wiki
	}
	b.WriteString("# HELP agmem_wikisync_enabled Wiki sync enabled flag (1=enabled, 0=disabled/absent).\n")
	b.WriteString("# TYPE agmem_wikisync_enabled gauge\n")
	writeSample(b, "agmem_wikisync_enabled", nil, boolMetric(st.Enabled))

	b.WriteString("# HELP agmem_wikisync_runs_total Wiki sync runs finished (success or failure).\n")
	b.WriteString("# TYPE agmem_wikisync_runs_total counter\n")
	writeSample(b, "agmem_wikisync_runs_total", nil, strconv.FormatInt(st.Runs, 10))

	b.WriteString("# HELP agmem_wikisync_added_total Documents newly ingested by wiki sync.\n")
	b.WriteString("# TYPE agmem_wikisync_added_total counter\n")
	writeSample(b, "agmem_wikisync_added_total", nil, strconv.FormatInt(st.TotalAdded, 10))

	b.WriteString("# HELP agmem_wikisync_changed_total Documents re-ingested after a wiki edit.\n")
	b.WriteString("# TYPE agmem_wikisync_changed_total counter\n")
	writeSample(b, "agmem_wikisync_changed_total", nil, strconv.FormatInt(st.TotalChanged, 10))

	b.WriteString("# HELP agmem_wikisync_removed_total Documents removed from the wiki and deleted from the KB.\n")
	b.WriteString("# TYPE agmem_wikisync_removed_total counter\n")
	writeSample(b, "agmem_wikisync_removed_total", nil, strconv.FormatInt(st.TotalRemoved, 10))

	b.WriteString("# HELP agmem_wikisync_failed_total Per-document failures across runs.\n")
	b.WriteString("# TYPE agmem_wikisync_failed_total counter\n")
	writeSample(b, "agmem_wikisync_failed_total", nil, strconv.FormatInt(st.TotalFailed, 10))

	b.WriteString("# HELP agmem_wikisync_last_run_timestamp_seconds Unix time of the last run start (0 = never).\n")
	b.WriteString("# TYPE agmem_wikisync_last_run_timestamp_seconds gauge\n")
	writeSample(b, "agmem_wikisync_last_run_timestamp_seconds", nil, strconv.FormatInt(st.LastRunUnix, 10))

	b.WriteString("# HELP agmem_wikisync_last_duration_seconds Duration of the last run.\n")
	b.WriteString("# TYPE agmem_wikisync_last_duration_seconds gauge\n")
	writeSample(b, "agmem_wikisync_last_duration_seconds", nil, formatMetricFloat(float64(st.LastDurationMS)/1000))

	b.WriteString("# HELP agmem_wikisync_scanned_nodes Nodes scanned in the last run.\n")
	b.WriteString("# TYPE agmem_wikisync_scanned_nodes gauge\n")
	writeSample(b, "agmem_wikisync_scanned_nodes", nil, strconv.Itoa(st.LastScanned))

	b.WriteString("# HELP agmem_wikisync_last_run_failed Documents that failed in the last run.\n")
	b.WriteString("# TYPE agmem_wikisync_last_run_failed gauge\n")
	writeSample(b, "agmem_wikisync_last_run_failed", nil, strconv.Itoa(st.LastFailed))
}

func boolMetric(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func writeSample(b *strings.Builder, name string, labels []string, value string) {
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteString("{")
		b.WriteString(strings.Join(labels, ","))
		b.WriteString("}")
	}
	b.WriteString(" ")
	b.WriteString(value)
	b.WriteString("\n")
}

func labelPair(name, value string) string {
	return name + `="` + escapeLabelValue(value) + `"`
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return strings.ReplaceAll(v, `"`, `\"`)
}

func formatMetricFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
