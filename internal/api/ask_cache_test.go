package api

// /api/ask 进程内本地缓存（2026-09-11）行为测试：
//   - 同问复答命中缓存（不再打 qdrant），命中与未命中的 hits 逐字节一致；
//   - 新问题不误命中（向量近邻 + 相似度门槛）；
//   - 知识库变版（version 变化）整体失效回上游；
//   - 上游/qdrant 失败不落缓存；
//   - Cache=nil（目录不可写 fail-open 后的终态）时 /api/ask 照常、唯不缓存；
//   - LRU 容量上限生效。
//
// embedding 用「按输入哈希取 one-hot 1024d」的 fake：同输入同向量、异输入近正交——
// 语义等价于真实 embedding 的「同问相似度高、异问相似度低」，且完全确定、免外网。
// store/qdrant/cache 全 fake/本地，不依赖 MySQL 与网络。

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sync/atomic"

	"sre/agmem/internal/cache"
	"sre/agmem/internal/embedding"
)

// md5Hex 计算字节 md5 十六进制（验证命中/未命中 hits 逐字节一致）。
func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// fakeEmbedderVarying 按输入文本哈希取 one-hot 1024d 向量（确定性）。
// 同一文本 → 同一向量（cosine=1，命中）；不同文本 → 不同维（cosine≈0，不误命中）。
func fakeEmbedderVarying(t *testing.T) *embedding.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i, in := range req.Input {
			vec := make([]float64, embedding.Dim)
			h := sha256.Sum256([]byte(in))
			idx := (int(h[0])<<8 | int(h[1])) % embedding.Dim
			vec[idx] = 1
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return embedding.New(srv.URL, "k", "", "", "m", 16, 0)
}

// newLiveCache 打开一个真实 SQLite 缓存（t.TempDir()，测试后关闭）。
func newLiveCache(t *testing.T) *cache.Store {
	t.Helper()
	s, err := cache.OpenCache(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	t.Cleanup(func() { s.DB.Close() })
	return s
}

// askRaw POST /api/ask 返回原始响应体（供 hits 逐字节比对）。
func askRaw(t *testing.T, url, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/api/ask", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post ask: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// hitsOf 从原始响应体取 hits 字段的原始字节（json.RawMessage，保序保字节）。
func hitsOf(t *testing.T, body []byte) []byte {
	t.Helper()
	var v struct {
		Hits json.RawMessage `json:"hits"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode hits: %v (%s)", err, body)
	}
	return v.Hits
}

func cacheHitFlag(t *testing.T, body []byte) bool {
	t.Helper()
	var v struct {
		CacheHit bool `json:"cache_hit"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode cache_hit: %v", err)
	}
	return v.CacheHit
}

// 同问复答：第二次命中缓存（不再打 qdrant），且命中与未命中的 hits 逐字节一致。
func TestAskCache_HitOnRepeat_ByteIdenticalHits(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{}
	s, ts := newSingleKBTestServer(t, st, qh)
	s.Cache = newLiveCache(t)
	s.Embedder = fakeEmbedderVarying(t)

	body := `{"query":"统一出口代理地址是什么","top_k":3}`
	code1, b1 := askRaw(t, ts.URL, body, "test-token")
	if code1 != http.StatusOK {
		t.Fatalf("first ask: want 200 got %d: %s", code1, b1)
	}
	if cacheHitFlag(t, b1) {
		t.Fatalf("first ask must be a miss (cache_hit=false): %s", b1)
	}
	qdrantCallsAfterMiss := len(qh.cols)
	if qdrantCallsAfterMiss != 1 {
		t.Fatalf("first ask must reach qdrant once, got %d", qdrantCallsAfterMiss)
	}

	code2, b2 := askRaw(t, ts.URL, body, "test-token")
	if code2 != http.StatusOK {
		t.Fatalf("second ask: want 200 got %d: %s", code2, b2)
	}
	if !cacheHitFlag(t, b2) {
		t.Fatalf("second ask must be a hit (cache_hit=true): %s", b2)
	}
	// 命中不再打 qdrant（这是「相同问题变快」的机制证据）
	if len(qh.cols) != qdrantCallsAfterMiss {
		t.Fatalf("cache hit must not call qdrant again: before=%d after=%d", qdrantCallsAfterMiss, len(qh.cols))
	}
	// hits 内容逐字节一致（md5 相同）
	if got, want := md5Hex(hitsOf(t, b2)), md5Hex(hitsOf(t, b1)); got != want {
		t.Fatalf("hit vs miss hits not byte-identical: hit md5=%s miss md5=%s\nhit=%s\nmiss=%s",
			got, want, hitsOf(t, b2), hitsOf(t, b1))
	}
	// 命中不得少字段：kb/version/filter/hits 都在
	var out map[string]any
	_ = json.Unmarshal(b2, &out)
	for _, k := range []string{"kb", "version", "hits", "filter", "global", "cache_hit"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("hit response missing field %q: %s", k, b2)
		}
	}
}

// 新问题不误命中：向量近正交 → 不满足相似度门槛，回上游。
func TestAskCache_NewQuestionNoFalseHit(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{}
	s, ts := newSingleKBTestServer(t, st, qh)
	s.Cache = newLiveCache(t)
	s.Embedder = fakeEmbedderVarying(t)

	if _, b := askRaw(t, ts.URL, `{"query":"问题一：代理端口"}`, "test-token"); cacheHitFlag(t, b) {
		t.Fatalf("first ask must miss: %s", b)
	}
	code, b2 := askRaw(t, ts.URL, `{"query":"问题二：完全不同的话题"}`, "test-token")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, b2)
	}
	if cacheHitFlag(t, b2) {
		t.Fatalf("a different question must not hit the cache: %s", b2)
	}
}

