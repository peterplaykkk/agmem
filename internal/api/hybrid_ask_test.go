package api

// 混合检索（dense + bm25 稀疏 RRF）查询路径测试：
//   - filterHybridHits 纯函数：保留条件 = DenseScore ≥ 阈值 或 SparseStrong
//     （词法强命中由 qdrant 层判定，见 internal/qdrant）；排序恒用 Score（rrf）；
//   - ask 查询侧走 EmbedQuery：注入 QueryInstruction 后 /embeddings 请求带指令前缀；
//   - HTTP 单库路径（2026-09-08 单库化，原跨 kb fan-out 退役）：hybrid on 时走
//     /points/query（rrf + dense/sparse 分数回查 + 并集阈值过滤），hybrid off
//     （AGMEM_HYBRID=off 回退阀）只走 /points/search 且阈值回到 Score。
// Store 依赖经 fakeSingleKB 注入免 MySQL；qdrant 层行为见 internal/qdrant。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sre/agmem/internal/config"
	"sre/agmem/internal/embedding"
	"sre/agmem/internal/qdrant"
)

// filterHybridHits 纯函数：dense 阈值边界（=阈值保留）+ SparseStrong 并集语义；
// rrf 分再高、dense 不达标且非强词法也必须丢弃——两路都看不见的点位宁可漏过
// 不误放（UC-1 宁缺毋滥）。SparseStrong 的 rank/score 门槛判定在 qdrant 层
// （internal/qdrant TestSearchHybrid_SparseStrong），本层只消费结果。
func TestFilterHybridHits(t *testing.T) {
	hits := []qdrant.SearchHit{
		{ID: "rrf高但无词法", Score: 0.9, DenseScore: 0.2},                       // 词法缺位、语义弱 → 丢
		{ID: "sparse强命中", Score: 0.8, DenseScore: 0.3, SparseStrong: true},  // 3128 回归形态：强词法、dense 不达标 → 保留
		{ID: "dense恰在阈值", Score: 0.7, DenseScore: 0.5},                      // 纯 dense 达标（无稀疏数据）→ 保留
		{ID: "双达标", Score: 0.6, DenseScore: 0.8, SparseStrong: true},        // 两路都强 → 保留
		{ID: "sparse弱命中", Score: 0.4, DenseScore: 0.1, SparseStrong: false}, // 非强词法 → 丢
		{ID: "双低", Score: 0.1, DenseScore: 0.1},                             // 丢
	}
	cases := []struct {
		name      string
		threshold float64
		wantIDs   []string
	}{
		// 输入即 rrf 序；过滤不改序 → wantIDs 也按 rrf 序
		{name: "阈值0.5并集语义", threshold: 0.5, wantIDs: []string{"sparse强命中", "dense恰在阈值", "双达标"}},
		{name: "阈值0.7纯dense达标并保留词法豁免", threshold: 0.7, wantIDs: []string{"sparse强命中", "双达标"}},
		{name: "阈值0.95词法豁免仍独立生效（并集与阈值取值无关）", threshold: 0.95, wantIDs: []string{"sparse强命中", "双达标"}},
		{name: "rrf排序不变", threshold: 0, wantIDs: []string{"rrf高但无词法", "sparse强命中", "dense恰在阈值", "双达标", "sparse弱命中", "双低"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterHybridHits(hits, c.threshold)
			if got == nil {
				t.Fatal("filter must return non-nil slice (JSON [] not null)")
			}
			var ids []string
			for _, h := range got {
				ids = append(ids, h.ID)
			}
			if strings.Join(ids, ",") != strings.Join(c.wantIDs, ",") {
				t.Fatalf("want %v, got %v", c.wantIDs, ids)
			}
		})
	}
	t.Run("空入参返回空切片", func(t *testing.T) {
		got := filterHybridHits(nil, 0.5)
		if got == nil || len(got) != 0 {
			t.Fatalf("want non-nil empty, got %#v", got)
		}
	})
}

