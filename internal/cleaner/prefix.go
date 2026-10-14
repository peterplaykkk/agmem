// prefix.go：清洗不变前缀构建器（S1，设计与依据见 docs/prefix-caching.md）。
//
// 目标：把「上一轮已经提取出来的知识条目」构造成逐字节稳定的前缀，放在每次清洗
// 请求最前面，让 DeepSeek 的 KV 前缀缓存命中（缓存按「前缀单元 + 完整匹配」，见
// api-docs.deepseek.com/zh-cn/guides/kv_cache）。任何每请求都变的内容（时间戳、
// 随机排序、来源文档名）一旦混进 L0–L2 就会让命中率归零，故本文件只做确定性拼接。
//
// 分层（稳定在前、易变在后）：
//
//	L0 全局契约        CleanRulesL0（规则文本，一字不改）
//	L1 全局知识前缀    全库条目中**排除目标目录及其整条父链**后的部分，按产出时间
//	                   正序（最早的在前）。append-only 载体（见下）。
//	L2 目录上下文      目标目录 + 父链祖先（沿父链上溯到根，**无层数上限**，能装多少
//	                   由 L2 额度决定），按「近→远」装填，接在 L1 后
//	L3 本次目标         由 glm.go PrefixUserBlock 拼（覆盖声明 + 目标文档 + 正文）
//
// L3 不在本文件，避免把每请求唯一变的内容混进被 hash 的 Text。
//
// # 为什么 L1 必须排除目标目录（设计 §3.1 必要条件 1）
//
// 本轮新产出的条目属于目标目录。若它们进 L1，就会插在 L1 的中段（按 ItemID 排序，
// 新条目 id 最大但排在 L2 之前的位置），旧 L1 不再是新 L1 的前缀 → 整段缓存失效。
// 把「目标目录及其父目录」整体挪到 L2 后：
//
//	L1(轮 N+1) == L1(轮 N) + [轮 N 新产出的、属于其他目录的条目]   # 只在末尾追加
//	L2(轮)  == 轮开始时「目标目录 + 父目录」条目的冻结快照           # 轮内逐字节不变
//
// L1 与 L2 因此**互斥**：同一目录的条目只出现在 L2，不出现在 L1，不重复。
//
// 冻结（必要条件 2）不由本文件做：Build 只按传入的 entries 拼一次；轮次内复用同一
// Prefix 由调用方（worker）负责——同一目标目录在本轮只调用一次 Build。
//
// # 已知偏差：覆盖标识用「文档名」而非 URL
//
// 设计文档写的是「覆盖 URL 清单」，但库里只有 documents.source_name（文档标题），
// 没有 wiki URL。构造可靠链接需要 node_token，而 wikisync 的 state 里只存 obj_token
// （见 internal/wikisync/state.go），凭 obj_token 拼不出稳定可点开的 wiki 链接。
// 故本批用**文档名**作为覆盖标识（稳定、可读、可去重）；KnowledgeEntry.DocName、
// Prefix.CoveredDocs 与 L3 文案都按文档名表达。不改 wikisync 去凑 URL。若将来
// documents 表补 node_token，再把标识切回 URL。
package cleaner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// DefaultPrefixBudgetTokens 单请求不变前缀（L0+L1+L2）的 token 预算上限。
//
// 2026-09-10 收紧：原 800k 改为 200k。理由——不变前缀是逐字节稳定的上下文，会随库
// 增长长期占满请求预算；200k 为 L3（目标正文 + 覆盖声明）与输出留出足够空间，且按
// 实测 wiki 树规模足以覆盖整条父链 + 绝大部分全库条目。
const DefaultPrefixBudgetTokens = 200000

