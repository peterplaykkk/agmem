package cachectl

// agmemd 服务端行为测试：mock agmem（版本不命中/命中路径、clear）。
// 本地 embedding 用可注入的假引擎（绕过 onnxruntime）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"sre/agmem/internal/cache"
)

// ask 测试通过 Server.EmbedFn 注入固定向量，绕过 onnxruntime；
// 真实 onnx 路径见 local_embed_test.go（库缺失时 skip）。

func mockAgmem(t *testing.T, version uint64, askHits int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/version/sre":
			_ = json.NewEncoder(w).Encode(map[string]any{"kb": "sre", "version": version})
		case r.URL.Path == "/api/ask":
			_ = json.NewEncoder(w).Encode(map[string]any{"kb": "sre", "hits": askHits})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestServer(t *testing.T, agmemURL string) *httptest.Server {
	t.Helper()
	store, err := cache.OpenCache(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	t.Cleanup(func() { store.DB.Close() })
	s := &Server{
		Cfg:   Config{AgmemURL: agmemURL, AgmemToken: "t", MaxBytes: 1 << 20, KB: "sre"},
		Store: store,
		HTTP:  &http.Client{},
	}
	return httptest.NewServer(s.Routes())
}

func TestAsk_UpstreamAndCacheStore(t *testing.T) {
	agmem := mockAgmem(t, 3, 42)
	defer agmem.Close()
	local := newTestServer(t, agmem.URL)
	defer local.Close()

	post := func() (askResp, int) {
		resp, err := http.Post(local.URL+"/api/ask", "application/json",
			bytesJSON(map[string]any{"kb": "sre", "query": "qdrant 维度"}))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		var out askResp
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out, resp.StatusCode
	}

	// 引擎未就绪（EmbedFn nil）→ 503
	_, code := post()
	if code != http.StatusServiceUnavailable {
		t.Fatalf("engine unset: want 503, got %d", code)
	}

	// 注入固定向量
	var embedCount int
	// 直接改 server 内部（httptest 包装后拿不到原 Server）——通过第二次构造注入
	store, _ := cache.OpenCache(t.TempDir())
	defer store.DB.Close()
	s := &Server{
		Cfg:   Config{AgmemURL: agmem.URL, AgmemToken: "t", MaxBytes: 1 << 20, KB: "sre"},
		Store: store,
		HTTP:  &http.Client{},
		EmbedFn: func(text string) ([]float32, error) {
			embedCount++
			return []float32{1, 0}, nil
		},
	}
	local2 := httptest.NewServer(s.Routes())
	defer local2.Close()

	resp, err := http.Post(local2.URL+"/api/ask", "application/json",
		bytesJSON(map[string]any{"kb": "sre", "query": "qdrant 维度"}))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	var out askResp
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || out.CacheHit {
		t.Fatalf("first ask must be upstream miss: code=%d out=%+v", resp.StatusCode, out)
	}
	if out.ID == "" || out.KBVersion != 3 {
		t.Fatalf("want id+version, got %+v", out)
	}

	// 第二次同问题同版本 → 本地命中
	resp2, _ := http.Post(local2.URL+"/api/ask", "application/json",
		bytesJSON(map[string]any{"kb": "sre", "query": "qdrant 维度"}))
	var out2 askResp
	_ = json.NewDecoder(resp2.Body).Decode(&out2)
	resp2.Body.Close()
	if !out2.CacheHit {
		t.Fatalf("second ask should hit local cache: %+v", out2)
	}
	if out2.ID != out.ID {
		t.Fatalf("same question must map to same id: %s vs %s", out.ID, out2.ID)
	}
	if embedCount != 2 {
		t.Fatalf("want 2 embeds, got %d", embedCount)
	}
}

func TestAsk_VersionMismatchNoHit(t *testing.T) {
	agmem := mockAgmem(t, 3, 1)
	defer agmem.Close()
	store, _ := cache.OpenCache(t.TempDir())
	defer store.DB.Close()
	// 预置一条 version=1 的历史记录（向量同向）
	_ = store.Put(cache.Entry{
		ID: "q-old", KB: "sre", Question: "老问题",
		QuestionVec: cache.EncodeVec([]float32{1, 0}),
		ResultJSON:  `{"hits":0}`, KBVersion: 1,
	})
	s := &Server{
		// MaxBytes 须为正：本用例断言落库状态，0 会立即 LRU 清空
		Cfg:     Config{AgmemURL: agmem.URL, AgmemToken: "t", MaxBytes: 1 << 20, KB: "sre"},
		Store:   store,
		HTTP:    &http.Client{},
		EmbedFn: func(string) ([]float32, error) { return []float32{1, 0}, nil },
	}
	local := httptest.NewServer(s.Routes())
	defer local.Close()
	resp, err := http.Post(local.URL+"/api/ask", "application/json",
		bytesJSON(map[string]any{"kb": "sre", "query": "老问题"}))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	var out askResp
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.CacheHit {
		t.Fatalf("version mismatch (local=1 remote=3) must not hit: %+v", out)
	}
	// 行为变更（UC-3 修复）：失效旧条目被删除，同问按真实 QueryID 重写
	// （不再加时间戳后缀分叉）。旧行为下僵尸条目永久遮蔽新结果——
	// 同问此后每次都 miss 且缓存无限增长。
	if _, err := store.Get("q-old"); err != cache.ErrCacheNotFound {
		t.Fatalf("stale zombie entry must be deleted, got %v", err)
	}
	fresh, err := store.Get(cache.QueryID("", "老问题"))
	if err != nil {
		t.Fatalf("fresh entry must be stored under real QueryID: %v", err)
	}
	if fresh.KBVersion != 3 {
		t.Fatalf("fresh entry must carry current version, got %d", fresh.KBVersion)
	}
}

func TestAsk_SimilarityBelowThresholdNoHit(t *testing.T) {
	agmem := mockAgmem(t, 5, 1)
	defer agmem.Close()
	store, _ := cache.OpenCache(t.TempDir())
	defer store.DB.Close()
	_ = store.Put(cache.Entry{
		ID: "q-x", KB: "sre", Question: "完全不相关",
		QuestionVec: cache.EncodeVec([]float32{0, 1}),
		ResultJSON:  `{"hits":0}`, KBVersion: 5,
	})
	s := &Server{
		Cfg:     Config{AgmemURL: agmem.URL, KB: "sre"},
		Store:   store,
		HTTP:    &http.Client{},
		EmbedFn: func(string) ([]float32, error) { return []float32{1, 0}, nil }, // 正交 → sim=0
	}
	local := httptest.NewServer(s.Routes())
	defer local.Close()
	resp, _ := http.Post(local.URL+"/api/ask", "application/json",
		bytesJSON(map[string]any{"kb": "sre", "query": "随便问"}))
	var out askResp
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.CacheHit {
		t.Fatalf("sim 0 must not hit: %+v", out)
	}
}

func TestAsk_EmbedErrorPropagates(t *testing.T) {
	agmem := mockAgmem(t, 1, 0)
	defer agmem.Close()
	store, _ := cache.OpenCache(t.TempDir())
	defer store.DB.Close()
	s := &Server{
		Cfg:     Config{AgmemURL: agmem.URL, KB: "sre"},
		Store:   store,
		HTTP:    &http.Client{},
		EmbedFn: func(string) ([]float32, error) { return nil, errors.New("ort boom") },
	}
	local := httptest.NewServer(s.Routes())
	defer local.Close()
	resp, err := http.Post(local.URL+"/api/ask", "application/json",
		bytesJSON(map[string]any{"kb": "sre", "query": "x"}))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", resp.StatusCode)
	}
}

func TestAsk_VersionFetchFails(t *testing.T) {
	// agmem 挂了：版本校验失败 → 502（不允许无版本命中）
	s := &Server{
		Cfg:   Config{AgmemURL: "http://127.0.0.1:1", KB: "sre"},
		Store: mustStore(t),
		HTTP:  &http.Client{},
		EmbedFn: func(string) ([]float32, error) {
			return []float32{1, 0}, nil
		},
	}
	local := httptest.NewServer(s.Routes())
	defer local.Close()
	resp, err := http.Post(local.URL+"/api/ask", "application/json",
		bytesJSON(map[string]any{"kb": "sre", "query": "x"}))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
}

func TestClear(t *testing.T) {
	agmem := mockAgmem(t, 1, 0)
	defer agmem.Close()
	store, _ := cache.OpenCache(t.TempDir())
	defer store.DB.Close()
	_ = store.Put(cache.Entry{ID: "q-a", KB: "sre", Question: "a", ResultJSON: "xxxxxxxx"})
	s := &Server{Cfg: Config{AgmemURL: agmem.URL}, Store: store, HTTP: &http.Client{}}
	local := httptest.NewServer(s.Routes())
	defer local.Close()

	// 缺参数 → 400
	resp, _ := http.Post(local.URL+"/api/clear", "application/json", bytesJSON(map[string]any{}))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 按 id
	var out struct {
		FreedBytes int64 `json:"freed_bytes"`
	}
	resp2, _ := http.Post(local.URL+"/api/clear", "application/json",
		bytesJSON(map[string]any{"id": "q-a"}))
	_ = json.NewDecoder(resp2.Body).Decode(&out)
	resp2.Body.Close()
	if out.FreedBytes <= 0 {
		t.Fatalf("want freed>0, got %d", out.FreedBytes)
	}
	// all（空库）
	resp3, _ := http.Post(local.URL+"/api/clear", "application/json", bytesJSON(map[string]any{"all": true}))
	_ = json.NewDecoder(resp3.Body).Decode(&out)
	resp3.Body.Close()
}

func TestSecurityHeaders(t *testing.T) {
	s := &Server{Store: mustStore(t)}
	local := httptest.NewServer(s.Routes())
	defer local.Close()
	resp, err := http.Get(local.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Security-Policy"); got != "default-src 'none'" {
		t.Fatalf("CSP missing: %q", got)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("nosniff missing")
	}
}

// UC-004 C1：/healthz 回读缓存有效上限（默认 524288000，AGMEMD_MAX_BYTES 覆盖后生效）。
func TestHealthzExposesMaxBytes(t *testing.T) {
	for _, maxb := range []int64{524288000, 1048576} {
		s := &Server{Store: mustStore(t), Cfg: Config{MaxBytes: maxb}}
		local := httptest.NewServer(s.Routes())
		resp, err := http.Get(local.URL + "/healthz")
		if err != nil {
			local.Close()
			t.Fatalf("healthz: %v", err)
		}
		var out struct {
			MaxBytes int64 `json:"max_bytes"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		local.Close()
		if err != nil {
			t.Fatalf("decode healthz: %v", err)
		}
		if out.MaxBytes != maxb {
			t.Fatalf("healthz max_bytes=%d want %d", out.MaxBytes, maxb)
		}
	}
}

func mustStore(t *testing.T) *cache.Store {
	t.Helper()
	store, err := cache.OpenCache(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	t.Cleanup(func() { store.DB.Close() })
	return store
}

func bytesJSON(v any) *bytes.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}