// filterHybridHits SparseStrong 边界：SparseStrong 真即保留（与 dense 无关）；
// 假且 dense 不达标则丢弃；无稀疏数据恒不豁免。
func TestFilterHybridHits_SparseStrong(t *testing.T) {
	cases := []struct {
		name string
		hit  qdrant.SearchHit
		keep bool
	}{
		{
			name: "strong真_dense不达标_保留",
			hit:  qdrant.SearchHit{ID: "强词法", DenseScore: 0.1, SparseStrong: true},
			keep: true,
		},
		{
			name: "strong假_dense不达标_丢弃",
			hit:  qdrant.SearchHit{ID: "弱词法", DenseScore: 0.1, SparseStrong: false},
			keep: false,
		},
		{
			name: "strong假_dense达标_走dense路径保留",
			hit:  qdrant.SearchHit{ID: "纯dense", DenseScore: 0.5, SparseStrong: false},
			keep: true,
		},
		{
			name: "无稀疏数据_dense不达标_必不豁免",
			hit:  qdrant.SearchHit{ID: "无词法", DenseScore: 0.4, SparseStrong: false},
			keep: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterHybridHits([]qdrant.SearchHit{c.hit}, 0.5)
			if c.keep && len(got) != 1 {
				t.Fatalf("want kept, got %v", got)
			}
			if !c.keep && len(got) != 0 {
				t.Fatalf("want filtered, got %v", got)
			}
		})
	}
}

// hybridQdrant 混合检索 fake：/points/query 按 collection 返回 rrf 分（带
// prefetch 的融合请求，响应形状 = 真实 qdrant v1.19 prefetch+fusion 的
// {"result":{"points":[...]}}）；无 prefetch 的 sparse 回查请求返回 sparse 分数
// （同样包裹形状，Qdrant v1.19.0 实测）；/points/search 返回 dense 分并
// 校验 vector 非空（off 分支漏传 vector=null 的回归锚，2026-09-08）；
// 记录各端点命中次数。
type hybridQdrant struct {
	mu       sync.Mutex
	rrf      map[string]float64 // collection → 融合分
	dense    map[string]float64 // collection → dense 分
	sparse   map[string]float64 // collection → sparse 分（唯一命中点 rank0；缺省 0 = 无词法证据）
	fail     map[string]bool    // collection → /points/query 500
	dirs     []string           // 融合请求 top-level filter 的 dir 值（G05 归一断言）
	queryOK  int                // /points/query 融合请求成功次数（回退阀断言用）
	sparseOK int                // /points/query sparse 回查成功次数
	searchOK int                // /points/search 成功次数
}

func (h *hybridQdrant) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	col := strings.TrimPrefix(r.URL.Path, "/collections/")
	col = strings.TrimSuffix(col, "/points/query")
	col = strings.TrimSuffix(col, "/points/search")
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/points/query") {
		var body struct {
			Prefetch []any `json:"prefetch"`
			Filter   struct {
				Must []struct {
					Key   string `json:"key"`
					Match struct {
						Value any `json:"value"`
					} `json:"match"`
				} `json:"must"`
			} `json:"filter"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if h.fail[col] {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"err":"boom"}`))
			return
		}
		if len(body.Prefetch) == 0 {
			// sparse 分数回查（无 prefetch）：唯一命中点 rank0；sparse 未记录 =
			// 无匹配点（真实 qdrant 不回零分点 → 空结果 = 无词法证据）
			h.sparseOK++
			if sc, ok := h.sparse[col]; ok {
				_, _ = w.Write([]byte(`{"result":{"points":[{"id":"p-` + col + `","score":` +
					fnum(sc) + `}]},"status":"ok"}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":{"points":[]},"status":"ok"}`))
			return
		}
		h.queryOK++
		for _, m := range body.Filter.Must {
			if m.Key == "dir" {
				if v, ok := m.Match.Value.(string); ok {
					h.dirs = append(h.dirs, v)
				}
			}
		}
		score, ok := h.rrf[col]
		if !ok {
			score = 0.5
		}
		_, _ = w.Write([]byte(`{"result":{"points":[{"id":"p-` + col + `","score":` +
			fnum(score) + `,"payload":{"text":"hit of ` + col + `","point_type":"clean"}}]},"status":"ok"}`))
		return
	}
	var body struct {
		Vector []float32 `json:"vector"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if len(body.Vector) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":{"error":"vector must be non-empty (off-path regression)"}}`))
		return
	}
	h.searchOK++
	score, ok := h.dense[col]
	if !ok {
		score = 0.5
	}
	_, _ = w.Write([]byte(`{"result":[{"id":"p-` + col + `","score":` +
		fnum(score) + `,"payload":{"text":"hit of ` + col + `","point_type":"clean"}}]}`))
}

