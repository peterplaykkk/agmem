package qdrant

// 混合检索（dense + bm25 稀疏 RRF）客户端行为测试：
// wire 形状（以 qdrant v1.19.0 OpenAPI 确认形状为准）、rrf 分数与 dense 分数
// 双轨、空稀疏退化、fan-out 合并、稀疏部分更新分批。

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
)

// TestPointMarshalJSON 点位 wire 双形态：仅稠密 = 数组（存量兼容）；带稀疏 =
// 命名向量 map，默认稠密向量内部名 ""（v1.19.0 DEFAULT_VECTOR_NAME）。
func TestPointMarshalJSON(t *testing.T) {
	t.Run("仅稠密保持数组形态", func(t *testing.T) {
		b, err := json.Marshal(Point{ID: "p1", Vector: []float32{1, 2}})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]any{}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if _, isMap := got["vector"].(map[string]any); isMap {
			t.Fatalf("dense-only vector must stay array, got %s", b)
		}
		if !strings.Contains(string(b), `"vector":[1,2]`) {
			t.Fatalf("vector array missing: %s", b)
		}
	})

	t.Run("带稀疏走命名向量map", func(t *testing.T) {
		b, err := json.Marshal(Point{
			ID:     "p2",
			Vector: []float32{0.5},
			Sparse: map[string]SparseVector{
				SparseVectorName: {Indices: []uint32{7, 9}, Values: []float32{1, 1.5}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			ID     string                     `json:"id"`
			Vector map[string]json.RawMessage `json:"vector"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got.Vector[""]; !ok {
			t.Fatalf("dense must be addressed by empty-string name: %s", b)
		}
		if string(got.Vector[""]) != `[0.5]` {
			t.Fatalf("dense value = %s, want [0.5]", got.Vector[""])
		}
		var sv SparseVector
		if err := json.Unmarshal(got.Vector[SparseVectorName], &sv); err != nil {
			t.Fatal(err)
		}
		if len(sv.Indices) != 2 || sv.Indices[0] != 7 || sv.Values[1] != 1.5 {
			t.Fatalf("sparse roundtrip wrong: %s", b)
		}
		// "-" 字段不得泄漏到 wire
		if strings.Contains(string(b), `"sparse":`) {
			t.Fatalf("internal Sparse field leaked to wire: %s", b)
		}
	})
}

// TestEnsureCollection_NewCollectionHasBM25 新建即带 bm25 稀疏向量。
func TestEnsureCollection_NewCollectionHasBM25(t *testing.T) {
	var createBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collections/kb1"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":"error","result":null}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/kb1":
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			_, _ = w.Write([]byte(`{"result":true,"status":"ok"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	if err := c.EnsureCollection(context.Background(), "kb1", 1024); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	if createBody["vectors"].(map[string]any)["size"].(float64) != 1024 {
		t.Fatalf("dense config missing: %v", createBody)
	}
	sv, ok := createBody["sparse_vectors"].(map[string]any)["bm25"].(map[string]any)
	if !ok {
		t.Fatalf("sparse_vectors.bm25 missing: %v", createBody)
	}
	idx := sv["index"].(map[string]any)
	if idx["datatype"] != "float32" {
		t.Fatalf("sparse index datatype = %v, want float32", idx["datatype"])
	}
}

// TestEnsureSparseVector_ExistingCollection 存量 collection 走「Create named
// vector」增量补齐；已配置时幂等跳过。
func TestEnsureSparseVector_ExistingCollection(t *testing.T) {
	var mu sync.Mutex
	var putVectorPaths []string
	var putVectorBodies []map[string]any
	withSparse := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collections/kb1"):
			cfg := `{"result":{"config":{"params":{"vectors":{"size":1024,"distance":"Cosine"}`
			if withSparse {
				cfg += `,"sparse_vectors":{"bm25":{"index":{"datatype":"float32"}}}`
			}
			cfg += `}}}}`
			_, _ = w.Write([]byte(cfg))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collections/kb-missing"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":"error"}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/vectors/bm25"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			putVectorPaths = append(putVectorPaths, r.URL.Path)
			putVectorBodies = append(putVectorBodies, body)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"result":true,"status":"ok"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)

	t.Run("缺失时PUT命名向量补齐", func(t *testing.T) {
		if err := c.EnsureSparseVector(context.Background(), "kb1"); err != nil {
			t.Fatalf("EnsureSparseVector: %v", err)
		}
		if len(putVectorPaths) != 1 || !strings.HasSuffix(putVectorPaths[0], "/collections/kb1/vectors/bm25") {
			t.Fatalf("want PUT /collections/kb1/vectors/bm25, got %v", putVectorPaths)
		}
		sp := putVectorBodies[0]["sparse"].(map[string]any)
		if sp["datatype"] != "float32" {
			t.Fatalf("sparse config = %v, want datatype float32", sp)
		}
	})

	t.Run("已配置时幂等跳过", func(t *testing.T) {
		withSparse = true
		before := len(putVectorPaths)
		if err := c.EnsureSparseVector(context.Background(), "kb1"); err != nil {
			t.Fatalf("EnsureSparseVector idempotent: %v", err)
		}
		if len(putVectorPaths) != before {
			t.Fatalf("already-configured collection must not be PUT again")
		}
	})

	t.Run("collection不存在报错不建库", func(t *testing.T) {
		err := c.EnsureSparseVector(context.Background(), "kb-missing")
		if err == nil || !strings.Contains(err.Error(), "kb-missing") {
			t.Fatalf("missing collection must error naming it, got %v", err)
		}
	})
}

// TestSearchHybrid_RequestShapeAndScoreDualTrack 验证：
//  1. /points/query 融合请求形状 = prefetch[dense, sparse(bm25)] + fusion rrf + 过滤器；
//  2. hit.Score=rrf 融合分、DenseScore 来自 legacy search 回查、SparseRank/SparseScore
//     来自 sparse-only query 回查（rank 按分数降序，0=榜首）、SparseStrong 由相对门槛判定；
//  3. sparse 回查请求形状 = 无 prefetch + using=bm25 + with_payload=false；
//  4. rrf 降序排序不变。
func TestSearchHybrid_RequestShapeAndScoreDualTrack(t *testing.T) {
	var fusedBody, sparseBody, searchBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/query"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["prefetch"]; ok {
				fusedBody = body
				// 真实 qdrant v1.19 形状：带 prefetch+fusion 时 result 为
				// {"points":[...]} 包裹对象（2026-09-08 实测），不是裸数组
				_, _ = w.Write([]byte(`{"result":{"points":[
					{"id":"hit-b","score":0.6667,"payload":{"text":"词法命中","point_type":"clean"}},
					{"id":"hit-a","score":0.5,"payload":{"text":"稠密命中","point_type":"clean"}}
				]},"status":"ok","time":0.1}`))
				return
			}
			// 无 prefetch = sparse 分数/名次回查；实测（Qdrant v1.19.0
			// 2026-09-08）同样返回 {"points":[...]} 包裹形状
			sparseBody = body
			_, _ = w.Write([]byte(`{"result":{"points":[
				{"id":"hit-b","score":9.9},
				{"id":"hit-a","score":5.5}
			]},"status":"ok"}`))
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			_ = json.NewDecoder(r.Body).Decode(&searchBody)
			_, _ = w.Write([]byte(`{"result":[
				{"id":"hit-a","score":0.9,"payload":{"text":"稠密命中","point_type":"clean"}},
				{"id":"hit-b","score":0.3,"payload":{"text":"词法命中","point_type":"clean"}}
			],"status":"ok"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)

	hits, err := c.SearchHybrid(context.Background(), "kb1",
		[]float32{0.1, 0.2},
		SparseVector{Indices: []uint32{1, 2}, Values: []float32{1, 1}},
		SearchRequest{Limit: 5, Dir: "sre-clean", PointType: "clean"})
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}

	// 融合请求形状（OpenAPI 确认形状）
	prefetch, ok := fusedBody["prefetch"].([]any)
	if !ok || len(prefetch) != 2 {
		t.Fatalf("prefetch must be 2 branches, got %v", fusedBody["prefetch"])
	}
	dense := prefetch[0].(map[string]any)
	sparse := prefetch[1].(map[string]any)
	if _, has := dense["using"]; has {
		t.Errorf("dense prefetch must omit using (default vector), got %v", dense["using"])
	}
	if _, ok := dense["query"].([]any); !ok {
		t.Errorf("dense prefetch query must be array, got %v", dense["query"])
	}
	if sparse["using"] != SparseVectorName {
		t.Errorf("sparse prefetch using = %v, want bm25", sparse["using"])
	}
	sq := sparse["query"].(map[string]any)
	if len(sq["indices"].([]any)) != 2 || len(sq["values"].([]any)) != 2 {
		t.Errorf("sparse prefetch query shape wrong: %v", sq)
	}
	for _, p := range []map[string]any{dense, sparse} {
		if p["limit"].(float64) != hybridPrefetchLimit {
			t.Errorf("prefetch limit = %v, want %d", p["limit"], hybridPrefetchLimit)
		}
		f := p["filter"].(map[string]any)
		if !strings.Contains(mustJSON(f), `"sre-clean"`) {
			t.Errorf("prefetch filter must carry dir, got %v", f)
		}
	}
	if fusedBody["query"].(map[string]any)["fusion"] != "rrf" {
		t.Errorf("fusion must be rrf, got %v", fusedBody["query"])
	}
	if fusedBody["limit"].(float64) != 5 {
		t.Errorf("top-level limit = %v, want 5", fusedBody["limit"])
	}
	if fusedBody["with_payload"] != true {
		t.Errorf("with_payload must be true")
	}
	// sparse 回查请求形状：无 prefetch、using=bm25、同 limit、with_payload=false
	if _, has := sparseBody["prefetch"]; has {
		t.Errorf("sparse lookup must not carry prefetch, got %v", sparseBody["prefetch"])
	}
	if sparseBody["using"] != SparseVectorName {
		t.Errorf("sparse lookup using = %v, want bm25", sparseBody["using"])
	}
	if sparseBody["limit"].(float64) != float64(hybridPrefetchLimit) {
		t.Errorf("sparse lookup limit = %v, want %d", sparseBody["limit"], hybridPrefetchLimit)
	}
	if sparseBody["with_payload"] != false {
		t.Errorf("sparse lookup with_payload must be false (id/score only)")
	}
	sb := sparseBody["query"].(map[string]any)
	if len(sb["indices"].([]any)) != 2 {
		t.Errorf("sparse lookup query vector shape wrong: %v", sb)
	}
	if !strings.Contains(mustJSON(sparseBody["filter"]), `"sre-clean"`) {
		t.Errorf("sparse lookup filter must match hybrid filter")
	}
	// dense 分数回查：同 filter 同 prefetch 条数
	if searchBody["limit"].(float64) != float64(hybridPrefetchLimit) {
		t.Errorf("dense lookup limit = %v, want %d", searchBody["limit"], hybridPrefetchLimit)
	}
	if !strings.Contains(mustJSON(searchBody["filter"]), `"sre-clean"`) {
		t.Errorf("dense lookup filter must match hybrid filter")
	}

	// 三轨分数（rrf / dense / sparse）+ rrf 降序（0.6667 > 0.5）
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %+v", hits)
	}
	if hits[0].ID != "hit-b" || hits[1].ID != "hit-a" {
		t.Fatalf("hits must be rrf-descending, got %+v", hits)
	}
	if hits[0].Score != 0.6667 || hits[0].DenseScore != 0.3 {
		t.Errorf("hit-b Score/DenseScore = %v/%v, want 0.6667/0.3", hits[0].Score, hits[0].DenseScore)
	}
	if hits[1].Score != 0.5 || hits[1].DenseScore != 0.9 {
		t.Errorf("hit-a Score/DenseScore = %v/%v, want 0.5/0.9", hits[1].Score, hits[1].DenseScore)
	}
	// sparse 名次按回查分数降序：hit-b 9.9 → rank 0；hit-a 5.5 → rank 1
	if hits[0].SparseRank == nil || *hits[0].SparseRank != 0 || hits[0].SparseScore != 9.9 {
		t.Errorf("hit-b SparseRank/SparseScore = %v/%v, want 0/9.9", hits[0].SparseRank, hits[0].SparseScore)
	}
	if hits[1].SparseRank == nil || *hits[1].SparseRank != 1 || hits[1].SparseScore != 5.5 {
		t.Errorf("hit-a SparseRank/SparseScore = %v/%v, want 1/5.5", hits[1].SparseRank, hits[1].SparseScore)
	}
	// SparseStrong 按该池相对门槛：top=9.9；hit-b rank0 9.9 ≥ 0.6×9.9 且 ≥1.0
	// → strong；hit-a rank1 5.5 < 5.94 → 非 strong
	if !hits[0].SparseStrong {
		t.Errorf("hit-b rank0 9.9 must be SparseStrong, got %+v", hits[0])
	}
	if hits[1].SparseStrong {
		t.Errorf("hit-a rank1 5.5 below 0.6×top must not be SparseStrong, got %+v", hits[1])
	}
}

// TestSearchHybrid_SparseRankFollowsScoreOrder sparse 回查响应乱序（服务端不承诺
// 排序的场景兜底）时，名次仍必须按分数降序分配（rank 0 = 分数最高）。
func TestSearchHybrid_SparseRankFollowsScoreOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/query"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["prefetch"]; ok {
				_, _ = w.Write([]byte(`{"result":{"points":[
					{"id":"low","score":0.4},{"id":"high","score":0.6}
				]},"status":"ok"}`))
				return
			}
			// 故意乱序：low(5.0) 在前、high(9.9) 在后
			_, _ = w.Write([]byte(`{"result":{"points":[
				{"id":"low","score":5.0},{"id":"high","score":9.9}
			]},"status":"ok"}`))
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			_, _ = w.Write([]byte(`{"result":[]}`))
		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	hits, err := c.SearchHybrid(context.Background(), "kb1", []float32{0.1},
		SparseVector{Indices: []uint32{1}, Values: []float32{1}}, SearchRequest{Limit: 3})
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	byID := map[string]*SearchHit{}
	for i := range hits {
		byID[hits[i].ID] = &hits[i]
	}
	if h := byID["high"]; h.SparseRank == nil || *h.SparseRank != 0 || h.SparseScore != 9.9 {
		t.Fatalf("high must be rank0 score 9.9, got %+v", h)
	} else if !h.SparseStrong {
		t.Fatalf("high rank0 9.9 must be SparseStrong, got %+v", h)
	}
	if h := byID["low"]; h.SparseRank == nil || *h.SparseRank != 1 || h.SparseScore != 5.0 {
		t.Fatalf("low must be rank1 score 5.0, got %+v", h)
	} else if h.SparseStrong {
		t.Fatalf("low rank1 5.0 below 0.6×9.9 must not be SparseStrong, got %+v", h)
	}
}

// TestSearchHybrid_SparseOnlyHitDenseScoreZero 仅稀疏分支召回的点位拿不到
// dense 分数 → DenseScore=0；但 sparse 回查仍给出名次/分数——强词法命中
// （SparseStrong：rank ≤ 3 且 score ≥ 0.6×池榜首 且 ≥1.0）经 api 层并集保留，
// 不再被 dense 阈值误杀（3128 回归：bm25 rank0 曾因 dense<0.5 被滤）。
func TestSearchHybrid_SparseOnlyHitDenseScoreZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/query"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["prefetch"]; ok {
				_, _ = w.Write([]byte(`{"result":{"points":[{"id":"sparse-only","score":0.4,"payload":{"text":"x"}}]},"status":"ok"}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":{"points":[{"id":"sparse-only","score":9.9}]},"status":"ok"}`))
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			_, _ = w.Write([]byte(`{"result":[]}`))
		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	hits, err := c.SearchHybrid(context.Background(), "kb1", []float32{0.1},
		SparseVector{Indices: []uint32{5}, Values: []float32{1}},
		SearchRequest{Limit: 3})
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(hits) != 1 || hits[0].DenseScore != 0 || hits[0].Score != 0.4 {
		t.Fatalf("sparse-only hit must carry Score=rrf and DenseScore=0, got %+v", hits)
	}
	if hits[0].SparseRank == nil || *hits[0].SparseRank != 0 || hits[0].SparseScore != 9.9 {
		t.Fatalf("sparse-only hit must carry lexical evidence rank0/9.9, got %+v", hits[0])
	}
	if !hits[0].SparseStrong {
		t.Fatalf("sparse-only rank0 9.9 (池内唯一 = 榜首) must be SparseStrong, got %+v", hits[0])
	}
}

// TestSearchHybrid_SparseLookupFailurePropagates sparse 分数回查失败必须上抛
// （不静默产出缺词法证据的半结果——缺 rank/score 会让强词法命中被误杀，且
// 排查无门；降级只经 AGMEM_HYBRID=off 显式控制）。
func TestSearchHybrid_SparseLookupFailurePropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/query"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["prefetch"]; ok {
				_, _ = w.Write([]byte(`{"result":{"points":[{"id":"a","score":0.5}]},"status":"ok"}`))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"err":"sparse lookup boom"}`))
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			_, _ = w.Write([]byte(`{"result":[{"id":"a","score":0.9,"payload":{}}]}`))
		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	_, err := c.SearchHybrid(context.Background(), "kb1", []float32{0.1},
		SparseVector{Indices: []uint32{1}, Values: []float32{1}}, SearchRequest{Limit: 3})
	if err == nil || !strings.Contains(err.Error(), "sparse score lookup") {
		t.Fatalf("sparse lookup failure must propagate naming the stage, got %v", err)
	}
}

// TestSearchHybrid_EmptySparseFallsBackToLegacy 查询无词元（空稀疏向量）→
// 退化 legacy Search，DenseScore=Score 保持阈值语义统一，不发 /points/query，
// 不带任何稀疏证据字段（SparseRank=nil，wire 与旧响应一致）。
func TestSearchHybrid_EmptySparseFallsBackToLegacy(t *testing.T) {
	var queryCalled, searchCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/query"):
			queryCalled = true
			_, _ = w.Write([]byte(`{"result":[]}`))
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			searchCalled = true
			_, _ = w.Write([]byte(`{"result":[{"id":"a","score":0.8,"payload":{}}]}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	hits, err := c.SearchHybrid(context.Background(), "kb1", []float32{0.1}, SparseVector{}, SearchRequest{Limit: 3})
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if queryCalled {
		t.Fatal("empty sparse must not hit /points/query")
	}
	if !searchCalled || len(hits) != 1 || hits[0].DenseScore != hits[0].Score {
		t.Fatalf("legacy fallback must set DenseScore=Score, got %+v (search=%v)", hits, searchCalled)
	}
	if hits[0].SparseRank != nil || hits[0].SparseScore != 0 {
		t.Fatalf("legacy fallback must carry no sparse evidence, got %+v", hits[0])
	}
	if hits[0].SparseStrong {
		t.Fatalf("legacy fallback must not be SparseStrong, got %+v", hits[0])
	}
}

// TestSearchHybrid_QueryFailurePropagates /points/query 失败必须上抛（不静默
// 降级稠密结果——降级由 AGMEM_HYBRID=off 显式控制，错误静默=排查黑洞）。
func TestSearchHybrid_QueryFailurePropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/points/query") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"err":"Vector bm25 not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	if _, err := c.SearchHybrid(context.Background(), "kb1", []float32{0.1},
		SparseVector{Indices: []uint32{1}, Values: []float32{1}}, SearchRequest{}); err == nil {
		t.Fatal("query failure must propagate as error")
	}
}

// TestSearchHybridMulti_FanoutMergeAndAnnotation 多 collection 混合 fan-out：
// 按 rrf 合并、来源标注、失败快速传播；rank/score 为源 collection 内名次。
func TestSearchHybridMulti_FanoutMergeAndAnnotation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		col := strings.TrimPrefix(r.URL.Path, "/collections/")
		col = strings.TrimSuffix(col, "/points/query")
		col = strings.TrimSuffix(col, "/points/search")
		switch {
		case col == "kb-bad":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"err":"boom"}`))
		case strings.HasSuffix(r.URL.Path, "/points/query") && col == "kb-a":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["prefetch"]; ok {
				_, _ = w.Write([]byte(`{"result":{"points":[{"id":"a1","score":0.8,"payload":{"text":"A"}}]},"status":"ok"}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":{"points":[{"id":"a1","score":9.9}]},"status":"ok"}`))
		case strings.HasSuffix(r.URL.Path, "/points/query") && col == "kb-c":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["prefetch"]; ok {
				_, _ = w.Write([]byte(`{"result":{"points":[{"id":"c1","score":0.9,"payload":{"text":"C"}}]},"status":"ok"}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":{"points":[{"id":"c1","score":2.0}]},"status":"ok"}`))
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			_, _ = w.Write([]byte(`{"result":[{"id":"a1","score":0.85,"payload":{"text":"A"}}]}`))
		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)

	t.Run("失败传播并指明collection", func(t *testing.T) {
		_, err := c.SearchHybridMulti(context.Background(),
			[]string{"kb-a", "kb-bad"}, []float32{0.1},
			SparseVector{Indices: []uint32{1}, Values: []float32{1}},
			SearchRequest{Limit: 2}, 2)
		if err == nil || !strings.Contains(err.Error(), "kb-bad") {
			t.Fatalf("fan-out failure must name collection, got %v", err)
		}
	})

	t.Run("合并rrf排序并标注来源", func(t *testing.T) {
		hits, err := c.SearchHybridMulti(context.Background(),
			[]string{"kb-a", "kb-c"}, []float32{0.1},
			SparseVector{Indices: []uint32{1}, Values: []float32{1}},
			SearchRequest{Limit: 2}, 2)
		if err != nil {
			t.Fatalf("SearchHybridMulti: %v", err)
		}
		if len(hits) != 2 {
			t.Fatalf("want 2 hits, got %+v", hits)
		}
		// kb-c 0.9 > kb-a 0.8，rrf 降序
		if hits[0].ID != "c1" || hits[0].Payload.KB != "kb-c" {
			t.Fatalf("rank0 = %+v, want c1@kb-c", hits[0])
		}
		if hits[1].ID != "a1" || hits[1].Payload.KB != "kb-a" || hits[1].DenseScore != 0.85 {
			t.Fatalf("rank1 = %+v, want a1@kb-a with dense 0.85", hits[1])
		}
		// rank/score/SparseStrong 均为源 collection 内各自计算：a1@kb-a rank0 9.9
		// （榜首，强词法）；c1@kb-c rank0 2.0 是它自己池内的榜首 → 相对门槛下
		// 也判 strong（2.0 ≥ 0.6×2.0 且 ≥1.0）——绝对门槛 8.0 会漏掉这类
		// 小词元量查询（2026-09-09 实测 sparse 分只有 1.0–7.5）。
		if hits[1].SparseRank == nil || *hits[1].SparseRank != 0 || hits[1].SparseScore != 9.9 {
			t.Fatalf("a1 sparse evidence = %v/%v, want rank0/9.9", hits[1].SparseRank, hits[1].SparseScore)
		}
		if !hits[1].SparseStrong {
			t.Fatalf("a1 rank0 9.9 must be SparseStrong, got %+v", hits[1])
		}
		if hits[0].SparseRank == nil || *hits[0].SparseRank != 0 || hits[0].SparseScore != 2.0 {
			t.Fatalf("c1 sparse evidence = %v/%v, want rank0/2.0", hits[0].SparseRank, hits[0].SparseScore)
		}
		if !hits[0].SparseStrong {
			t.Fatalf("c1 rank0 2.0 (per-collection top) must be SparseStrong, got %+v", hits[0])
		}
	})
}

// TestUpdateSparseVectors_BatchingAndShape 稀疏部分更新：按 100 分批、
// PUT .../points/vectors?wait=true、只携带 bm25 命名向量。
func TestUpdateSparseVectors_BatchingAndShape(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/points/vectors") {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		paths = append(paths, r.URL.RawQuery)
		bodies = append(bodies, body)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"result":{"status":"completed"},"status":"ok"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)

	updates := make([]SparseUpdate, 250)
	for i := range updates {
		updates[i] = SparseUpdate{
			ID:  fmt.Sprintf("pt-%d", i),
			Vec: SparseVector{Indices: []uint32{uint32(i)}, Values: []float32{1}},
		}
	}
	if err := c.UpdateSparseVectors(context.Background(), "kb1", updates); err != nil {
		t.Fatalf("UpdateSparseVectors: %v", err)
	}
	if len(bodies) != 3 {
		t.Fatalf("250 updates must batch 100/100/50, got %d requests", len(bodies))
	}
	for i, b := range bodies {
		if paths[i] != "wait=true" {
			t.Errorf("batch %d query = %q, want wait=true", i, paths[i])
		}
		pts := b["points"].([]any)
		want := 100
		if i == 2 {
			want = 50
		}
		if len(pts) != want {
			t.Errorf("batch %d size = %d, want %d", i, len(pts), want)
		}
		p0 := pts[0].(map[string]any)
		vec := p0["vector"].(map[string]any)
		sv, ok := vec[SparseVectorName]
		if !ok {
			t.Fatalf("batch %d point vector must carry only bm25: %v", i, p0)
		}
		if len(sv.(map[string]any)["indices"].([]any)) != 1 {
			t.Fatalf("sparse indices missing")
		}
	}
}

