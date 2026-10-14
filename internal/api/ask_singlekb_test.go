package api

// 单库检索（2026-09-08 拍板）行为测试：
//   - 请求里的 kb / global 字段一律忽略（deprecated wire 兼容），检索范围恒为
//     cfg.KB（全局唯一库）；
//   - dir 仍可收窄到单 1 级目录（透传 qdrant filter）；
//   - 权限掩蔽 404、query 必填 400、embedding/qdrant 失败 502 传播。
//
// embedding/qdrant/store 全部 httptest/内存 fake —— 不依赖 MySQL 与外网。
// （原跨 kb fan-out 测试随单库化退役：visibleKBs/SearchMulti 循环已收敛为
// 单 collection SearchHybrid 直调，见 server.go。）

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sre/agmem/internal/auth"
	"sre/agmem/internal/config"
	"sre/agmem/internal/embedding"
	"sre/agmem/internal/qdrant"
	"sre/agmem/internal/store"
)

// fakeSingleKB 记录单库路径的 store 调用（GetKB 入参、权限主体、版本），
// 返回预置 kb（默认 sre/v7）。
type fakeSingleKB struct {
	mu        sync.Mutex
	getKBCall []string // GetKB 收到的 name 序列
	subs      []string // HasReadAccess 收到的 sub 序列（""=机器 token）
	accessOK  bool     // HasReadAccess 返回值
	version   uint64   // GetVersion 返回值
	dirs      map[string]*store.Dir
	created   [][]string // CreateTaskWithItems 收到的 texts
	kbName    string
}

func newFakeSingleKB() *fakeSingleKB {
	return &fakeSingleKB{
		accessOK: true,
		version:  7,
		kbName:   "sre",
	}
}

func (f *fakeSingleKB) GetKB(_ context.Context, name string) (*store.KB, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getKBCall = append(f.getKBCall, name)
	if name != f.kbName {
		return nil, store.ErrNotFound
	}
	return &store.KB{ID: 1, Name: f.kbName, OwnerSub: "owner-1", Version: f.version}, nil
}

func (f *fakeSingleKB) HasReadAccess(_ context.Context, _ int64, sub string, _ bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, sub)
	return f.accessOK, nil
}

func (f *fakeSingleKB) GetVersion(_ context.Context, _ int64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, nil
}

func (f *fakeSingleKB) GetKBDirByName(_ context.Context, _ int64, name string) (*store.Dir, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.dirs[name]; ok {
		return d, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeSingleKB) CreateTaskWithItems(_ context.Context, _, _ int64, texts []string, _ []string) (int64, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, texts)
	return 42, len(texts), nil
}

func (f *fakeSingleKB) kbCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.getKBCall...)
}

// fakeQdrantFanout 记录被检索的 collection 与 dir 过滤；按名返回单条 hit。
// scores 可按 collection 配置分数；nil 时默认（sre=0.9 / 其余 0.5）。
type fakeQdrantFanout struct {
	mu     sync.Mutex
	cols   []string
	dirs   []string
	fail   map[string]bool
	scores map[string]float64
}

func (f *fakeQdrantFanout) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	col := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/collections/"), "/points/search")
	f.mu.Lock()
	f.cols = append(f.cols, col)
	f.mu.Unlock()
	if f.fail[col] {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"err":"boom"}`))
		return
	}
	// legacy Search 请求体带 filter.must（dir/point_type 条件）——捕获 dir 值
	var body struct {
		Filter struct {
			Must []struct {
				Key   string `json:"key"`
				Match struct {
					Value any `json:"value"`
				} `json:"match"`
			} `json:"must"`
		} `json:"filter"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	for _, m := range body.Filter.Must {
		if m.Key == "dir" {
			if v, ok := m.Match.Value.(string); ok {
				f.mu.Lock()
				f.dirs = append(f.dirs, v)
				f.mu.Unlock()
			}
		}
	}
	score, ok := f.scores[col]
	if !ok {
		score = 0.5
		if col == "sre" {
			score = 0.9
		}
	}
	_, _ = w.Write([]byte(`{"result":[{"id":"p-` + col + `","score":` +
		fmt.Sprintf("%v", score) + `,"payload":{"text":"hit of ` + col + `","point_type":"clean"}}]}`))
}