func fnum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// newHybridTestServer hybrid Cfg 的单库测试 server（KB=sre；store 用 fakeSingleKB，
// qdrant 指向 fake）。
func newHybridTestServer(t *testing.T, st *fakeSingleKB, qh http.Handler, hybrid bool, threshold float64) *httptest.Server {
	t.Helper()
	s := &Server{
		Machine:  testServer().Machine,
		Session:  testServer().Session,
		Embedder: fakeEmbedder(t),
		SingleKB: st,
		Cfg:      config.Config{KB: config.DefaultKB, ScoreThreshold: threshold, HybridEnabled: hybrid},
	}
	qsrv := httptest.NewServer(qh)
	t.Cleanup(qsrv.Close)
	s.Qdrant = qdrant.New(qsrv.URL, "", time.Second)
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return ts
}

// 单库 hybrid：dense 阈值过滤生效（rrf 分不参与阈值）+ dense/sparse 分数透出。
// 多 hit 的 rrf 排序/过滤语义由 filterHybridHits 纯函数测试覆盖（TestFilterHybridHits）。
func TestAsk_HybridDenseThresholdFilters(t *testing.T) {
	st := newFakeSingleKB()
	qh := &hybridQdrant{
		rrf:   map[string]float64{"sre": 0.83},
		dense: map[string]float64{"sre": 0.3}, // dense 不达标 → 过滤（无 sparse 证据）
	}
	ts := newHybridTestServer(t, st, qh, true, 0.5)

	_, out := ask(t, ts.URL, `{"query":"怎么推代码"}`, "test-token", nil)
	if hits, _ := out["hits"].([]any); len(hits) != 0 {
		t.Fatalf("dense below threshold with no sparse evidence must be filtered, got %v", out["hits"])
	}
	// 单 collection：融合 query + sparse 回查 + dense search 各一次
	if qh.queryOK != 1 || qh.sparseOK != 1 || qh.searchOK != 1 {
		t.Fatalf("hybrid path must fuse+lookup(sparse,dense) once for the single collection, got query=%d sparse=%d search=%d",
			qh.queryOK, qh.sparseOK, qh.searchOK)
	}

	// dense 达标 → 保留，dense_score 透出
	qh2 := &hybridQdrant{
		rrf:    map[string]float64{"sre": 0.83},
		dense:  map[string]float64{"sre": 0.9},
		sparse: map[string]float64{"sre": 4.0}, // 词法弱（不触发豁免），分数照常透出
	}
	ts2 := newHybridTestServer(t, st, qh2, true, 0.5)
	_, out2 := ask(t, ts2.URL, `{"query":"怎么推代码"}`, "test-token", nil)
	hits2, _ := out2["hits"].([]any)
	if len(hits2) != 1 {
		t.Fatalf("dense above threshold must be kept, got %v", out2["hits"])
	}
	h0 := hits2[0].(map[string]any)
	if h0["id"] != "p-sre" || h0["dense_score"] != 0.9 {
		t.Fatalf("rank0 = %v, want p-sre with dense_score 0.9", h0)
	}
	if v, ok := h0["sparse_score"].(float64); !ok || v != 4.0 {
		t.Fatalf("p-sre sparse_score = %v, want 4.0", h0["sparse_score"])
	}
}

// hybrid 词法豁免（3128 回归钉死）：dense 0.3 不达标但 bm25 rank0 score
// 9.9 强词法命中必须保留，且 wire 上 sparse_rank=0 不得被 omitempty 吞掉
// （rank 0 是有效名次——这正是本豁免要救回的查询形态）。
func TestAsk_HybridSparseExemptKept(t *testing.T) {
	st := newFakeSingleKB()
	qh := &hybridQdrant{
		rrf:    map[string]float64{"sre": 0.83},
		dense:  map[string]float64{"sre": 0.3}, // dense 阈值不达标
		sparse: map[string]float64{"sre": 9.9}, // 3128 形态：rank0 9.9
	}
	ts := newHybridTestServer(t, st, qh, true, 0.5)

	_, out := ask(t, ts.URL, `{"query":"npm go pip 依赖下载走哪个端口"}`, "test-token", nil)
	hits, _ := out["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("strong lexical hit must survive dense threshold, got %v", out["hits"])
	}
	h0 := hits[0].(map[string]any)
	if h0["id"] != "p-sre" {
		t.Fatalf("kept hit = %v, want p-sre", h0)
	}
	// map 键不存在 ⇔ JSON omitempty 吞字段：sparse_rank=0 必须存在
	if _, ok := h0["sparse_rank"]; !ok {
		t.Fatalf("sparse_rank 0 must be serialized (omitempty must not swallow rank 0), got %v", h0)
	}
	if h0["sparse_rank"].(float64) != 0 || h0["sparse_score"].(float64) != 9.9 {
		t.Fatalf("sparse evidence = %v/%v, want rank0/9.9", h0["sparse_rank"], h0["sparse_score"])
	}
}

