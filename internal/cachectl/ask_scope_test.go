package cachectl

// 单库版守护级行为测试（2026-09-08 单库化）：
//   - scope 隔离收敛为 (dir, question) 两维：dir 变化必须 miss（范围泄漏回归锚）；
//   - 请求 kb / global 字段（deprecated）一律忽略——同问不同字段命中同一条目；
//   - query 必填；引擎未就绪 503；版本端点失败 502。
// mock agmem 上游 + 注入 EmbedFn，绕过 onnxruntime。
// 回归锚点：旧实现 Nearest 只按 kb+向量匹配，跨 dir 会错误命中（范围泄漏）；
// kb/global 维度已随单库化退役，不再参与隔离。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"sre/agmem/internal/cache"
)

// scopedAgmem mock 上游：/api/version/{kb} 返回可变版本号；/api/ask 计数并回显
// 请求体关键字段。
type scopedAgmem struct {
	mu          sync.Mutex
	askCalls    int
	lastBody    map[string]any
	version     int  // /api/version/{kb} 返回值（默认 2）
	failVersion bool // true → 版本端点 500
}

func (m *scopedAgmem) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/version/"):
			m.mu.Lock()
			fail, v := m.failVersion, m.version
			m.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"version": v})
		case r.URL.Path == "/api/ask":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.askCalls++
			m.lastBody = body
			n := m.askCalls
			m.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"kb": body["kb"], "dir": body["dir"], "global": body["global"], "upstream_n": n,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// setVersion 变更单 kb 版本（模拟服务端知识库变版）。
func (m *scopedAgmem) setVersion(v int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.version = v
}

func (m *scopedAgmem) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.askCalls
}

// newScopedServer 带 mock 上游与固定向量 EmbedFn 的 agmemd（单库 Cfg：KB=sre）。
func newScopedServer(t *testing.T, agmem *scopedAgmem) (*httptest.Server, *cache.Store) {
	t.Helper()
	agsrv := httptest.NewServer(agmem.handler())
	t.Cleanup(agsrv.Close)
	store, err := cache.OpenCache(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	t.Cleanup(func() { store.DB.Close() })
	s := &Server{
		Cfg:     Config{AgmemURL: agsrv.URL, AgmemToken: "t", MaxBytes: 1 << 20, KB: "sre"},
		Store:   store,
		HTTP:    &http.Client{},
		EmbedFn: func(string) ([]float32, error) { return []float32{1, 0}, nil },
	}
	return httptest.NewServer(s.Routes()), store
}

func postAsk(t *testing.T, url string, body map[string]any) (int, askResp) {
	t.Helper()
	resp, err := http.Post(url+"/api/ask", "application/json", bytesJSON(body))
	if err != nil {
		t.Fatalf("post ask: %v", err)
	}
	defer resp.Body.Close()
	var out askResp
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

const scopeQuestion = "APISIX 上游健康检查怎么做"

// dir 隔离（单库下唯一保留的 scope 维度）：同问 dir 维度变化必须 miss
// （旧实现会按向量命中 → 范围泄漏）。请求里的 kb/global 字段为 deprecated 回显，不影响。
func TestAsk_DirScopeIsolation(t *testing.T) {
	cases := []struct {
		name   string
		first  map[string]any // 预置（首轮查询，落缓存）
		second map[string]any // 跨 scope 复查，必须 miss
	}{
		{
			name:   "无dir(先) → 带dir(后) 不命中",
			first:  map[string]any{"query": scopeQuestion},
			second: map[string]any{"dir": "k8s", "query": scopeQuestion},
		},
		{
			name:   "带dir(先) → 无dir(后) 不命中",
			first:  map[string]any{"dir": "k8s", "query": scopeQuestion},
			second: map[string]any{"query": scopeQuestion},
		},
		{
			name:   "dirA(先) → dirB(后) 不命中",
			first:  map[string]any{"dir": "mysql", "query": scopeQuestion},
			second: map[string]any{"dir": "k8s", "query": scopeQuestion},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agmem := &scopedAgmem{}
			local, _ := newScopedServer(t, agmem)

			code, out1 := postAsk(t, local.URL, c.first)
			if code != 200 || out1.CacheHit {
				t.Fatalf("first ask must be upstream miss: code=%d out=%+v", code, out1)
			}
			code, out2 := postAsk(t, local.URL, c.second)
			if code != 200 || out2.CacheHit {
				t.Fatalf("cross-dir ask must NOT hit local cache (scope leak): code=%d out=%+v", code, out2)
			}
			// 两次都打到上游
			if n := agmem.calls(); n != 2 {
				t.Fatalf("want 2 upstream calls, got %d", n)
			}
		})
	}
}

// 同 dir 同问必须命中（隔离不误伤正常命中）；请求里的 deprecated kb/global
// 字段不影响命中。
func TestAsk_SameScopeHits(t *testing.T) {
	cases := []struct {
		name string
		req  map[string]any
	}{
		{name: "纯query同问", req: map[string]any{"query": scopeQuestion}},
		{name: "同dir同问", req: map[string]any{"dir": "k8s", "query": scopeQuestion}},
		{name: "带deprecated字段同问", req: map[string]any{"kb": "sre", "global": true, "query": scopeQuestion}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agmem := &scopedAgmem{}
			local, _ := newScopedServer(t, agmem)
			_, out1 := postAsk(t, local.URL, c.req)
			if out1.CacheHit {
				t.Fatalf("first ask must miss: %+v", out1)
			}
			_, out2 := postAsk(t, local.URL, c.req)
			if !out2.CacheHit {
				t.Fatalf("same-scope repeat must hit local cache: %+v", out2)
			}
			if n := agmem.calls(); n != 1 {
				t.Fatalf("second ask must be local, want 1 upstream call, got %d", n)
			}
		})
	}
}

