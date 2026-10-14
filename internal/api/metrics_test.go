package api

// G31 / UC-010 A7：/metrics 端点（注册、路由可达、无鉴权、无敏感字段）。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sre/agmem/internal/cleaner"
	"sre/agmem/internal/store"
)

// metricsText 取一次 /metrics 响应体（经 RootHandler，即生产装配路径）。
func metricsText(t *testing.T, s *Server) (int, http.Header, string) {
	t.Helper()
	h := s.RootHandler(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}

func TestMetricsEndpointNoAuth(t *testing.T) {
	s := testServer()
	s.Cfg.Version = "abc1234"
	s.Cfg.KB = "sre"
	code, hdr, body := metricsText(t, s)
	if code != http.StatusOK {
		t.Fatalf("GET /metrics without token: want 200 (no auth), got %d: %s", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Fatalf("content-type = %q, want Prometheus text/0.0.4", ct)
	}
	for k, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'; object-src 'none'",
	} {
		if got := hdr.Get(k); got != want {
			t.Errorf("security header %s=%q want %q", k, got, want)
		}
	}
	for _, want := range []string{
		`agmem_build_info{version="abc1234"} 1`,
		"# TYPE agmem_process_start_time_seconds gauge",
		"agmem_process_uptime_seconds ",
		`agmem_clean_queue_items{status="pending"} 0`,
		`agmem_clean_queue_items{status="embedded"} 0`,
		`agmem_clean_queue_items{status="cleaning"} 0`,
		`agmem_clean_queue_items{status="failed"} 0`,
		"# TYPE agmem_glm_errors_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
}

// 非 /metrics 路径仍走 fallback（/metrics 只在 RootHandler 层前置注册，不吞其它路径）。
func TestRootHandlerFallsThroughNonMetrics(t *testing.T) {
	s := testServer()
	fallbackHit := false
	h := s.RootHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHit = true
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/healthz", nil))
	if !fallbackHit || rec.Code != http.StatusTeapot {
		t.Fatalf("non-/metrics must reach fallback: hit=%v code=%d", fallbackHit, rec.Code)
	}
}

// 真实 socket 层验证（非 Recorder）：Prometheus 抓取路径不带任何凭证即可 200。
func TestMetricsServedOverHTTPWithoutCredentials(t *testing.T) {
	s := testServer()
	s.Cfg.Version = "sha-e2e"
	ts := httptest.NewServer(s.RootHandler(http.NotFoundHandler()))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `agmem_build_info{version="sha-e2e"} 1`) {
		t.Fatalf("metrics body missing build info:\n%s", body)
	}
}

func TestMetricsRouteLabelBoundedNoSensitive(t *testing.T) {
	s := testServer()
	s.Cfg.KB = "sre"
	h := s.RootHandler(s.Routes())
	// 带 secret 的原始路径 + 查询串：不得出现在指标标签里。
	req := httptest.NewRequest(http.MethodGet, "/api/kb/supersecret-kb-name/points?token=leak-me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", rec.Code)
	}
	_, _, body := metricsText(t, s)
	if !strings.Contains(body, `route="GET /api/kb/{kb}/points"`) {
		t.Fatalf("route template label missing:\n%s", body)
	}
	for _, secret := range []string{"supersecret-kb-name", "leak-me", "test-token"} {
		if strings.Contains(body, secret) {
			t.Errorf("metrics leaked %q:\n%s", secret, body)
		}
	}
	if !strings.Contains(body, `agmem_http_requests_total{method="GET",route="GET /api/kb/{kb}/points",status="401"} 1`) {
		t.Errorf("http request counter wrong:\n%s", body)
	}
	if !strings.Contains(body, `agmem_http_request_duration_seconds_count{method="GET",route="GET /api/kb/{kb}/points",status="401"} 1`) {
		t.Errorf("latency histogram count wrong:\n%s", body)
	}
}

type fakeQueueStore struct {
	gotKB  string
	counts map[store.TaskStatus]int64
}

func (f *fakeQueueStore) GetKB(_ context.Context, name string) (*store.KB, error) {
	f.gotKB = name
	return &store.KB{ID: 1, Name: name}, nil
}

func (f *fakeQueueStore) ListItemsByStatusPage(_ context.Context, _ int64, status store.TaskStatus, _, _ int) (*store.PageResult, error) {
	return &store.PageResult{Total: f.counts[status]}, nil
}