// hybrid 回退阀：AGMEM_HYBRID=off → 只走 /points/search，阈值回到
// Score（cosine）——存量部署未回填稀疏向量期间的安全阀行为钉死。
func TestAsk_HybridOffFallsBackToLegacy(t *testing.T) {
	st := newFakeSingleKB()
	// sre 0.9（rrf 槽位故意写高分，若误走 hybrid 会干扰 dense 判断）
	qh := &hybridQdrant{
		rrf:   map[string]float64{"sre": 0.99},
		dense: map[string]float64{"sre": 0.9},
	}
	ts := newHybridTestServer(t, st, qh, false, 0.5)

	_, out := ask(t, ts.URL, `{"query":"x"}`, "test-token", nil)
	if qh.queryOK != 0 {
		t.Fatalf("hybrid=off must not call /points/query, got %d calls", qh.queryOK)
	}
	hits, _ := out["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("legacy threshold on Score: want only 0.9 hit, got %v", out["hits"])
	}
	h0 := hits[0].(map[string]any)
	if h0["id"] != "p-sre" {
		t.Fatalf("kept hit = %v, want p-sre", h0)
	}
	// off 路径不带任何 hybrid 分数字段（omitempty wire 与旧响应一致）
	for _, k := range []string{"dense_score", "sparse_rank", "sparse_score"} {
		if _, ok := h0[k]; ok {
			t.Fatalf("legacy off-path hit must not carry %s, got %v", k, h0)
		}
	}
}

// ask 查询侧走 EmbedQuery（2026-09-09 检索质量批）：注入 QueryInstruction 后
// /embeddings 请求 input 带指令前缀——查询侧非对称检索保证（写入侧见
// cleaner.TestProcessPendingEmbedPathIgnoresQueryInstruction）。
func TestAsk_SendsQueryInstructionPrefix(t *testing.T) {
	var mu sync.Mutex
	var inputs [][]string
	embSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		inputs = append(inputs, req.Input)
		mu.Unlock()
		vec := make([]float64, embedding.Dim)
		b, _ := json.Marshal(map[string]any{"data": []map[string]any{{"index": 0, "embedding": vec}}})
		_, _ = w.Write(b)
	}))
	defer embSrv.Close()
	emb := embedding.New(embSrv.URL, "k", "", "", "m", 16, 0)
	emb.QueryInstruction = "Instruct: X\nQuery: "

	st := newFakeSingleKB()
	s := &Server{
		Machine:  testServer().Machine,
		Session:  testServer().Session,
		Embedder: emb,
		SingleKB: st,
		Cfg:      config.Config{KB: config.DefaultKB, ScoreThreshold: 0.5, HybridEnabled: false},
	}
	qsrv := httptest.NewServer(&fakeQdrantFanout{})
	defer qsrv.Close()
	s.Qdrant = qdrant.New(qsrv.URL, "", time.Second)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	code, out := ask(t, ts.URL, `{"query":"怎么推代码"}`, "test-token", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, out)
	}
	if len(inputs) != 1 || len(inputs[0]) != 1 || inputs[0][0] != "Instruct: X\nQuery: 怎么推代码" {
		t.Fatalf("query embedding input must carry instruction prefix, got %v", inputs)
	}
}

// hybrid on 但 qdrant /points/query 失败（如存量 collection 缺 bm25 配置）→
// 502 显式报错，不静默降级稠密结果（降级只经 AGMEM_HYBRID=off 显式控制）。
func TestAsk_HybridQueryFailureFails(t *testing.T) {
	st := newFakeSingleKB()
	qh := &hybridQdrant{fail: map[string]bool{"sre": true}}
	ts := newHybridTestServer(t, st, qh, true, 0.5)

	code, out := ask(t, ts.URL, `{"query":"x"}`, "test-token", nil)
	if code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %v", code, out)
	}
	if !strings.Contains(out["error"].(string), "qdrant") {
		t.Fatalf("error must mention qdrant, got %v", out)
	}
}