// fakeEmbedder 起一个 aigateway OpenAI 兼容 /embeddings fake（1024d 固定向量）。
func fakeEmbedder(t *testing.T) *embedding.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		vec := make([]float64, embedding.Dim)
		b, _ := json.Marshal(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": vec}},
		})
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return embedding.New(srv.URL, "k", "", "", "m", 16, 0)
}

// fakeEmbedderFailing embedding 一律失败（502 传播断言用）。
func fakeEmbedderFailing(t *testing.T) *embedding.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return embedding.New(srv.URL, "k", "", "", "m", 16, 0)
}

// newSingleKBTestServer 带单库 Cfg（KB=sre）与注入 fake store 的测试 server。
func newSingleKBTestServer(t *testing.T, st singleKBStore, qh http.Handler) (*Server, *httptest.Server) {
	t.Helper()
	return newSingleKBTestServerCfg(t, st, qh, config.Config{KB: config.DefaultKB})
}

// newSingleKBTestServerCfg 自定义 Cfg（阈值/hybrid 等场景用）。
func newSingleKBTestServerCfg(t *testing.T, st singleKBStore, qh http.Handler, cfg config.Config) (*Server, *httptest.Server) {
	t.Helper()
	if cfg.KB == "" {
		cfg.KB = config.DefaultKB
	}
	s := &Server{
		Machine:  auth.NewMachineAuth("test-token"),
		Session:  auth.NewSession(time.Hour),
		Embedder: fakeEmbedder(t),
		SingleKB: st,
		Cfg:      cfg,
	}
	qsrv := httptest.NewServer(qh)
	t.Cleanup(qsrv.Close)
	s.Qdrant = qdrant.New(qsrv.URL, "", time.Second)
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func ask(t *testing.T, url string, body string, token string, cookie *http.Cookie) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/api/ask", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post ask: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, out
}

// 核心：请求 kb/global 字段被忽略——kb 写成别的名字也检索 cfg.KB（sre），
// store 收到的解析名恒为 cfg.KB，qdrant 只搜 sre collection。
func TestAsk_RequestKBFieldIgnored(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{}
	_, ts := newSingleKBTestServer(t, st, qh)

	// 旧 CLI 形态：kb="ops"（不存在的库）+ global=true
	code, out := ask(t, ts.URL, `{"kb":"ops","global":true,"query":"统一出口代理地址","top_k":1}`, "test-token", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, out)
	}
	// store 只解析 cfg.KB（sre），请求 kb="ops" 未被采用
	if calls := st.kbCalls(); len(calls) != 1 || calls[0] != "sre" {
		t.Fatalf("store must resolve cfg.KB \"sre\" only, got %v", calls)
	}
	// qdrant 只搜 sre collection
	if len(qh.cols) != 1 || qh.cols[0] != "sre" {
		t.Fatalf("must search exactly the single collection sre, got %v", qh.cols)
	}
	// 响应 kb=实际检索库名；global 字段为旧 wire 回显
	if out["kb"] != "sre" || out["global"] != true {
		t.Fatalf("response kb/global wrong: %v", out)
	}
	// 机器 token：sub 传递为 ""
	if st.subs[0] != "" {
		t.Fatalf("machine token must pass empty sub, got %q", st.subs[0])
	}
	hits, _ := out["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %v", out["hits"])
	}
	h0 := hits[0].(map[string]any)
	if h0["id"] != "p-sre" {
		t.Fatalf("hit must come from sre collection, got %v", h0["id"])
	}
	if out["version"] != float64(7) {
		t.Fatalf("version must come from single kb store, got %v", out["version"])
	}
}

// OIDC 用户：sub 透传权限检查；请求不带 kb/global（新 CLI 形态）同样只查 sre。
func TestAsk_OIDCUserSingleKB(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{}
	s, ts := newSingleKBTestServer(t, st, qh)
	sid, err := s.Session.Put("user-1")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	code, out := ask(t, ts.URL, `{"query":"x"}`, "", &http.Cookie{Name: "agmem_session", Value: sid})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, out)
	}
	if st.subs[0] != "user-1" {
		t.Fatalf("OIDC sub must be passed to access check, got %q", st.subs[0])
	}
	if len(qh.cols) != 1 || qh.cols[0] != "sre" {
		t.Fatalf("must search single collection sre, got %v", qh.cols)
	}
}