// 预算按层切分（不得互相挤占；见 docs/prefix-caching.md §4）：
//
//	L1（跨目录恒定，缓存连续性载体）拿 3/4，L2（目录上下文，随目标目录变）拿 1/4。
//
// 为什么必须切分、而不是「先到先得」：L1 是 L0+L1 段跨目录复用的唯一载体，其体积必须
// **与目标目录无关**。若让无上限上溯的 L2 与 L1 竞争同一个池子，深目录会吃掉更多预算、
// 把 L1 挤小 → 每次换目录 L1 都变 → 跨目录缓存全 miss（CLAUDE.md §3.1 命根子）。
// 切分后 L1 额度恒定、L2 额度独立，两者之和 == 预算（扣除恒定 L0 后）。
//
// 比例取 3:1：L1 承载全库、是长期增长项与命中主体，给大头；L2 只覆盖「目标目录 + 父链」，
// 单轮体积通常远小于全库，1/4（默认 200k → 约 50k token）足够覆盖实测最深的 5 层目录链。
const (
	prefixL1BudgetNum = 3
	prefixL1BudgetDen = 4
)

// prefixQuotas 把预算派生成 L1/L2 两个额度：先扣除恒定的 L0 契约，再把余量按
// prefixL1BudgetNum/prefixL1BudgetDen（3:1）切给 L1/L2；余数归 L2，保证
// l1Quota+l2Quota == budget-l0，因而 l0 + l1Used + l2Used ≤ budget 恒成立
// （Build 的硬断言 EstTokens ≤ budget）。
//
// 边界：L0 恒在（不可裁），故 budget ≤ L0 的退化配置下 rest=0、两个额度皆 0，此时
// EstTokens == L0 > budget——硬断言只在 budget > 估算后的 L0 时成立。默认预算 200k
// 远大于 L0（数百 token），config 对非正值回落默认，正常路径不受影响。
func prefixQuotas(budget int) (l1Quota, l2Quota int) {
	if budget <= 0 {
		budget = DefaultPrefixBudgetTokens
	}
	rest := budget - estimateTokens(CleanRulesL0)
	if rest < 0 {
		rest = 0
	}
	l1Quota = rest * prefixL1BudgetNum / prefixL1BudgetDen
	l2Quota = rest - l1Quota
	return l1Quota, l2Quota
}

// token 估算系数：0.6 token/字符（rune）。
//
// 依据与保守方向：DeepSeek 中文约 1 token/字，实测约 0.5 token/字符；这里取 0.6（偏高）意味着**高估** token 数 → 实际装入的条目更
// 少 → prompt 不会超预算。保守方向是「宁可少装也不超限」。
const (
	prefixTokensPerRuneNum = 3
	prefixTokensPerRuneDen = 5
)

// estimateTokens 按 rune 数估算 token（向上取整，见上方保守方向）。
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := runeLen(s)
	return (n*prefixTokensPerRuneNum + prefixTokensPerRuneDen - 1) / prefixTokensPerRuneDen
}

// PrefixSource 提供构建前缀所需的数据（由 worker 注入，便于测试）。kbID 决定取哪个
// 知识库的条目——前缀必须与目标条目同库，否则会拿别的库的知识当背景。
type PrefixSource interface {
	// KnowledgeEntries 返回某 kb 截至当前已清洗入库的条目（按产出时间正序）。
	KnowledgeEntries(ctx context.Context, kbID int64) ([]KnowledgeEntry, error)
}

// KnowledgeEntry 一条已入库的知识条目（构建不变前缀的最小输入）。
type KnowledgeEntry struct {
	ItemID int64
	// DocName 来源文档名（documents.source_name）。作为覆盖声明的稳定标识；见文件
	// 头「已知偏差」——本批没有 URL，用文档名而非 URL。
	DocName string
	Dir     string // 归属目录（-clean 归一后的名字）
	Text    string // 条目正文（≤600 字）
}

// PrefixBuilder 构建某一轮 + 某目录的不变前缀。
type PrefixBuilder struct {
	// Budget tokens 上限；≤0 用 DefaultPrefixBudgetTokens。
	Budget int
	// ParentDirFunc 由目录名推导其父目录（入参/返回都是 -clean 归一后的名字）。
	// 为 nil 时**降级为「仅同目录」**：L2 只取 Dir == targetDir 的条目，不含任何
	// 父目录条目——数据不足以判定父子时的安全缺省，宁可少给上下文也不给错上下文。
	// 非 nil 时 cleaner **沿父链一直上溯到根，不设层数上限**（父缺失/成环即停，见
	// contextDirChain）；能装多少层由 L2 额度决定。
	ParentDirFunc func(dir string) string
}