// G05：hybrid（默认路径）下用户目录名同样归一为 <dir>-clean 并透传融合请求 filter。
func TestAsk_HybridDirNormalized(t *testing.T) {
	st := newFakeSingleKB()
	qh := &hybridQdrant{
		rrf:   map[string]float64{"sre": 0.83},
		dense: map[string]float64{"sre": 0.9},
	}
	ts := newHybridTestServer(t, st, qh, true, 0.5)

	_, out := ask(t, ts.URL, `{"query":"x","dir":"k8s"}`, "test-token", nil)
	if len(qh.dirs) != 1 || qh.dirs[0] != "k8s-clean" {
		t.Fatalf("hybrid dir filter must be normalized to k8s-clean, got %v", qh.dirs)
	}
	if hits, _ := out["hits"].([]any); len(hits) != 1 {
		t.Fatalf("want 1 hit, got %v", out["hits"])
	}
}

// G06：--debug/--json 可观测的过滤诊断——hybrid 丢弃命中时 filter 段给出
// mode/threshold/rule/kept/dropped/reason，把「dense≥阈值 或 SparseStrong」钉死。
func TestAsk_HybridFilterDiag(t *testing.T) {
	t.Run("dense不达标且非强词法_丢弃并给出原因", func(t *testing.T) {
		st := newFakeSingleKB()
		qh := &hybridQdrant{
			rrf:   map[string]float64{"sre": 0.83},
			dense: map[string]float64{"sre": 0.3},
		}
		ts := newHybridTestServer(t, st, qh, true, 0.5)
		_, out := ask(t, ts.URL, `{"query":"x"}`, "test-token", nil)
		if hits, _ := out["hits"].([]any); len(hits) != 0 {
			t.Fatalf("want filtered, got %v", out["hits"])
		}
		f, ok := out["filter"].(map[string]any)
		if !ok {
			t.Fatalf("filter diag missing: %v", out)
		}
		if f["mode"] != "hybrid" || f["threshold"] != 0.5 || f["dropped"] != float64(1) || f["kept"] != float64(0) {
			t.Fatalf("filter diag wrong: %v", f)
		}
		if !strings.Contains(f["rule"].(string), "sparse_strong") {
			t.Fatalf("rule must state hybrid union semantics, got %v", f["rule"])
		}
		if !strings.Contains(f["dropped_reason"].(string), "dense_score") {
			t.Fatalf("dropped_reason must explain dense threshold, got %v", f["dropped_reason"])
		}
	})
	t.Run("dense达标_保留且无丢弃原因", func(t *testing.T) {
		st := newFakeSingleKB()
		qh := &hybridQdrant{
			rrf:   map[string]float64{"sre": 0.83},
			dense: map[string]float64{"sre": 0.9},
		}
		ts := newHybridTestServer(t, st, qh, true, 0.5)
		_, out := ask(t, ts.URL, `{"query":"x"}`, "test-token", nil)
		f, ok := out["filter"].(map[string]any)
		if !ok {
			t.Fatalf("filter diag missing: %v", out)
		}
		if f["kept"] != float64(1) || f["dropped"] != float64(0) {
			t.Fatalf("want kept=1 dropped=0, got %v", f)
		}
		if _, present := f["dropped_reason"]; present {
			t.Fatalf("no dropped hits → dropped_reason must be omitted, got %v", f)
		}
	})
}

// filterHybridHitsDiag 纯函数：诊断字段与保留集合一致。
func TestFilterHybridHitsDiag(t *testing.T) {
	hits := []qdrant.SearchHit{
		{ID: "keep-dense", DenseScore: 0.8},
		{ID: "keep-lexical", DenseScore: 0.2, SparseStrong: true},
		{ID: "drop", DenseScore: 0.2},
	}
	kept, d := filterHybridHitsDiag(hits, 0.5)
	if len(kept) != 2 || kept[0].ID != "keep-dense" || kept[1].ID != "keep-lexical" {
		t.Fatalf("kept = %v", kept)
	}
	if d.Mode != "hybrid" || d.Threshold != 0.5 || d.Kept != 2 || d.Dropped != 1 || d.DroppedReason == "" {
		t.Fatalf("diag = %+v", d)
	}
	// 全部保留 → 不产生丢弃原因
	_, d2 := filterHybridHitsDiag(hits[:2], 0.5)
	if d2.Kept != 2 || d2.Dropped != 0 || d2.DroppedReason != "" {
		t.Fatalf("diag2 = %+v", d2)
	}
}