// dir 仍可收窄：用户目录名归一为 clean 镜像目录 `<dir>-clean` 后透传 qdrant
// filter（G05：README 示例 --dir k8s 必须命中 k8s-clean，不能 0 命中）。
func TestAsk_DirNarrowsScope(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{}
	// hybrid off → legacy Search，请求体带 filter
	_, ts := newSingleKBTestServerCfg(t, st, qh, config.Config{KB: config.DefaultKB, HybridEnabled: false})

	code, _ := ask(t, ts.URL, `{"query":"x","dir":"k8s"}`, "test-token", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if len(qh.dirs) != 1 || qh.dirs[0] != "k8s-clean" {
		t.Fatalf("user dir must be normalized to <dir>-clean, got %v", qh.dirs)
	}
	// 已带 -clean 的写法幂等（不重复加后缀）
	code, _ = ask(t, ts.URL, `{"query":"x","dir":"k8s-clean"}`, "test-token", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if len(qh.dirs) != 2 || qh.dirs[1] != "k8s-clean" {
		t.Fatalf("already-suffixed dir must stay as-is, got %v", qh.dirs)
	}
	// 不带 dir → 无 dir 过滤（全库）
	code, _ = ask(t, ts.URL, `{"query":"x"}`, "test-token", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if len(qh.dirs) != 2 {
		t.Fatalf("no-dir query must not add dir filter, got %v", qh.dirs)
	}
}

// normalizeQueryDir 纯函数（G05）：空保持空、加 -clean、已带后缀幂等。
func TestNormalizeQueryDir(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"k8s", "k8s-clean"},
		{"k8s-clean", "k8s-clean"},
		{"mysql", "mysql-clean"},
		{"导航问答沉淀", "导航问答沉淀-clean"},
		{"a-clean-clean", "a-clean-clean"}, // 后缀只补一次，不叠加
	}
	for _, c := range cases {
		if got := normalizeQueryDir(c.in); got != c.want {
			t.Errorf("normalizeQueryDir(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// query 必填；store 不得被无 query 请求触碰。
func TestAsk_RequiresQuery(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{}
	_, ts := newSingleKBTestServer(t, st, qh)

	code, out := ask(t, ts.URL, `{"query":""}`, "test-token", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("empty query: want 400, got %d: %v", code, out)
	}
	code, _ = ask(t, ts.URL, `{"kb":"sre"}`, "test-token", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("missing query: want 400, got %d", code)
	}
	// 旧契约「无 kb 报 400」已随单库化废除：只有 query 合法即受理
	if len(st.kbCalls()) != 0 {
		t.Fatalf("rejected requests must not touch store, got %v", st.kbCalls())
	}
}

// 无权限 → 404 掩蔽（不暴露 kb 存在性），且不消耗 embedding 配额。
func TestAsk_PermissionMasked404(t *testing.T) {
	st := newFakeSingleKB()
	st.accessOK = false
	qh := &fakeQdrantFanout{}
	s, ts := newSingleKBTestServer(t, st, qh)
	sid, err := s.Session.Put("stranger")
	if err != nil {
		t.Fatal(err)
	}
	code, out := ask(t, ts.URL, `{"query":"x"}`, "", &http.Cookie{Name: "agmem_session", Value: sid})
	if code != http.StatusNotFound {
		t.Fatalf("no access: want 404 masked, got %d: %v", code, out)
	}
	if len(qh.cols) != 0 {
		t.Fatalf("unauthorized ask must not reach qdrant, got %v", qh.cols)
	}
}

// kb 行不存在（部署漂移：cfg.KB 无对应记录）→ 404。
func TestAsk_KBMissing404(t *testing.T) {
	st := newFakeSingleKB()
	st.kbName = "other" // GetKB("sre") → ErrNotFound
	qh := &fakeQdrantFanout{}
	_, ts := newSingleKBTestServer(t, st, qh)
	code, out := ask(t, ts.URL, `{"query":"x"}`, "test-token", nil)
	if code != http.StatusNotFound {
		t.Fatalf("missing kb: want 404, got %d: %v", code, out)
	}
}

// embedding 失败 → 502。
func TestAsk_EmbedFailure502(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{}
	s, ts := newSingleKBTestServer(t, st, qh)
	// 换成必失败的 embedder
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer embedSrv.Close()
	s.Embedder = embedding.New(embedSrv.URL, "k", "", "", "m", 16, 0)

	code, out := ask(t, ts.URL, `{"query":"x"}`, "test-token", nil)
	if code != http.StatusBadGateway {
		t.Fatalf("embed failure: want 502, got %d: %v", code, out)
	}
}

// qdrant 检索失败 → 502（不静默降级半结果）。
func TestAsk_QdrantFailureIs502(t *testing.T) {
	st := newFakeSingleKB()
	qh := &fakeQdrantFanout{fail: map[string]bool{"sre": true}}
	_, ts := newSingleKBTestServer(t, st, qh)
	code, out := ask(t, ts.URL, `{"query":"x"}`, "test-token", nil)
	if code != http.StatusBadGateway {
		t.Fatalf("want 502 on qdrant failure, got %d: %v", code, out)
	}
}

// push 路径单库守卫（G36）：路径 {kb} 必须等于 cfg.KB——跨库写 404 且零副作用
// （绝不静默写入别的库）；cfg.KB 路径正常写入。owner-only 权限保持。
func TestUploadDocs_PathKBMustMatchSingleKB(t *testing.T) {
	st := newFakeSingleKB()
	st.dirs = map[string]*store.Dir{"mysql": {ID: 2, KBID: 1, Name: "mysql"}}
	qh := &fakeQdrantFanout{}
	_, ts := newSingleKBTestServer(t, st, qh)

	// 旧 CLI 形态（URL 写不存在的库名）→ 404，不触碰 store/qdrant
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/kb/wrongname/docs",
		strings.NewReader(`{"dir":"mysql","texts":["第一条"]}`))
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-kb upload: want 404, got %d", resp.StatusCode)
	}
	if len(st.created) != 0 || len(st.kbCalls()) != 0 {
		t.Fatalf("cross-kb upload must not touch store: created=%v kbCalls=%v", st.created, st.kbCalls())
	}

	// 正确路径 → 202，实际写入 cfg.KB
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/kb/sre/docs",
		strings.NewReader(`{"dir":"mysql","texts":["第一条"]}`))
	req2.Header.Set("Authorization", "Bearer test-token")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp2.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&out)
	if out["kb"] != "sre" || out["items"] != float64(1) {
		t.Fatalf("upload must target single kb sre, got %v", out)
	}
	if calls := st.kbCalls(); len(calls) == 0 || calls[0] != "sre" {
		t.Fatalf("upload must resolve cfg.KB sre, got %v", calls)
	}
	if len(st.created) != 1 || len(st.created[0]) != 1 || st.created[0][0] != "第一条" {
		t.Fatalf("texts must be forwarded, got %v", st.created)
	}
}

// push 全部重复（store 返回 ErrAllDuplicates）→ 200 status=duplicate 不建任务。
func TestUploadDocs_AllDuplicatesDuplicateStatus(t *testing.T) {
	st := newFakeSingleKB()
	st.dirs = map[string]*store.Dir{"mysql": {ID: 2, KBID: 1, Name: "mysql"}}
	qh := &fakeQdrantFanout{}
	// dup 嵌入 fakeSingleKB，仅覆写 CreateTaskWithItems 模拟全部重复
	dup := &fakeDupStore{fakeSingleKB: st}
	_, ts := newSingleKBTestServer(t, dup, qh)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/kb/sre/docs",
		strings.NewReader(`{"dir":"mysql","texts":["dup"]}`))
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("all-duplicates: want 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["task_id"] != float64(0) || out["status"] != "duplicate" {
		t.Fatalf("want task_id=0 status=duplicate, got %v", out)
	}
}

// fakeDupStore 覆写 CreateTaskWithItems 返回 ErrAllDuplicates（全重复场景）。
type fakeDupStore struct {
	*fakeSingleKB
}

func (f *fakeDupStore) CreateTaskWithItems(_ context.Context, _, _ int64, _ []string, _ []string) (int64, int, error) {
	return 0, 0, store.ErrAllDuplicates
}