// Prefix 某一轮 + 某目录的不变前缀。
type Prefix struct {
	Text string // L0+L1+L2 全文（逐字节稳定）
	// L1Text L1 段（L0+L1，不含 L2）。暴露出来专供 append-only 断言：向目标目录内
	// 新增条目时 L1Text 必须逐字节不变（新条目只进 L2）。
	L1Text    string
	Hash      string // sha256(Text) 前 16 位——同轮同目录必须相等
	EstTokens int    // Text 的 token 估算（保守偏高，见 estimateTokens）
	// L1Tokens L1 段（L0 契约 + L1 条目）的 token 估算；L2Tokens L2 段的 token 估算。
	// 二者相加恒等于 EstTokens，便于 /metrics 观测预算切分的实际占用。
	L1Tokens int
	L2Tokens int
	// DirDepth L2 实际装入的目录层数：目标目录记 1 层，每上溯一层 +1（父链成环/父缺失
	// 即停，不受人为层数上限约束；空目录不计）。0 表示 L2 未装入任何目录。
	DirDepth    int
	CoveredDocs []string // 去重后的来源文档名列表（排序，确定性）
	// DocCounts 每个覆盖文档命中的条目数（L3 覆盖声明用「<文档>（<n> 条）」）。
	DocCounts map[string]int
}

// WithinBudget 硬断言：前缀总量不超预算（budget<=0 用 DefaultPrefixBudgetTokens）。
// Build 按 L1/L2 切分额度构造，恒有 EstTokens ≤ budget（见 prefixQuotas）；本方法供
// 调用方与测试独立复核，返回 false 即视为构建 bug。
func (p Prefix) WithinBudget(budget int) bool {
	if budget <= 0 {
		budget = DefaultPrefixBudgetTokens
	}
	return p.EstTokens <= budget
}

// Build 产出某一轮 + 某目录的不变前缀。entries 必须按产出时间正序（最早的在前）。
//
// 分层（见文件头「为什么 L1 必须排除目标目录」）：
//   - L1 = dir 不在「目标目录 + 整条父链」集合内的条目，时间正序，受 L1 额度约束；
//   - L2 = dir 在集合内的条目，按「近→远」目录顺序装填（每目录内时间正序），
//     受独立 L2 额度约束，接在 L1 之后。
//
// 超额度只从尾部砍：L1 砍最新产出的（保留稳定老前缀），L2 砍父链远端（保留近目录）。
// L1/L2 额度独立、不互相挤占——这是 L1 跨目录恒定的前提（见 prefixQuotas）。
func (b *PrefixBuilder) Build(entries []KnowledgeEntry, targetDir string) Prefix {
	budget := DefaultPrefixBudgetTokens
	var parent func(string) string
	if b != nil {
		if b.Budget > 0 {
			budget = b.Budget
		}
		parent = b.ParentDirFunc
	}
	l1Quota, l2Quota := prefixQuotas(budget)

	var sb strings.Builder
	sb.WriteString(CleanRulesL0)
	l0Tokens := estimateTokens(CleanRulesL0)

	counts := map[string]int{}
	seen := map[string]bool{}
	chain := contextDirChain(targetDir, parent)
	dirSet := make(map[string]bool, len(chain))
	for _, d := range chain {
		dirSet[d] = true
	}

	// L1：**排除**目标目录及其整条父链的全部条目，时间正序；超 L1 额度即停（只砍
	// 尾部，不跳中间）。这些条目不随目标目录变化，是跨轮 append-only 的载体。
	l1Used := 0
	for _, e := range entries {
		if dirSet[e.Dir] {
			continue
		}
		txt := entryText(e)
		if txt == "" {
			continue
		}
		seg := "\n\n" + txt
		cost := estimateTokens(seg)
		if l1Used+cost > l1Quota {
			break
		}
		sb.WriteString(seg)
		l1Used += cost
		addDocCoverage(counts, seen, e)
	}

	// L1Text 在写 L2 之前截取：L1 段 = L0+L1。
	l1Text := sb.String()

	// L2：目标目录 + 父链祖先，按「近→远」装填（近目录优先占额度），每目录内时间正序。
	// 与 L1 互斥，不重复。L2 额度独立：装满即停，等价于从父链远端砍。
	byDir := make(map[string][]KnowledgeEntry, len(chain))
	for _, e := range entries {
		if dirSet[e.Dir] {
			byDir[e.Dir] = append(byDir[e.Dir], e)
		}
	}
	l2Used := 0
	dirDepth := 0
	room := true
	for _, d := range chain {
		if !room {
			break
		}
		packedThisDir := false
		for _, e := range byDir[d] {
			txt := entryText(e)
			if txt == "" {
				continue
			}
			seg := "\n\n" + txt
			cost := estimateTokens(seg)
			if l2Used+cost > l2Quota {
				room = false
				break
			}
			sb.WriteString(seg)
			l2Used += cost
			packedThisDir = true
			addDocCoverage(counts, seen, e)
		}
		if packedThisDir {
			dirDepth++
		}
	}

	text := sb.String()
	docs := make([]string, 0, len(counts))
	for d := range counts {
		docs = append(docs, d)
	}
	sort.Strings(docs)
	sum := sha256.Sum256([]byte(text))
	return Prefix{
		Text:        text,
		L1Text:      l1Text,
		Hash:        hex.EncodeToString(sum[:])[:16],
		EstTokens:   l0Tokens + l1Used + l2Used,
		L1Tokens:    l0Tokens + l1Used,
		L2Tokens:    l2Used,
		DirDepth:    dirDepth,
		CoveredDocs: docs,
		DocCounts:   counts,
	}
}

