// dedupe.go：检索结果近重复折叠（2026-09-10）。
//
// 背景：同一事实常常被多篇文档分别讲过，各自清洗后成为多条语义重复的条目
// （实测：「GitLab 提交代码流程 / worktree」主题有 7 条来自不同文档的条目）。
// 前缀缓存让**未来的**提取不再重复，但存量重复会在每次 ask 里一起返回——
// 既浪费 token，也让 agent 看到自相矛盾的多个版本。
//
// 做法：返回前对命中做**两两近重复折叠**——3-gram Jaccard ≥ 阈值的归为一组，
// 每组只保留排序最高的那条（Score 已按 rrf 降序）。折叠只发生在响应层，
// 不动库内数据、不影响阈值过滤语义。
//
// 阈值取 0.75 是刻意的**保守**选择：只折叠「几乎逐字相同」的条目，不碰
// 「讲同一主题但内容互补」的条目——后者恰恰是 agent 需要的上下文。
// 折叠数经响应字段 deduped 透出（--json/--debug 可见），默认文本不显示。
package api

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"sre/agmem/internal/qdrant"
)

// nearDupJaccard 近重复判定阈值（3-gram Jaccard）。调高=更保守（少折叠）。
const nearDupJaccard = 0.75

// nearDupContainment 包含度阈值：短条目的 3-gram 有该比例以上出现在已保留的长条目里
// 即折叠。动机（实测）：「推送代码到 GitLab 的命令步骤」（278 字）与「…完整步骤」
// （580 字）讲的是同一件事的不同详细度，Jaccard 只有 ~0.3 抓不到，但前者近乎被后者
// 包含——agent 看到的就是重复。包含度对「长短详略」这一最常见的重复形态更敏感。
const nearDupContainment = 0.7

// collapseNearDuplicates 折叠近重复命中：保留每组排序最高的一条。
// 输入须已按期望优先级排序（调用方保证 Score 降序）；返回保持相对顺序。
// dropped 为被折叠掉的条数（调用方可在诊断字段里透出）。
func collapseNearDuplicates(hits []qdrant.SearchHit) (kept []qdrant.SearchHit, dropped int) {
	kept = make([]qdrant.SearchHit, 0, len(hits))
	grams := make([]map[string]struct{}, 0, len(hits))
	for _, h := range hits {
		g := textTrigrams(hitText(h))
		dupOf := -1
		for i, prev := range grams {
			// 两个方向都要看：containment 不对称——「短版在前、长版在后」时
			// containment(短,长)≥阈值 但反向不达，只看一个方向会漏折（实测形态）。
			if jaccard(g, prev) >= nearDupJaccard ||
				containment(g, prev) >= nearDupContainment ||
				containment(prev, g) >= nearDupContainment {
				dupOf = i
				break
			}
		}
		if dupOf >= 0 {
			// 折叠时**保留信息更全的那条**（2026-09-10）：同一主题的不同详略，
			// 排序高的往往是短版——实测 0.6 阈值下「命令步骤(278字)」压住
			// 「完整步骤(580字，含 Token 创建+钥匙串+write_repository)」，把用户
			// 真正要的细节折没了。既然判定为重复，就该留详版：用更长的正文替换
			// 已保留项（位置不变，排序语义不受影响）。
			if runeLen(hitText(h)) > runeLen(hitText(kept[dupOf])) {
				kept[dupOf] = h
				grams[dupOf] = g
			}
			dropped++
			continue
		}
		kept = append(kept, h)
		grams = append(grams, g)
	}
	return kept, dropped
}

// hitText 取命中正文（payload.text）。Payload 是值类型（零值 Text 为空串），
// 因此无需判空：空正文的 gram 集为空，jaccard 恒为 0，不参与折叠。
func hitText(h qdrant.SearchHit) string { return h.Payload.Text }

// textTrigrams 归一化后取 3-gram 集合：去空白、转小写。
// 归一化保证「只差空格/换行/大小写」的两条被判为同一份内容。
func textTrigrams(s string) map[string]struct{} {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	runes := []rune(b.String())
	if len(runes) < 3 {
		return nil
	}
	out := make(map[string]struct{}, len(runes)-2)
	for i := 0; i+3 <= len(runes); i++ {
		out[string(runes[i:i+3])] = struct{}{}
	}
	return out
}

// containment 返回 a 被 b 覆盖的比例（|a∩b| / |a|）；a 为空返回 0。
// 只在 a 更短时才有意义——a 比 b 长时比例可能很低，自然不会触发折叠。
func containment(a, b map[string]struct{}) float64 {
	if len(a) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	return float64(inter) / float64(len(a))
}

// runeLen 字数（与 cleaner 同口径：中文按 Unicode 码点计）。
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// jaccard 两个集合的 Jaccard 相似度；任一为空返回 0（不误判为重复）。
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