// 知识库变版 → 整体失效回上游，并以新版本重新落库（随后的复答命中新版本）。
func TestAskCache_VersionBumpInvalidates(t *testing.T) {
	st := newFakeSingleKB() // version 7
	qh := &fakeQdrantFanout{}
	s, ts := newSingleKBTestServer(t, st, qh)
	s.Cache = newLiveCache(t)
	s.Embedder = fakeEmbedderVarying(t)

	body := `{"query":"版本门禁问题","top_k":3}`
	askRaw(t, ts.URL, body, "test-token") // miss（落 v7）
	if _, b := askRaw(t, ts.URL, body, "test-token"); !cacheHitFlag(t, b) {
		t.Fatalf("precondition: repeat must hit before version bump: %s", b)
	}

	st.version = 8 // 知识库变版
	code, b3 := askRaw(t, ts.URL, body, "test-token")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, b3)
	}
	if cacheHitFlag(t, b3) {
		t.Fatalf("version bump must invalidate the cache: %s", b3)
	}
	// 变版后复答 → 命中新版本
	if _, b4 := askRaw(t, ts.URL, body, "test-token"); !cacheHitFlag(t, b4) {
		t.Fatalf("repeat after refresh must hit: %s", b4)
	}
	var out map[string]any
	_ = json.Unmarshal(b3, &out)
	if out["version"] != float64(8) {
		t.Fatalf("miss after bump must carry new version 8, got %v", out["version"])
	}
}

// 上游（qdrant）失败 → 不落缓存（失败不入缓存）。
func TestAskCache_FailureNotCached(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{fail: map[string]bool{"sre": true}}
	s, ts := newSingleKBTestServer(t, st, qh)
	store := newLiveCache(t)
	s.Cache = store
	s.Embedder = fakeEmbedderVarying(t)

	code, b := askRaw(t, ts.URL, `{"query":"失败问题"}`, "test-token")
	if code != http.StatusBadGateway {
		t.Fatalf("qdrant failure: want 502, got %d: %s", code, b)
	}
	if total, _ := store.TotalBytes(); total != 0 {
		t.Fatalf("failed ask must not write cache, bytes=%d", total)
	}
}

// dir 隔离：同问不同 dir 不共享缓存条目（Nearest 仅按 dir 过滤）。
func TestAskCache_DirScopeIsolation(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{}
	s, ts := newSingleKBTestServer(t, st, qh)
	s.Cache = newLiveCache(t)
	s.Embedder = fakeEmbedderVarying(t)

	if _, b := askRaw(t, ts.URL, `{"query":"同问","dir":"k8s"}`, "test-token"); cacheHitFlag(t, b) {
		t.Fatalf("first (dir=k8s) must miss: %s", b)
	}
	// 同问不同 dir → 必须 miss（范围不外泄）
	if _, b := askRaw(t, ts.URL, `{"query":"同问","dir":"mysql"}`, "test-token"); cacheHitFlag(t, b) {
		t.Fatalf("same question under a different dir must not hit: %s", b)
	}
	// 回到 dir=k8s → 命中
	if _, b := askRaw(t, ts.URL, `{"query":"同问","dir":"k8s"}`, "test-token"); !cacheHitFlag(t, b) {
		t.Fatalf("repeat under same dir must hit: %s", b)
	}
}

// Cache=nil（目录不可写 fail-open 后的终态）：/api/ask 照常成功、字段齐全，唯不缓存。
func TestAskCache_NilCacheDisabledStillServes(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{}
	s, ts := newSingleKBTestServer(t, st, qh)
	s.Cache = nil // 显式禁用
	s.Embedder = fakeEmbedderVarying(t)

	body := `{"query":"无缓存也要能答","top_k":2}`
	for i := 0; i < 2; i++ {
		code, b := askRaw(t, ts.URL, body, "test-token")
		if code != http.StatusOK {
			t.Fatalf("ask #%d with nil cache: want 200, got %d: %s", i, code, b)
		}
		if cacheHitFlag(t, b) {
			t.Fatalf("ask #%d with nil cache must report cache_hit=false: %s", i, b)
		}
	}
	// 无缓存 → 每次都打 qdrant
	if len(qh.cols) != 2 {
		t.Fatalf("nil cache must always call qdrant, got %d calls", len(qh.cols))
	}
}