// sparseScore 一条 sparse 回查池记录（分数由客户端排序定名次）。
type sparseScore struct {
	id    string
	score float64
}

// searchHybridSparseScores 用真实 SearchHybrid 打 fake qdrant：融合响应返回
// 池内全部 id，sparse 回查返回给定池（乱序也无妨，客户端按分数排序定名次），
// dense 回查返回空（DenseScore=0，隔离 SparseStrong 判定）。返回 id→hit。
func searchHybridSparseScores(t *testing.T, pool []sparseScore) map[string]SearchHit {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/query"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["prefetch"]; ok {
				pts := make([]string, 0, len(pool))
				for i := range pool {
					pts = append(pts, fmt.Sprintf(`{"id":%q,"score":%d,"payload":{}}`, pool[i].id, len(pool)-i))
				}
				_, _ = w.Write([]byte(`{"result":{"points":[` + strings.Join(pts, ",") + `]},"status":"ok"}`))
				return
			}
			pts := make([]string, 0, len(pool))
			for _, s := range pool {
				pts = append(pts, fmt.Sprintf(`{"id":%q,"score":%v}`, s.id, s.score))
			}
			_, _ = w.Write([]byte(`{"result":{"points":[` + strings.Join(pts, ",") + `]},"status":"ok"}`))
		case strings.HasSuffix(r.URL.Path, "/points/search"):
			_, _ = w.Write([]byte(`{"result":[]}`))
		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	hits, err := c.SearchHybrid(context.Background(), "kb1", []float32{0.1},
		SparseVector{Indices: []uint32{1}, Values: []float32{1}}, SearchRequest{Limit: len(pool) + 1})
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	byID := make(map[string]SearchHit, len(hits))
	for _, h := range hits {
		byID[h.ID] = h
	}
	return byID
}