// CoverageLines 覆盖声明行：每个文档名一行「<文档>（<n> 条）」，按文档名排序
// （确定性，排序不影响被 hash 的 Text——覆盖声明在 L3，不进前缀）。
func (p Prefix) CoverageLines() []string {
	lines := make([]string, 0, len(p.DocCounts))
	for _, d := range p.CoveredDocs {
		lines = append(lines, fmt.Sprintf("%s（%d 条）", d, p.DocCounts[d]))
	}
	return lines
}

// entryText 单条被装入前缀的正文（trim 后；空正文跳过不计入，见 addDocCoverage 语义）。
func entryText(e KnowledgeEntry) string {
	return strings.TrimSpace(e.Text)
}

// addDocCoverage 记录一个被装入前缀的条目的来源文档：按文档名去重后累计**唯一条目数**
// （同一条目在 L1 与 L2 互斥、不会重复出现；去重键仍保留 itemID+正文以防未来分层
// 变化引入重复时把覆盖声明的条目数放大）。空文档名（无法在覆盖声明里表达）与空
// 正文不计。
func addDocCoverage(counts map[string]int, seen map[string]bool, e KnowledgeEntry) {
	d := strings.TrimSpace(e.DocName)
	if d == "" {
		return
	}
	key := fmt.Sprintf("%s\x00%d\x00%s", d, e.ItemID, entryText(e))
	if seen[key] {
		return
	}
	seen[key] = true
	counts[d]++
}

// contextDirChain 计算 L2 的目录链：targetDir 起，沿 ParentDirFunc **上溯到根，不设
// 层数上限**（能装多少由 L2 额度决定，不由层数决定）。返回「近→远」有序列表。
//
// 安全停止（避免死循环/panic）：
//   - parent 为 nil（数据不足以判定父子）→ 只含 targetDir，降级为「仅同目录」；
//   - 父目录为空串 → 已到根，停；
//   - 父目录已在链上（含 parent 返回自身）→ 成环，停（链上出现重复目录必须检测）。
func contextDirChain(targetDir string, parent func(string) string) []string {
	chain := []string{targetDir}
	if parent == nil {
		return chain
	}
	seen := map[string]bool{targetDir: true}
	cur := targetDir
	for {
		p := strings.TrimSpace(parent(cur))
		if p == "" || seen[p] {
			break
		}
		seen[p] = true
		chain = append(chain, p)
		cur = p
	}
	return chain
}
