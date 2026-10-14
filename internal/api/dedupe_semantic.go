// dedupe_semantic.go：检索结果**语义**近重复折叠（2026-09-10）。
//
// 与 dedupe.go 的字面判据互补：字面判据抓「近乎逐字相同」，抓不住「同义改写」。
// 实测（ask「gitlab 推送代码」返回的 5 条）两两余弦：
//
//	[0]完整步骤580字 ↔ [1]命令步骤278字   0.862  ← 同一内容不同详略，属重复
//	[3]提交流程353字 ↔ [4]提交流程439字   0.889  ← 同义改写，属重复
//	[0]完整步骤580字 ↔ [2]Token步骤377字  0.905  ← 互补（全流程 vs Token 细节），不该折
//
// 可见**纯余弦阈值无法区分「互补细节」与「同义复述」**——0.905 那对反而更高。
// 因此阈值做成可配置（AGMEM_DEDUP_COSINE）。**默认 0.8**（2026-09-10 用户拍板）：
// 语义相似度 ≥0.8 视为「同一份知识」只留一条。0.6 实测过：把「同主题但各有细节」
// 的条目也折了，eval 12/12 → 10/12（git-token-02 丢 Edit profile/Generate token、
// 「代理端口」用例 丢 3128/3130）；0.8 落在「同义改写折、互补细节留」的分界上。
// 置 0 关闭本层。
//
// 代价：每次 ask 多一次 embedding 调用（≤10 条短文本，一次批量）。失败时**跳过
// 本层并记日志**，绝不让去重把查询本身弄挂。
package api

import (
	"context"
	"log"
	"math"

	"sre/agmem/internal/qdrant"
)

// DefaultDedupCosine 语义去重默认阈值：语义相似度 ≥ 该值视为同一份知识。
// 0 = 关闭本层。取值依据（0.6 实测掉 2 例 eval，0.8 为分界）见文件头。
const DefaultDedupCosine = 0.8

// collapseSemanticDuplicates 用向量余弦折叠语义近重复：按入参顺序贪心，
// 每条与已保留的逐条比较，≥ threshold 即折叠（保留排序更前的那条）。
// 调用方保证 hits 已按期望优先级排序。
//
// vecs 必须与 hits 等长且同序；长度不符视为调用方错误（返回原样，不折）。
func collapseSemanticDuplicates(hits []qdrant.SearchHit, vecs [][]float32, threshold float64) (kept []qdrant.SearchHit, dropped int) {
	if threshold <= 0 || len(vecs) != len(hits) || len(hits) < 2 {
		return hits, 0
	}
	norm := make([][]float32, len(vecs))
	for i, v := range vecs {
		norm[i] = l2norm(v)
	}
	kept = make([]qdrant.SearchHit, 0, len(hits))
	keptIdx := make([]int, 0, len(hits))
	for i, h := range hits {
		dup, dupIdx := false, -1
		for ki, j := range keptIdx {
			if cosine(norm[i], norm[j]) >= threshold {
				dup, dupIdx = true, ki
				break
			}
		}
		if dup {
			// 同 dedupe.go：重复组里留**信息更全**的那条，不是排序最高的那条。
			// 比较基准必须是**当前已保留的那条**（kept[dupIdx]）——它可能已被
			// 更长的候选替换过；若拿 hits[keptIdx[dupIdx]]（原始下标）比，替换
			// 后再来更长的也不会生效（实测：580 字段落被 353 字段住）。
			if runeLen(h.Payload.Text) > runeLen(kept[dupIdx].Payload.Text) {
				kept[dupIdx] = h
				norm[keptIdx[dupIdx]] = norm[i]
			}
			dropped++
			continue
		}
		kept = append(kept, h)
		keptIdx = append(keptIdx, i)
	}
	return kept, dropped
}

// semanticCollapse 取向量并折叠；任何失败都降级为「不折」并记日志。
func (s *Server) semanticCollapse(ctx context.Context, hits []qdrant.SearchHit) ([]qdrant.SearchHit, int) {
	th := s.Cfg.DedupCosine
	if th <= 0 || len(hits) < 2 {
		return hits, 0
	}
	texts := make([]string, len(hits))
	for i, h := range hits {
		texts[i] = h.Payload.Text
	}
	vecs, err := s.Embedder.Embed(ctx, texts)
	if err != nil {
		log.Printf("api: 语义去重跳过（embedding 失败，不影响本次查询）: %v", err)
		return hits, 0
	}
	return collapseSemanticDuplicates(hits, vecs, th)
}

func l2norm(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	n := float32(math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / n
	}
	return out
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}