// TestSearchHybrid_SparseStrong 词法强命中判定（qdrant 层，2026-09-09 检索质量批）：
// strong = rank ≤ 3 且 score ≥ 0.6×该池榜首 且 score ≥ 1.0，三条件含等号。
// 表驱动覆盖榜首、相对门槛边界、绝对下限边界、rank 上限。
func TestSearchHybrid_SparseStrong(t *testing.T) {
	cases := []struct {
		name string
		pool []sparseScore
		want map[string]bool
	}{
		{
			name: "榜首强命中_次席低于0.6倍top不豁免",
			pool: []sparseScore{{"a", 9.9}, {"b", 5.5}},
			want: map[string]bool{"a": true, "b": false},
		},
		{
			name: "0.6倍top边界含等号",
			pool: []sparseScore{{"a", 10}, {"b", 6.0}, {"c", 5.99}},
			want: map[string]bool{"a": true, "b": true, "c": false},
		},
		{
			name: "floor1.0边界含等号",
			pool: []sparseScore{{"a", 1.5}, {"b", 1.0}, {"c", 0.99}},
			want: map[string]bool{"a": true, "b": true, "c": false},
		},
		{
			name: "rank超3不豁免",
			pool: []sparseScore{{"r0", 10}, {"r1", 10}, {"r2", 10}, {"r3", 10}, {"r4", 10}},
			want: map[string]bool{"r0": true, "r1": true, "r2": true, "r3": true, "r4": false},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			byID := searchHybridSparseScores(t, c.pool)
			for id, want := range c.want {
				h, ok := byID[id]
				if !ok {
					t.Fatalf("hit %s missing, got %v", id, byID)
				}
				if h.SparseStrong != want {
					t.Errorf("%s SparseStrong = %v, want %v (rank=%v score=%v)",
						id, h.SparseStrong, want, h.SparseRank, h.SparseScore)
				}
			}
		})
	}

	// 池为空（有词元但 qdrant 无任何词法匹配）→ 融合点无 rank 证据，恒不豁免。
	t.Run("sparse池为空不豁免", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/points/query"):
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if _, ok := body["prefetch"]; ok {
					_, _ = w.Write([]byte(`{"result":{"points":[{"id":"dense-only","score":0.5,"payload":{}}]},"status":"ok"}`))
					return
				}
				_, _ = w.Write([]byte(`{"result":{"points":[]},"status":"ok"}`))
			case strings.HasSuffix(r.URL.Path, "/points/search"):
				_, _ = w.Write([]byte(`{"result":[{"id":"dense-only","score":0.5,"payload":{}}]}`))
			default:
				t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(500)
			}
		}))
		defer srv.Close()
		c := New(srv.URL, "", time.Second)
		hits, err := c.SearchHybrid(context.Background(), "kb1", []float32{0.1},
			SparseVector{Indices: []uint32{1}, Values: []float32{1}}, SearchRequest{Limit: 3})
		if err != nil {
			t.Fatalf("SearchHybrid: %v", err)
		}
		if len(hits) != 1 || hits[0].SparseRank != nil || hits[0].SparseStrong {
			t.Fatalf("dense-only hit with empty sparse pool must not be SparseStrong, got %+v", hits)
		}
	})
}

// TestSearchHit_SparseStrongNotInJSON 内部字段不得进 API 响应 wire（json:"-"）：
// 既有 sparse_rank/sparse_score/dense_score 字段语义不变。
func TestSearchHit_SparseStrongNotInJSON(t *testing.T) {
	rk := 0
	b, err := json.Marshal(SearchHit{
		ID: "x", Score: 0.5, DenseScore: 0.3, SparseRank: &rk, SparseScore: 2.0, SparseStrong: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "sparse_strong") || strings.Contains(s, "SparseStrong") {
		t.Fatalf("SparseStrong must not leak to JSON: %s", s)
	}
	for _, k := range []string{`"dense_score"`, `"sparse_rank"`, `"sparse_score"`} {
		if !strings.Contains(s, k) {
			t.Fatalf("existing wire field %s missing: %s", k, s)
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