func TestRefreshQueueMetricsUsesConfiguredKB(t *testing.T) {
	s := testServer()
	s.Cfg.KB = "sre"
	s.Cfg.Version = "v1"
	fake := &fakeQueueStore{counts: map[store.TaskStatus]int64{
		store.StatusPending: 3, store.StatusEmbedded: 2, store.StatusCleaning: 1, store.StatusFailed: 4,
	}}
	s.QueueStats = fake
	s.RefreshQueueMetrics(context.Background())
	if fake.gotKB != "sre" {
		t.Fatalf("queue sampler must resolve cfg.KB, got %q", fake.gotKB)
	}
	_, _, body := metricsText(t, s)
	for _, want := range []string{
		`agmem_clean_queue_items{status="pending"} 3`,
		`agmem_clean_queue_items{status="embedded"} 2`,
		`agmem_clean_queue_items{status="cleaning"} 1`,
		`agmem_clean_queue_items{status="failed"} 4`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue gauge missing %q\n%s", want, body)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMetricsGLMCallErrors(t *testing.T) {
	m := NewMetrics("v9")
	m.ObserveHTTP("GET", "GET /api/healthz", 200, 2*time.Millisecond) // 干扰项：HTTP 指标不参与 GLM 计数
	c := m.InstrumentHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("boom"))}, nil
	})})
	req, _ := http.NewRequest(http.MethodPost, "https://ai-gateway.example.com/v1/chat/completions", nil)
	if _, err := c.Do(req); err != nil {
		t.Fatalf("do: %v", err)
	}
	cErr := m.InstrumentHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: timeout")
	})})
	req2, _ := http.NewRequest(http.MethodGet, "https://ai-gateway.example.com/v1/models", nil)
	if _, err := cErr.Do(req2); err == nil {
		t.Fatal("want transport error")
	}
	var b strings.Builder
	m.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		`agmem_glm_calls_total{op="chat",result="http_5xx"} 1`,
		`agmem_glm_calls_total{op="models",result="transport_error"} 1`,
		`agmem_glm_errors_total{op="chat"} 1`,
		`agmem_glm_errors_total{op="models"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("glm metric missing %q\n%s", want, out)
		}
	}
	// 路径原文不进标签（防高基数/敏感）
	if strings.Contains(out, "ai-gateway") || strings.Contains(out, "/v1/") {
		t.Errorf("GLM metric leaked URL:\n%s", out)
	}
}

// 采样失败（store 不可用）不 panic、不影响端点可抓取性。
func TestRefreshQueueMetricsStoreError(t *testing.T) {
	s := testServer()
	s.Cfg.KB = "sre"
	s.QueueStats = &failingQueueStore{}
	s.RefreshQueueMetrics(context.Background())
	code, _, body := metricsText(t, s)
	if code != http.StatusOK {
		t.Fatalf("metrics endpoint must stay 200 on queue sample error, got %d", code)
	}
	if !strings.Contains(body, `agmem_clean_queue_items{status="failed"} 0`) {
		t.Errorf("gauges must default to 0:\n%s", body)
	}
}

type failingQueueStore struct{}

func (f *failingQueueStore) GetKB(context.Context, string) (*store.KB, error) {
	return nil, errors.New("mysql down")
}

func (f *failingQueueStore) ListItemsByStatusPage(context.Context, int64, store.TaskStatus, int, int) (*store.PageResult, error) {
	return nil, errors.New("mysql down")
}

// 并发写入（HTTP handler / GLM transport / 队列采集器 / 抓取）不得数据竞争。
// 用 -race 运行时本用例是竞态门禁。
func TestMetricsConcurrentSafe(t *testing.T) {
	m := NewMetrics("v1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(4)
		go func() { defer wg.Done(); m.ObserveHTTP("GET", "GET /api/me", 200, time.Millisecond) }()
		go func() { defer wg.Done(); m.ObserveGLM("chat", "ok") }()
		go func() { defer wg.Done(); m.SetQueue("pending", int64(1)) }()
		go func() {
			defer wg.Done()
			var b strings.Builder
			m.WritePrometheus(&b)
			if !strings.Contains(b.String(), "agmem_build_info") {
				t.Errorf("scrape during writes must be complete")
			}
		}()
	}
	wg.Wait()
}

// TestMetricsCleanCachePrefix（S3）：带前缀清洗的 cache hit/miss、前缀体积与 hash
// 经 /metrics 暴露，供轮内命中率验收（hit/(hit+miss)）。
func TestMetricsCleanCachePrefix(t *testing.T) {
	s := testServer()
	s.Metrics = NewMetrics("x")
	s.Metrics.ObserveCleanCache(cleaner.CleanStats{
		PrefixHash: "abc123def4567890", PrefixTokens: 700000,
		L1Tokens: 500000, L2Tokens: 200000, DirDepth: 3,
		PromptTokens: 800000, CacheHitTokens: 900, CacheMissTokens: 100, ReasoningTokens: 42,
	})
	code, _, body := metricsText(t, s)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{
		"agmem_clean_cache_hit_tokens_total 900",
		"agmem_clean_cache_miss_tokens_total 100",
		"agmem_clean_prefix_tokens 700000",
		"agmem_clean_prefix_l1_tokens 500000",
		"agmem_clean_prefix_l2_tokens 200000",
		"agmem_clean_prefix_dir_depth 3",
		`agmem_clean_prefix_hash{hash="abc123def4567890"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
}
