// Package qdrant：Qdrant REST 客户端（upsert / search / collection 创建 /
// 混合检索 dense+sparse RRF / 稀疏向量部分更新）。
// 约定：collection 每 kb 一个；payload 带 dir / point_type(original|clean) / task_id。
package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

// SparseVectorName 混合检索命名稀疏向量（collection 内与默认稠密向量 "" 并存）。
// 名字进 collection 配置与点位 wire 格式，改动即破坏存量数据，视为常量契约。
const SparseVectorName = "bm25"

// Client 是 Qdrant REST 客户端。
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// New 创建客户端。
func New(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

// PointPayload 统一 payload 契约。
type PointPayload struct {
	Dir       string `json:"dir"`               // 1 级目录名（clean 条目为 `<dir>-clean`）
	PointType string `json:"point_type"`        // original | clean
	TaskID    int64  `json:"task_id"`           // 来源任务
	KB        string `json:"kb,omitempty"`      // 便于 --global 查询标注来源
	ItemID    int64  `json:"item_id,omitempty"` // task_items.id，回查元数据用
	Text      string `json:"text,omitempty"`    // 原始/清洗文本（查询直接返回内容）
	// 分条批（2026-09-07）：clean 点分条后每条目的序号与总数；original 点不带。
	// wire 契约由 MarshalJSON 保证（clean 恒输出含 0 的 entry_index，original 恒
	// 不输出）；tag 的 omitempty 只影响无 MarshalJSON 的默认编码路径。
	EntryIndex int `json:"entry_index,omitempty"`
	EntryCount int `json:"entry_count,omitempty"`
	// SourceDoc 入库文档名（documents.source_name；G01 结构化核实字段）。clean 点
	// 携带，与条目末尾「来源：」行同值；original 点不带（omitempty）。
	SourceDoc string `json:"source_doc,omitempty"`
}

// MarshalJSON payload wire 契约（UC-003 A2 / G13，2026-09-09）：
//
//   - clean 点恒带 entry_index（含 0）与 entry_count——int + omitempty 会把 0
//     省略，导致 idx=0 的条目在 wire 上缺字段（G13 实测 518 个 idx=0 点无
//     entry_index）；
//   - original 点恒不带这两个字段（test/agmem/uc003-clean-quality C1 断言
//     original 无 entry 字段）。
//
// 因此不能简单地给 EntryIndex 去掉 omitempty（那会让 original 点也带
// entry_index:0，直接违反 C1）。这里用「clean 时输出指针（&0 照常序列化）」
// 的等价实现：外层匿名结构体字段在更浅深度遮蔽内嵌 alias 的同名 int 字段，
// 指针为 nil 时整字段省略。
func (p PointPayload) MarshalJSON() ([]byte, error) {
	type alias PointPayload // 避免递归调用本方法
	aux := struct {
		alias
		EntryIndex *int `json:"entry_index,omitempty"`
		EntryCount *int `json:"entry_count,omitempty"`
	}{alias: alias(p)}
	// PointType=clean 是唯一权威判据；entry 字段已被写入（非零）时同样输出，
	// 避免历史上未标 point_type 的调用点丢失 entry 信息。
	if p.PointType == "clean" || p.EntryCount > 0 || p.EntryIndex != 0 {
		idx, cnt := p.EntryIndex, p.EntryCount
		aux.EntryIndex, aux.EntryCount = &idx, &cnt
	}
	return json.Marshal(aux)
}

// Point 单条向量点。
type Point struct {
	ID      string       `json:"id"`
	Vector  []float32    `json:"vector"`
	Payload PointPayload `json:"payload"`
	// Sparse 命名稀疏向量（混合检索批）：非空时整点走 qdrant 命名向量 wire
	// 格式（见 MarshalJSON）。键 = 稀疏向量名（bm25）。
	Sparse map[string]SparseVector `json:"-"`
}

// SparseVector qdrant 稀疏向量（v1.19.0 OpenAPI SparseVector：
// indices 唯一（bm25 包保证升序），values 与 indices 等长对齐）。
type SparseVector struct {
	Indices []uint32  `json:"indices"`
	Values  []float32 `json:"values"`
}

// pointWire Point 的 wire 形态（MarshalJSON 内部用，避免递归）。
type pointWire struct {
	ID      string       `json:"id"`
	Vector  any          `json:"vector"`
	Payload PointPayload `json:"payload"`
}

// MarshalJSON 兼容两种 wire 形态（qdrant v1.19.0）：
//   - 仅稠密（Sparse 为空）：vector 数组——既有格式，存量兼容不变；
//   - 带稀疏：命名向量 map。默认（未命名）稠密向量的内部名是空串 ""
//     （qdrant v1.19.0 源码 lib/segment/src/data_types/vectors.rs:259
//     `pub const DEFAULT_VECTOR_NAME: &VectorName = "";`），故混合点位为
//     {"vector": {"": [dense...], "bm25": {"indices": [...], "values": [...]}}}。
func (p Point) MarshalJSON() ([]byte, error) {
	w := pointWire{ID: p.ID, Payload: p.Payload}
	if len(p.Sparse) == 0 {
		w.Vector = p.Vector
		return json.Marshal(w)
	}
	vec := make(map[string]any, len(p.Sparse)+1)
	if p.Vector != nil {
		vec[""] = p.Vector
	}
	for name, sv := range p.Sparse {
		vec[name] = sv
	}
	w.Vector = vec
	return json.Marshal(w)
}

// doRaw 执行请求返回状态码与响应体；非 2xx 不自动转 error（404 探测等场景用）。
func (c *Client) doRaw(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("api-key", c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("qdrant: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	status, b, err := c.doRaw(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return b, fmt.Errorf("qdrant: %s %s: status %d: %s", method, path, status, truncate(b))
	}
	return b, nil
}

// collectionInfo GET /collections/{name} → (status, body)。
func (c *Client) collectionInfo(ctx context.Context, name string) (int, []byte, error) {
	return c.doRaw(ctx, http.MethodGet, "/collections/"+name, nil)
}

// EnsureCollection 幂等创建 collection（dim + cosine + bm25 稀疏向量，出生即
// 混合检索就绪）。
//
// 请求形状（qdrant v1.19.0 OpenAPI CreateCollection 确认，勿凭记忆改）：
//
//	PUT /collections/{name}
//	{"vectors": {"size": dim, "distance": "Cosine"},
//	 "sparse_vectors": {"bm25": {"index": {"datatype": "float32"}}}}
//
// SparseIndexParams.datatype=float32 = 全精度权重；on_disk 在 spec 中已标注
// deprecated 且默认 false（索引常驻 RAM），故不显式传废弃字段。
// 存量 collection（GET 200）：不整包重 PUT（PUT 全量配置对已存在 collection 的
// 增量语义无承诺），改走 Create named vector 幂等补齐 bm25（ensureSparse）。
func (c *Client) EnsureCollection(ctx context.Context, name string, dim int) error {
	status, body, err := c.collectionInfo(ctx, name)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		_, err := c.do(ctx, http.MethodPut, "/collections/"+name, map[string]any{
			"vectors": map[string]any{"size": dim, "distance": "Cosine"},
			"sparse_vectors": map[string]any{
				SparseVectorName: map[string]any{
					"index": map[string]any{"datatype": "float32"},
				},
			},
		})
		return err
	}
	if status >= 300 {
		return fmt.Errorf("qdrant: GET /collections/%s: status %d: %s", name, status, truncate(body))
	}
	return c.ensureSparse(ctx, name, body)
}

// EnsureSparseVector 幂等给已存在 collection 增补 bm25 命名稀疏向量（存量
// collection 混合检索就绪的唯一通道；agmem-migrate SPARSE=1 回填前置步骤）。
// collection 不存在 → error（不静默建库——dim 缺失，建出错误配置更糟）。
func (c *Client) EnsureSparseVector(ctx context.Context, collection string) error {
	status, body, err := c.collectionInfo(ctx, collection)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("qdrant: collection %s 不存在，无法增补稀疏向量", collection)
	}
	if status >= 300 {
		return fmt.Errorf("qdrant: GET /collections/%s: status %d: %s", collection, status, truncate(body))
	}
	return c.ensureSparse(ctx, collection, body)
}

