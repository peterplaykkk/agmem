package cachectl

// 契约超集测试（2026-09-11）：agmemd 的 /api/ask 必须能被直接按 agmemserv
// /api/ask 契约消费的上游客户端（如 claude-ask-server：只读顶层 `hits`）零改动
// 复用——否则把 AGMEM_URL 指向本地 agmemd 会静默解析出 0 命中、答案缺知识注入。
//
// 断言：
//   1. 透传：miss 与 hit 两条路径的顶层 hits/kb/version/filter/global 都出现；
//   2. 语义一致：命中返回的顶层 hits 与未命中完全逐字节相同（缓存不改语义）；
//   3. 命中不落上游：同问第二次不再打上游（0.8 近义 + 版本一致）；
//   4. 失败不入缓存：上游 5xx 后同问仍 miss（无僵尸条目）；
//   5. 不误命中：换问题必 miss。
// EmbedFn 注入绕过 onnxruntime；mock 上游带人工延迟以体现命中收益。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"sre/agmem/internal/cache"
)

const (
	ptQuestion = "kubectl 是什么"
	ptOther    = "统一出口代理地址"
	ptVersion  = 7
)

// ptUpstream mock 上游：/api/version/{kb} 返回固定版本；/api/ask 返回真实形状的
// agmem 响应体（顶层 hits），并支持人工延迟与失败注入。
type ptUpstream struct {
	mu       sync.Mutex
	calls    int
	delay    time.Duration
	failAsk  bool
	failCode int
}

func (m *ptUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= len("/api/version/") && r.URL.Path[:len("/api/version/")] == "/api/version/" {
			_ = json.NewEncoder(w).Encode(map[string]any{"version": ptVersion})
			return
		}
		if r.URL.Path == "/api/ask" {
			m.mu.Lock()
			m.calls++
			delay, fail, code := m.delay, m.failAsk, m.failCode
			m.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			if fail {
				w.WriteHeader(code)
				return
			}
			_, _ = w.Write([]byte(`{"kb":"sre","global":true,"version":7,` +
				`"hits":[{"id":"p1","score":0.91,"payload":{"kb":"sre","dir":"导航问答沉淀-clean","text":"kubectl 是 Kubernetes 命令行工具"}}],` +
				`"filter":{"mode":"hybrid","threshold":0.5,"kept":1,"dropped":0,"deduped":0}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
}

func (m *ptUpstream) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func newPTLocal(t *testing.T, up *ptUpstream) (*httptest.Server, *cache.Store) {
	t.Helper()
	agsrv := httptest.NewServer(up.handler())
	t.Cleanup(agsrv.Close)
	store, err := cache.OpenCache(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	t.Cleanup(func() { store.DB.Close() })
	// 文本相关向量：同文本同向量（可命中），异文本正交（余弦 0 < 0.8 阈值 →
	// 不误命中）。真实引擎（bge-base-zh）对无关问题的余弦亦远低于 0.8。
	embed := func(text string) ([]float32, error) {
		if text == ptOther {
			return []float32{0, 1}, nil
		}
		return []float32{1, 0}, nil
	}
	s := &Server{
		Cfg:     Config{AgmemURL: agsrv.URL, AgmemToken: "t", MaxBytes: 1 << 20, KB: "sre"},
		Store:   store,
		HTTP:    &http.Client{},
		EmbedFn: embed,
	}
	return httptest.NewServer(s.Routes()), store
}

// 透传 + 语义一致 + 不落上游：同问第二次命中，顶层 hits 与首次逐字节相同。
func TestAsk_PassthroughSupersetAndStableHits(t *testing.T) {
	up := &ptUpstream{}
	local, _ := newPTLocal(t, up)
	req := map[string]any{"query": ptQuestion, "top_k": 5, "global": true}

	_, miss := postAsk(t, local.URL, req)
	if miss.CacheHit {
		t.Fatalf("first ask must miss: %+v", miss)
	}
	// 1) 顶层透传字段齐全（与 agmemserv 契约一致）
	if miss.KB != "sre" || miss.Version != ptVersion || len(miss.Hits) == 0 || len(miss.Filter) == 0 || !miss.Global {
		t.Fatalf("miss passthrough incomplete: %+v (hits=%s filter=%s)", miss, miss.Hits, miss.Filter)
	}

	_, hit := postAsk(t, local.URL, req)
	if !hit.CacheHit {
		t.Fatalf("repeat ask must hit local cache: %+v", hit)
	}
	// 2) 命中顶层 hits 与未命中逐字节一致（缓存不改结果语义）
	if string(hit.Hits) != string(miss.Hits) {
		t.Fatalf("hit hits != miss hits\n hit=%s\nmiss=%s", hit.Hits, miss.Hits)
	}
	if hit.KB != miss.KB || hit.Version != miss.Version || string(hit.Filter) != string(miss.Filter) {
		t.Fatalf("hit passthrough differs from miss: hit=%+v miss=%+v", hit, miss)
	}
	// 3) 第二次不再打上游
	if n := up.callCount(); n != 1 {
		t.Fatalf("second ask must be local, want 1 upstream call, got %d", n)
	}
}

// 命中收益：上游人工延迟 200ms；第二次同问命中应在 <150ms 内返回（目标门禁）。
func TestAsk_CacheHitUnder150ms(t *testing.T) {
	up := &ptUpstream{delay: 200 * time.Millisecond}
	local, _ := newPTLocal(t, up)
	req := map[string]any{"query": ptQuestion}

	postAsk(t, local.URL, req) // miss（落缓存）
	start := time.Now()
	_, hit := postAsk(t, local.URL, req)
	elapsed := time.Since(start)
	if !hit.CacheHit {
		t.Fatalf("second ask must hit: %+v", hit)
	}
	if elapsed >= 150*time.Millisecond {
		t.Fatalf("cache hit took %v, want <150ms", elapsed)
	}
	t.Logf("cache hit latency = %v (upstream miss was >=200ms)", elapsed)
}

// 失败不入缓存：上游 5xx 后同问仍走上游（无僵尸条目遮蔽）。
func TestAsk_UpstreamFailureNotCached(t *testing.T) {
	up := &ptUpstream{failAsk: true, failCode: http.StatusBadGateway}
	local, store := newPTLocal(t, up)
	req := map[string]any{"query": ptQuestion}

	code, _ := postAsk(t, local.URL, req)
	if code != http.StatusBadGateway {
		t.Fatalf("upstream failure must surface as 502, got %d", code)
	}
	if _, err := store.Get(cache.QueryID("", ptQuestion)); err != cache.ErrCacheNotFound {
		t.Fatalf("failed ask must NOT be cached, got err=%v", err)
	}
	// 再问一次仍打上游（未因失败条目命中）
	postAsk(t, local.URL, req)
	if n := up.callCount(); n != 2 {
		t.Fatalf("want 2 upstream calls (no zombie hit), got %d", n)
	}
}

// 不误命中：换新问题必 miss。
func TestAsk_NewQuestionMisses(t *testing.T) {
	up := &ptUpstream{}
	local, _ := newPTLocal(t, up)

	_, first := postAsk(t, local.URL, map[string]any{"query": ptQuestion})
	if first.CacheHit {
		t.Fatalf("first must miss: %+v", first)
	}
	_, second := postAsk(t, local.URL, map[string]any{"query": ptOther})
	if second.CacheHit {
		t.Fatalf("different question must NOT hit: %+v", second)
	}
	if n := up.callCount(); n != 2 {
		t.Fatalf("want 2 upstream calls, got %d", n)
	}
}