// deprecated 字段忽略（单库化核心断言）：同问带/不带 kb、global 字段——
// 缓存 scope 已收敛为 (dir, question)，字段差异不产生新条目、不影响命中。
func TestAsk_DeprecatedKBGlobalFieldsIgnored(t *testing.T) {
	agmem := &scopedAgmem{}
	local, _ := newScopedServer(t, agmem)

	plain := map[string]any{"query": scopeQuestion}
	withKB := map[string]any{"kb": "sre", "query": scopeQuestion}
	withGlobal := map[string]any{"kb": "sre", "global": true, "query": scopeQuestion}

	_, out1 := postAsk(t, local.URL, plain)
	if out1.CacheHit {
		t.Fatalf("first ask must miss: %+v", out1)
	}
	// 带 deprecated 字段的同问 → 同一条目命中（不再分叉 scope）
	for name, req := range map[string]map[string]any{"withKB": withKB, "withGlobal": withGlobal} {
		_, out := postAsk(t, local.URL, req)
		if !out.CacheHit {
			t.Fatalf("%s: deprecated kb/global fields must be ignored (same entry hit): %+v", name, out)
		}
		if out.ID != out1.ID {
			t.Fatalf("%s: must reuse the same entry id %s, got %s", name, out1.ID, out.ID)
		}
	}
	if n := agmem.calls(); n != 1 {
		t.Fatalf("only the first ask may go upstream, got %d calls", n)
	}
}

// 引擎未就绪（无 EmbedFn、无 Engine）→ 503（与旧 kb 级路径一致；不落缓存、
// /api/ask 不打到上游——版本校验先行，但版本端点 ≠ ask 端点）。
func TestAsk_EngineNotReady503(t *testing.T) {
	agmem := &scopedAgmem{}
	agsrv := httptest.NewServer(agmem.handler())
	t.Cleanup(agsrv.Close)
	store, err := cache.OpenCache(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	t.Cleanup(func() { store.DB.Close() })
	s := &Server{
		Cfg:   Config{AgmemURL: agsrv.URL, AgmemToken: "t", MaxBytes: 1 << 20, KB: "sre"},
		Store: store,
		HTTP:  &http.Client{},
	}
	local := httptest.NewServer(s.Routes())
	t.Cleanup(local.Close)

	code, out := postAsk(t, local.URL, map[string]any{"query": scopeQuestion})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("engine not ready: want 503, got %d out=%+v", code, out)
	}
	if n := agmem.calls(); n != 0 {
		t.Fatalf("engine-not-ready must not call upstream ask, got %d calls", n)
	}
	if total, _ := store.TotalBytes(); total != 0 {
		t.Fatalf("must not write local cache, bytes=%d", total)
	}
}

// 校验：空 query → 400（不再要求 kb/global）；拒绝的请求不打上游。
func TestAsk_RequiresOnlyQuery(t *testing.T) {
	agmem := &scopedAgmem{}
	local, _ := newScopedServer(t, agmem)

	code, _ := postAsk(t, local.URL, map[string]any{"query": ""})
	if code != http.StatusBadRequest {
		t.Fatalf("empty query: want 400, got %d", code)
	}
	code, _ = postAsk(t, local.URL, map[string]any{"kb": "sre"})
	if code != http.StatusBadRequest {
		t.Fatalf("missing query: want 400, got %d", code)
	}
	if agmem.calls() != 0 {
		t.Fatalf("rejected requests must not reach upstream, got %d calls", agmem.calls())
	}
}

// 缓存条目溯源字段落库回读：dir 与查询一致；KB=cfg.KB（sre）；Global 为请求
// deprecated 字段回显（不再参与命中判定）。
func TestAsk_CacheEntryCarriesScope(t *testing.T) {
	agmem := &scopedAgmem{}
	local, store := newScopedServer(t, agmem)
	postAsk(t, local.URL, map[string]any{"dir": "k8s", "global": true, "query": scopeQuestion})
	id := cache.QueryID("k8s", scopeQuestion)
	e, err := store.Get(id)
	if err != nil {
		t.Fatalf("entry not stored: %v", err)
	}
	if e.Dir != "k8s" || !e.Global || e.KB != "sre" {
		t.Fatalf("scope/provenance mismatch in stored entry: %+v", e)
	}
}