// ensureSparse 依据 collection 配置 body 幂等补 bm25 稀疏向量。
//
// 请求形状（qdrant v1.19.0 OpenAPI「Create named vector」，v1.18.0 起可用；
// VectorNameConfig = {"sparse": SparseVectorConfig{modifier?, datatype?}}，
// 索引参数自动派生、可后续单独配置）：
//
//	PUT /collections/{name}/vectors/bm25
//	{"sparse": {"datatype": "float32"}}
func (c *Client) ensureSparse(ctx context.Context, collection string, infoBody []byte) error {
	var cfg struct {
		Result struct {
			Config struct {
				Params struct {
					SparseVectors map[string]json.RawMessage `json:"sparse_vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	_ = json.Unmarshal(infoBody, &cfg) // 解析失败 = 视作缺失，走补建（幂等安全侧）
	if _, ok := cfg.Result.Config.Params.SparseVectors[SparseVectorName]; ok {
		return nil // 已配置：幂等跳过
	}
	_, err := c.do(ctx, http.MethodPut,
		"/collections/"+collection+"/vectors/"+SparseVectorName,
		map[string]any{"sparse": map[string]any{"datatype": "float32"}})
	return err
}

// Upsert 写入向量点（wait=true 保证可查语义）。
func (c *Client) Upsert(ctx context.Context, collection string, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	_, err := c.do(ctx, http.MethodPut, "/collections/"+collection+"/points?wait=true", map[string]any{
		"points": points,
	})
	return err
}

// SearchHit 检索结果。
type SearchHit struct {
	ID      string       `json:"id"`
	Score   float64      `json:"score"`
	Payload PointPayload `json:"payload"`
	// DenseScore 混合检索路径附带的 dense 余弦相似度：rrf 融合分（Score）是
	// 排名分（Σ 1/(k+rank)，k 默认 2），量纲与 cosine 无关，阈值过滤必须用
	// DenseScore（见 SearchHybrid 注释与 api.filterHybridHits）。
	// 纯稠密 Search 不填（omitempty，旧响应 wire 不变）。
	DenseScore float64 `json:"dense_score,omitempty"`
	// SparseRank 混合检索路径 sparse（bm25）分支的名次（0 = 榜首；nil = 无
	// 稀疏分支数据：纯稠密路径 / hybrid off / 空查询词元 / 未进 sparse 回查池）。
	// 指针类型不可省：0 是有效名次（词法最强命中恰是 rank 0），必须与「无数据」
	// 区分——omitempty 只省 nil 指针，&0 照常输出，wire 两态正确。
	// SparseScore sparse 分支原始分数（bm25 词元权重和，量纲与 cosine / rrf
	// 均无关）。
	SparseRank  *int    `json:"sparse_rank,omitempty"`
	SparseScore float64 `json:"sparse_score,omitempty"`
	// SparseStrong 词法强命中判定（内部字段，json:"-" 不导出到 API 响应）：
	// 由 SearchHybrid 按该 collection 的 sparse 回查池完整排序计算（见
	// sparseStrong* 常量）；api.filterHybridHits 用它做阈值并集豁免。
	// 纯稠密路径 / 空稀疏退化 / 未进 sparse 池恒为 false。
	SparseStrong bool `json:"-"`
}

// 词法强命中门槛（2026-09-09 检索质量批实测，集中命名勿散落）：
//
//   - sparseStrongRank = 3：sparse 回查池前 4 名（rank 0..3）；
//   - sparseStrongTopRatio = 0.6：分数 ≥ 该池榜首的 60%——相对门槛，精确 token
//     查询的 sparse 分随查询词元量浮动（实测 1.0–7.5），绝对门槛 8.0 不可达；
//   - sparseStrongMinScore = 1.0：绝对下限，滤掉池内极弱尾部（实测有效词法命中
//     最低 ~1.0）。
//
// 三条件同时满足才是强词法命中。非配置项：改门槛=改产品行为，须走设计拍板。
const (
	sparseStrongRank     = 3
	sparseStrongTopRatio = 0.6
	sparseStrongMinScore = 1.0
)

// scoredPoint 是 /points/search（result 为裸数组）与 /points/query
// prefetch+fusion（result 为 {"points":[...]} 包裹对象，qdrant v1.19 实测
// 2026-09-08）共用的单点形状。
type scoredPoint struct {
	ID      string       `json:"id"`
	Score   float64      `json:"score"`
	Payload PointPayload `json:"payload"`
}

type searchResp struct {
	Result []scoredPoint `json:"result"`
}

// queryResp 是 /points/query 的响应形状：result 是 {"points":[...]} 包裹对象。
// Qdrant v1.19.0 实测（2026-09-08）prefetch+fusion、无 prefetch 的稠密
// 数组 query、无 prefetch 的 sparse 向量 query 三种形态全部为包裹对象——裸
// 数组只有 legacy /points/search（用 searchResp 解析）。用错形状会把每个
// scored point 解进 Result 数组失败。
type queryResp struct {
	Result struct {
		Points []scoredPoint `json:"points"`
	} `json:"result"`
}

// SearchRequest 查询参数（同时充当 scroll 枚举的过滤条件载体：见 ItemID）。
type SearchRequest struct {
	Vector    []float32
	Limit     int
	Dir       string // 空表示不限目录（--global 语义）
	PointType string // original | clean | 空
	// ItemID 按 payload.item_id 精确过滤（>0 生效）。检索路径不设置（搜索只按
	// dir/point_type 收窄）；ScrollPointIDs 用它枚举某条目的全部点位 id。
	ItemID int64
}

// buildFilter 构造 dir/point_type/item_id 过滤器（Search、SearchHybrid 与
// ScrollPointIDs 共用单一事实源）。
func buildFilter(req SearchRequest) map[string]any {
	filter := map[string]any{"must": []map[string]any{}}
	if req.Dir != "" {
		filter["must"] = append(filter["must"].([]map[string]any),
			map[string]any{"key": "dir", "match": map[string]any{"value": req.Dir}})
	}
	if req.PointType != "" {
		filter["must"] = append(filter["must"].([]map[string]any),
			map[string]any{"key": "point_type", "match": map[string]any{"value": req.PointType}})
	}
	if req.ItemID > 0 {
		filter["must"] = append(filter["must"].([]map[string]any),
			map[string]any{"key": "item_id", "match": map[string]any{"value": req.ItemID}})
	}
	return filter
}

// Search 在 collection 内检索（纯稠密；hybrid=off 降级路径与 dense 分数回查共用）。
func (c *Client) Search(ctx context.Context, collection string, req SearchRequest) ([]SearchHit, error) {
	if req.Limit <= 0 {
		req.Limit = 5
	}
	b, err := c.do(ctx, http.MethodPost, "/collections/"+collection+"/points/search", map[string]any{
		"vector": req.Vector, "limit": req.Limit, "filter": buildFilter(req), "with_payload": true,
	})
	if err != nil {
		return nil, err
	}
	var r searchResp
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("qdrant: decode search result: %w", err)
	}
	hits := make([]SearchHit, 0, len(r.Result))
	for _, h := range r.Result {
		hits = append(hits, SearchHit{ID: h.ID, Score: h.Score, Payload: h.Payload})
	}
	return hits, nil
}

// mergeLimit fan-out 单 collection 检索条数（与 Search 缺省一致）。
func (r *SearchRequest) mergeLimit() int {
	if r.Limit <= 0 {
		return 5
	}
	return r.Limit
}

// hybridPrefetchLimit 混合检索各分支 prefetch 条数下限：给 rrf 重排留足
// 候选池（分支 top-k 之外的内容进不了融合），请求 limit 更大时按需放大。
const hybridPrefetchLimit = 20

// SearchHybrid 混合检索（单 collection）：dense + bm25 稀疏 prefetch，rrf 融合。
//
// 请求形状（qdrant v1.19.0 OpenAPI QueryRequest/Prefetch/FusionQuery + 官方
// hybrid-queries 文档确认，勿凭记忆改）：
//
//			POST /collections/{n}/points/query
//			{"prefetch": [
//			  {"query": [dense...], "limit": 20, "filter": F},
//			  {"query": {"indices": [...], "values": [...]}, "using": "bm25", "limit": 20, "filter": F}
//			 ],
//			 "query": {"fusion": "rrf"},
//			 "limit": k, "filter": F, "with_payload": true}
//
//	  - dense prefetch 不带 using（OpenAPI：缺省即默认向量，等价内部名 ""）；
//	  - rrf 分 = Σ 1/(k+rank)，k 默认 2，rank 从 0 起（官方公式，两分支双榜首
//	    上限 1.0）；分数是排名分，与 cosine 不可比——阈值语义见 DenseScore；
//	  - 融合响应只回融合分（ScoredPoint.score 单值，无各分支分数）→ 各分支分数
//	    分别回查补齐：dense 相似度用同 filter、limit=hybridPrefetchLimit 的
//	    legacy /points/search（map[id]score）；sparse 分数与名次用同 filter、
//	    同 limit 的 sparse-only /points/query（见 sparseLookup）。分支进 prefetch
//	    的点必然落在对应回查池内（同 filter 同 limit 同查询语义）。
//
// 阈值设计拍板（2026-09-08，2026-09-09 词法门槛下移本层）：api 层
// filterHybridHits 保留条件 = DenseScore（dense 余弦）≥ 阈值 **或** SparseStrong
// （本层按 sparseStrong* 常量判定）。dense 达标 = 语义相关；SparseStrong =
// 精确词法相关（端口/命令号/专有名词类查询，如「npm go pip 依赖下载走哪个端口」
// 的 3128 条目 bm25 rank0、dense < 0.5——纯 dense 阈值曾误杀）。两者并集即
// 「相关性强的知识」；排序恒用 Score（rrf）。
// 已知边界：sparse 弱命中（相对门槛不达）且 dense 不可见的点位仍被过滤——两路
// 都看不见的点位宁可漏过不误放（UC-1 宁缺毋滥同语义）。
// fan-out（SearchHybridMulti）下 rank/score/SparseStrong 均为源 collection 内
// 各自计算（合并不改写）。
// sparseVec 为空（查询无任何词元）→ 退化为 legacy Search（空稀疏 prefetch
// 无意义），DenseScore=Score 保持阈值语义统一。
func (c *Client) SearchHybrid(ctx context.Context, collection string, denseVec []float32, sparseVec SparseVector, req SearchRequest) ([]SearchHit, error) {
	if len(sparseVec.Indices) == 0 && len(sparseVec.Values) == 0 {
		hits, err := c.Search(ctx, collection, req)
		if err != nil {
			return nil, err
		}
		for i := range hits {
			hits[i].DenseScore = hits[i].Score
		}
		return hits, nil
	}
	limit := req.mergeLimit()
	pl := limit
	if pl < hybridPrefetchLimit {
		pl = hybridPrefetchLimit
	}
	filter := buildFilter(req)
	b, err := c.do(ctx, http.MethodPost, "/collections/"+collection+"/points/query", map[string]any{
		"prefetch": []map[string]any{
			{"query": denseVec, "limit": pl, "filter": filter},
			{"query": sparseVec, "using": SparseVectorName, "limit": pl, "filter": filter},
		},
		"query":        map[string]any{"fusion": "rrf"},
		"limit":        limit,
		"filter":       filter,
		"with_payload": true,
	})
	if err != nil {
		return nil, err
	}
	var r queryResp
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("qdrant: decode hybrid query result: %w", err)
	}
	// dense 相似度回查（同 filter 同 prefetch 条数；见函数注释）
	dense, err := c.Search(ctx, collection, SearchRequest{
		Vector: denseVec, Limit: pl, Dir: req.Dir, PointType: req.PointType,
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant: hybrid dense score lookup: %w", err)
	}
	denseScore := make(map[string]float64, len(dense))
	for _, h := range dense {
		denseScore[h.ID] = h.Score
	}
	// sparse 分数/名次回查（与 dense 回查对称；失败同样上抛不静默降级）
	sparse, err := c.sparseLookup(ctx, collection, sparseVec, req, pl)
	if err != nil {
		return nil, fmt.Errorf("qdrant: hybrid sparse score lookup: %w", err)
	}
	sparseRank := make(map[string]int, len(sparse))
	sparseScore := make(map[string]float64, len(sparse))
	for i, h := range sparse {
		sparseRank[h.ID] = i // 列表已按分数降序，下标即名次（rank 0 = 榜首）
		sparseScore[h.ID] = h.Score
	}
	// 词法强命中相对门槛基准 = 该 collection sparse 回查池榜首分（池空为 0，
	// 此时无人有 rank，SparseStrong 恒 false）。
	sparseTop := 0.0
	for _, h := range sparse {
		if h.Score > sparseTop {
			sparseTop = h.Score
		}
	}
	hits := make([]SearchHit, 0, len(r.Result.Points))
	for _, h := range r.Result.Points {
		hit := SearchHit{ID: h.ID, Score: h.Score, Payload: h.Payload, DenseScore: denseScore[h.ID]}
		if rank, ok := sparseRank[h.ID]; ok {
			rk := rank // 拷贝后取址：rank 0 是有效名次，不能与零值混同
			hit.SparseRank = &rk
			hit.SparseScore = sparseScore[h.ID]
			hit.SparseStrong = rank <= sparseStrongRank &&
				hit.SparseScore >= sparseStrongTopRatio*sparseTop &&
				hit.SparseScore >= sparseStrongMinScore
		}
		hits = append(hits, hit)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits, nil
}

// sparseLookup 混合检索 sparse 分支分数回查（与 dense 回查对称）：融合响应只回
// 融合分，sparse 分支分数用同 filter、limit 同 prefetch 条数的 sparse-only
// /points/query 补齐（无 prefetch，"using" 钉 bm25），按分数降序定名次后返回
// （rank 0 = 榜首；仅含真正匹配到词元的点位——未匹配点不在结果里即「无词法证据」）。
// 响应形状（Qdrant v1.19.0 实测 2026-09-08）：无 prefetch 的 sparse 向量
// query，result 仍是 {"points":[...]} 包裹对象（query 端点恒包裹，裸数组只在
// legacy /points/search——见 queryResp 注释），按 queryResp 解析。
// with_payload=false：只取 id/score，省 payload 带宽。
func (c *Client) sparseLookup(ctx context.Context, collection string, sparseVec SparseVector, req SearchRequest, limit int) ([]SearchHit, error) {
	b, err := c.do(ctx, http.MethodPost, "/collections/"+collection+"/points/query", map[string]any{
		"query": sparseVec, "using": SparseVectorName, "limit": limit,
		"filter": buildFilter(req), "with_payload": false,
	})
	if err != nil {
		return nil, err
	}
	var r queryResp
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("qdrant: decode sparse lookup result: %w", err)
	}
	hits := make([]SearchHit, 0, len(r.Result.Points))
	for _, h := range r.Result.Points {
		hits = append(hits, SearchHit{ID: h.ID, Score: h.Score})
	}
	// qdrant 语义上按分数降序返回；显式排序钉死「下标即名次」契约，不依赖
	// 服务端排序承诺（同分保持服务端顺序，SliceStable 确定可比）。
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits, nil
}

// fanOutSearch 跨 collection fan-out 公共骨架（SearchMulti / SearchHybridMulti
// 共用单一事实源）：并发度 concurrency>0；任一 collection 失败整体失败——
// 不静默降级半结果（避免「看似全局、实则缺库」的误导）；hit 来源标注
// payload.kb（老点位响应层补标注，不回填 qdrant）；合并按 score 降序取全局
// top-k。collections 为空返回空结果非错误。
func (c *Client) fanOutSearch(ctx context.Context, collections []string, concurrency, limit int,
	search func(ctx context.Context, collection string) ([]SearchHit, error)) ([]SearchHit, error) {
	if len(collections) == 0 {
		return []SearchHit{}, nil
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	type res struct {
		collection string
		hits       []SearchHit
		err        error
	}
	ch := make(chan res, len(collections))
	for _, col := range collections {
		go func(col string) {
			sem <- struct{}{}
			defer func() { <-sem }()
			hits, err := search(ctx, col)
			ch <- res{collection: col, hits: hits, err: err}
		}(col)
	}
	merged := make([]SearchHit, 0, len(collections)*limit)
	for range collections {
		r := <-ch
		if r.err != nil {
			return nil, fmt.Errorf("qdrant: fan-out %s: %w", r.collection, r.err)
		}
		for _, h := range r.hits {
			if h.Payload.KB == "" {
				h.Payload.KB = r.collection // 老点位响应层标注（不回填 qdrant）
			}
			merged = append(merged, h)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// SearchMulti 跨 collection fan-out（--global 无 --kb 语义）：
// 每个 collection 独立 top-k 检索（并发度 concurrency>0），合并按 score 降序
// 取全局 top-k；hit 带 payload.kb 来源标注。语义详见 fanOutSearch。
func (c *Client) SearchMulti(ctx context.Context, collections []string, req SearchRequest, concurrency int) ([]SearchHit, error) {
	limit := req.mergeLimit()
	return c.fanOutSearch(ctx, collections, concurrency, limit, func(ctx context.Context, col string) ([]SearchHit, error) {
		return c.Search(ctx, col, req)
	})
}

// SearchHybridMulti 跨 collection 混合检索 fan-out：每 collection 独立
// SearchHybrid（rrf 融合 + dense 分数回查）后按 rrf 分合并取全局 top-k，
// hit 带 payload.kb 来源标注。语义与失败策略同 fanOutSearch。
func (c *Client) SearchHybridMulti(ctx context.Context, collections []string, denseVec []float32, sparseVec SparseVector, req SearchRequest, concurrency int) ([]SearchHit, error) {
	limit := req.mergeLimit()
	return c.fanOutSearch(ctx, collections, concurrency, limit, func(ctx context.Context, col string) ([]SearchHit, error) {
		return c.SearchHybrid(ctx, col, denseVec, sparseVec, req)
	})
}

// SparseUpdate 单点稀疏向量部分更新（不动稠密向量与 payload）。
type SparseUpdate struct {
	ID  string
	Vec SparseVector
}

// sparseUpdateBatch 部分更新单批点数（wait=true 逐批确认，公网回填吞吐与
// 服务端压力的折中）。
const sparseUpdateBatch = 100

// UpdateSparseVectors 按命名稀疏向量（bm25）部分更新既有点。
//
// 请求形状（qdrant v1.19.0 OpenAPI「Update vectors」，逐字确认：
// "Update specified named vectors on points, keep unspecified vectors intact"）：
//
//	PUT /collections/{n}/points/vectors?wait=true
//	{"points": [{"id": "...", "vector": {"bm25": {"indices": [...], "values": [...]}}}]}
//
// 内部按 sparseUpdateBatch 分批；存量 bm25 稀疏向量回填（agmem-migrate
// SPARSE=1）用。
func (c *Client) UpdateSparseVectors(ctx context.Context, collection string, updates []SparseUpdate) error {
	for start := 0; start < len(updates); start += sparseUpdateBatch {
		end := start + sparseUpdateBatch
		if end > len(updates) {
			end = len(updates)
		}
		batch := updates[start:end]
		points := make([]map[string]any, len(batch))
		for i, u := range batch {
			points[i] = map[string]any{
				"id":     u.ID,
				"vector": map[string]any{SparseVectorName: u.Vec},
			}
		}
		if _, err := c.do(ctx, http.MethodPut,
			"/collections/"+collection+"/points/vectors?wait=true",
			map[string]any{"points": points}); err != nil {
			return fmt.Errorf("qdrant: update sparse vectors batch [%d,%d): %w", start, end, err)
		}
	}
	return nil
}

func truncate(b []byte) string {
	if len(b) > 256 {
		return string(b[:256]) + "..."
	}
	return string(b)
}

// scrollPageLimit scroll 单页条数（只取 id，页大减少往返；与 cmd/agmem-migrate 同量级）。
const scrollPageLimit = 256

// ScrollPointIDs 按 filter 分页 scroll 枚举点 id（翻页直到 next_page_offset 为空）。
//
// 请求形状（qdrant v1.19.0 OpenAPI ScrollPoints 确认）：
//
//	POST /collections/{n}/points/scroll
//	{"limit": 256, "with_payload": false, "with_vector": false,
//	 "filter": {"must": [...]}, "offset": <上一页 next_page_offset>}
//
// offset 原样回传上一页响应的 next_page_offset（点 id 可为 UUID 字符串或 u64
// 数字，故保留 json.RawMessage 不解析）；缺失/null 即结束。响应点 id 统一经
// decodePointID 归一为字符串。
//
// 用途：生命周期删除必须先拿到显式点 id 清单（DeleteByID 语义；绝不用 item_id
// filter 直删——同 item 并发写入会被误删，2026-09-07 事故教训）。
func (c *Client) ScrollPointIDs(ctx context.Context, collection string, req SearchRequest) ([]string, error) {
	var ids []string
	var offset json.RawMessage
	for {
		body := map[string]any{
			"limit":        scrollPageLimit,
			"with_payload": false,
			"with_vector":  false,
			"filter":       buildFilter(req),
		}
		if len(offset) > 0 && string(offset) != "null" {
			body["offset"] = offset
		}
		raw, err := c.do(ctx, http.MethodPost, "/collections/"+collection+"/points/scroll", body)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Result struct {
				Points []struct {
					ID json.RawMessage `json:"id"`
				} `json:"points"`
				NextPageOffset json.RawMessage `json:"next_page_offset"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("qdrant: decode scroll: %w", err)
		}
		for _, pt := range resp.Result.Points {
			if id := decodePointID(pt.ID); id != "" {
				ids = append(ids, id)
			}
		}
		offset = resp.Result.NextPageOffset
		if len(offset) == 0 || string(offset) == "null" {
			return ids, nil
		}
	}
}

// decodePointID qdrant 点 id 解码：UUID 字符串或数字统一归一为字符串。
func decodePointID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}

// DeleteByID 按显式点 id 删除（迁移/清理工具用；绝不用 filter 语义删——
// item_id 过滤会误删刚 upsert 的同 item 新点，2026-09-07 事故教训）。
func (c *Client) DeleteByID(ctx context.Context, collection string, ids []string) error {
	_, err := c.do(ctx, http.MethodPost, "/collections/"+collection+"/points/delete",
		map[string]any{"points": ids})
	return err
}