// LRU 容量上限：CacheMaxBytes 很小时，大量写入被淘汰、总量受控（EvictOver 生效）。
func TestAskCache_LRUEvictionBounded(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{}
	s, ts := newSingleKBTestServer(t, st, qh)
	store := newLiveCache(t)
	s.Cache = store
	s.CacheMaxBytes = 700 // 每条 hits 体量 > 该值 → 反复写入即反复淘汰
	s.Embedder = fakeEmbedderVarying(t)

	for i := 0; i < 8; i++ {
		body := `{"query":"淘汰用例问题-` + string(rune('a'+i)) + `"}`
		if code, b := askRaw(t, ts.URL, body, "test-token"); code != http.StatusOK {
			t.Fatalf("ask #%d: want 200, got %d: %s", i, code, b)
		}
	}
	total, err := store.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes: %v", err)
	}
	// 单条条目通常 > 700B（hits 文本 + 向量 + 问题），故不会全部留存；总量须受控。
	if total > 4*1024 {
		t.Fatalf("LRU not bounding cache size: total=%d bytes", total)
	}
}

// /metrics 暴露 agmem_ask_cache_total{result=hit|miss}（hit/miss 序列恒在）。
func TestMetrics_AskCacheCounterExposed(t *testing.T) {
	m := NewMetrics("v")
	m.ObserveAskCache("hit")
	m.ObserveAskCache("hit")
	m.ObserveAskCache("miss")
	var b strings.Builder
	m.WritePrometheus(&b)
	out := b.String()
	if !strings.Contains(out, `agmem_ask_cache_total{result="hit"} 2`) {
		t.Fatalf("hit counter not exposed:\n%s", out)
	}
	if !strings.Contains(out, `agmem_ask_cache_total{result="miss"} 1`) {
		t.Fatalf("miss counter not exposed:\n%s", out)
	}
}

// 精确命中路径不做 embedding：同问复答第二次的 embedding 调用次数不增加
// （这是「相同问题不变快」达标的关键——省掉云端 embedding 往返）。
func TestAskCache_ExactHitSkipsEmbedding(t *testing.T) {
	var embedCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&embedCalls, 1)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i, in := range req.Input {
			vec := make([]float64, embedding.Dim)
			h := sha256.Sum256([]byte(in))
			vec[(int(h[0])<<8|int(h[1]))%embedding.Dim] = 1
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)

	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{}
	s, ts := newSingleKBTestServer(t, st, qh)
	s.Cache = newLiveCache(t)
	s.Embedder = embedding.New(srv.URL, "k", "", "", "m", 16, 0)

	body := `{"query":"精确命中不做嵌入的问题","top_k":3}`
	askRaw(t, ts.URL, body, "test-token") // miss：embedding 1 次
	if got := atomic.LoadInt32(&embedCalls); got != 1 {
		t.Fatalf("after miss want 1 embedding call, got %d", got)
	}
	code, b := askRaw(t, ts.URL, body, "test-token") // exact hit：不再 embedding
	if code != http.StatusOK || !cacheHitFlag(t, b) {
		t.Fatalf("second ask must be an exact hit: code=%d body=%s", code, b)
	}
	if got := atomic.LoadInt32(&embedCalls); got != 1 {
		t.Fatalf("exact hit must skip embedding: want 1 total call, got %d", got)
	}
}

// 近邻命中路径（改写问句）：对同一「主题」的不同措辞返回同一向量 → 第二次走
// Store.Nearest 命中（id 与精确路径不同，证明向量近邻路径生效）。
func TestAskCache_NearestHitOnParaphrase(t *testing.T) {
	topic := make([]float64, embedding.Dim)
	topic[7] = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i, in := range req.Input {
			vec := make([]float64, embedding.Dim)
			if strings.Contains(in, "topicA") {
				copy(vec, topic) // 同主题不同措辞 → 同一向量（cosine=1）
			} else {
				h := sha256.Sum256([]byte(in))
				vec[(int(h[0])<<8|int(h[1]))%embedding.Dim] = 1
			}
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)

	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{}
	s, ts := newSingleKBTestServer(t, st, qh)
	s.Cache = newLiveCache(t)
	s.Embedder = embedding.New(srv.URL, "k", "", "", "m", 16, 0)

	if _, b := askRaw(t, ts.URL, `{"query":"topicA 怎么配"}`, "test-token"); cacheHitFlag(t, b) {
		t.Fatalf("first ask must miss: %s", b)
	}
	qdrantAfterMiss := len(qh.cols)
	code, b2 := askRaw(t, ts.URL, `{"query":"topicA 的配置方法是什么"}`, "test-token")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, b2)
	}
	if !cacheHitFlag(t, b2) {
		t.Fatalf("paraphrase must hit via vector nearest: %s", b2)
	}
	if len(qh.cols) != qdrantAfterMiss {
		t.Fatalf("nearest hit must not call qdrant again: before=%d after=%d", qdrantAfterMiss, len(qh.cols))
	}
}
