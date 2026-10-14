// entrysplit.go：清洗输出分条（2026-09-07 分条优化批）。
//
// 背景：原管线 1 个 task_item = 1 个 clean 点（GLM 对 8000 字分块的完整输出整体
// 落一个点），单条命中 p50≈1960 字 / p90≈3759 字，对 agent prompt 过长。统一决策
// L=600：GLM prompt 约束 = 本文件分条硬上限 = CLI ask 展示截断默认值，三处同源
// （见 MaxEntryLen）。存量长点位由后续迁移脚本处理，本批只做代码。
package cleaner

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxEntryLen 单条记忆条目长度硬上限（单位：字 = rune）。
//
// 统一约束（2026-09-07 拍板 L=600），改动必须同步：
//  1. GLM 清洗 prompt 字面「600」（glm.go CleanPrompt，tripwire TestCleanPromptEntryConstraint）；
//  2. worker 分条硬上限（processEmbedded 以本常量为 SplitEntries 实参）；
//  3. CLI ask 展示截断默认值（cmd/agmem --max-len 默认引用本常量）。
const MaxEntryLen = 600

// MinEntryLen 单条记忆条目长度下限（单位：字 = rune），低于此值的微型条目在
// embed 前丢弃（混合检索批 2026-09-08）。
//
// 动机（线上实证）：2809 条 ≤600 字条目中存在大量 6-8 字微型条目，其 embedding
// 向量近噪声，纯稠密检索时把口语查询（如「怎么推代码」）的 top-5 全部占位，
// 挤出真正含步骤的条目。信息量不足以被检索消费的条目照常 embed+入库 = 检索
// 污染，宁可不落点。
//
// 与 isPlaceholderEntry 的关系：占位过滤管「字面占位符」（""、（空） 等，
// 精确语义不变）；本下限管占位之外的短而空的条目（标题残片、孤立短语）。
// 仅是 embed 前的落点过滤——不改 GLM 清洗 prompt（CleanPrompt 的「600」约束
// 不动），因此无需与 glm.go 同步；改动同步点：worker.filterMicroEntries 与
// 本常量、测试（entrysplit_test.go / worker_test.go）。
const MinEntryLen = 15

// SplitEntries 把清洗输出按 markdown 结构切分为若干独立记忆条目（每条 ≤maxLen 字）。
//
// 切分优先级：markdown 标题（#/##/###…）→ 空行 → 列表项（1. / 1、 / - / *）边界；
// 相邻短段贪心打包（以 "\n" 连接）到 ≤maxLen；单段超长时先按句子（。！？!?与换行）
// 切，仍超按 rune 硬切（硬上限兜底，尽量落句界）；收尾折叠纯元数据条目
// （foldMetaOnlyEntries，2026-09-09 线上缺陷修复）。
//
// v5 G14（2026-09-09）：同一段的续片以承接式主题句开头（「（续）<段主题>：」，
// 主题取段首标题/结论句，无则不加前缀）；条目末行不得悬空主题句（末行为标题
// 且其后仍有内容时，顺延为下一条目首行）。
//
// 契约：
//   - 内容无损：去除合成行（「（续）…：」「来源：…」）后，全部非空白字符按原序
//     分布于返回条目，即
//     stripSpace(stripSynthLines(join)) == stripSpace(text)
//     （空白/换行差异属「分隔符归一」；合成行是 G01/G14 的有意新增）；
//   - 每条 rune 长度 ≤maxLen（maxLen>0 时）；
//   - text 全空白 → nil；maxLen<=0 → 不切分（单条返回 trim 后全文）。
func SplitEntries(text string, maxLen int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if maxLen <= 0 {
		return []string{text}
	}
	var units []segUnit
	for i, seg := range splitSegments(text) {
		topic := segTopic(seg)
		for _, u := range splitUnits(seg, maxLen) {
			units = append(units, segUnit{text: u, segID: i, topic: topic})
		}
	}
	packed := fixDanglingTopicLines(packUnitsWithTopics(units, maxLen), maxLen)
	return foldMetaOnlyEntries(packed, maxLen)
}

// segUnit 装包单元：文本 + 所属段序号 + 段主题（续片承接句用）。
type segUnit struct {
	text  string
	segID int
	topic string
}

// segTopic 抽取段主题（用于续片承接句）：段首非空行若为 markdown 标题，取去掉
// 井号后的标题；若是 ≤60 字且以「：/」结尾的结论句，取该行；否则返回 ""（不
// 加承接句——无主题可承接时长段落加前缀只是噪声）。
func segTopic(seg string) string {
	first := ""
	for _, l := range strings.Split(seg, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			first = t
			break
		}
	}
	if first == "" {
		return ""
	}
	if isHeadingLine(first) {
		return truncateRunes(strings.TrimSpace(strings.TrimLeft(first, "#")), 40)
	}
	if isTopicLine(first) {
		return truncateRunes(first, 40)
	}
	return ""
}

// isTopicLine 判定主题行：markdown 标题，或 ≤60 字且以「：/」结尾的结论句。
// 用于悬空末行修复与续片承接句（避免把普通正文行当主题）。
func isTopicLine(line string) bool {
	if isHeadingLine(line) {
		return true
	}
	return runeLen(line) <= 60 && (strings.HasSuffix(line, "：") || strings.HasSuffix(line, ":"))
}

// packUnitsWithTopics 贪心装包（相邻单元以 "\n" 连接成 ≤maxLen 的条目），并对
// 同段续片补承接式主题句「（续）<段主题>：」。
func packUnitsWithTopics(units []segUnit, maxLen int) []string {
	var out []string
	var cur []segUnit
	curLen := 0
	curLastSeg := -1
	prevLastSeg := -1
	seen := map[int]bool{}

	flush := func() {
		if len(cur) == 0 {
			return
		}
		parts := make([]string, len(cur))
		for i, u := range cur {
			parts[i] = strings.TrimSpace(u.text)
		}
		out = append(out, strings.TrimSpace(strings.Join(parts, "\n")))
		for _, u := range cur {
			seen[u.segID] = true
		}
		prevLastSeg = curLastSeg
		cur = nil
		curLen = 0
		curLastSeg = -1
	}

	for _, u := range units {
		t := strings.TrimSpace(u.text)
		if t == "" {
			continue
		}
		ul := runeLen(t)
		if curLen > 0 && curLen+1+ul > maxLen {
			flush()
		}
		if curLen == 0 && u.segID == prevLastSeg && u.topic != "" && seen[u.segID] {
			prefix := "（续）" + u.topic + "："
			if runeLen(prefix)+1+ul <= maxLen {
				cur = append(cur, segUnit{text: prefix, segID: u.segID})
				curLen = runeLen(prefix)
			}
		}
		if curLen > 0 {
			curLen++
		}
		cur = append(cur, u)
		curLen += ul
		curLastSeg = u.segID
	}
	flush()
	return out
}

// fixDanglingTopicLines 修悬空末行：条目末行为主题行（标题/结论句）且其后仍有
// 内容时，把该主题行顺延为下一条目首行（保持全局字符顺序不变——只跨越条目边界）。
// 顺延后超 maxLen 时对下一条正文按预算重切，保证每条 ≤maxLen。整条仅主题行的
// 退化形态同样顺延；全文以主题行结尾时无处可顺延，独立成条。
func fixDanglingTopicLines(entries []string, maxLen int) []string {
	work := append([]string(nil), entries...)
	var out []string
	pending := ""
	for i := 0; i < len(work); i++ {
		e := work[i]
		if pending != "" {
			p := pending
			pending = ""
			budget := maxLen - runeLen(p) - 1
			if budget >= minTopicBudget && runeLen(e) > budget {
				pieces := packUnits(splitUnits(e, budget), budget)
				if len(pieces) > 1 {
					rest := append([]string(nil), pieces[1:]...)
					nw := make([]string, 0, len(work)+len(rest))
					nw = append(nw, work[:i+1]...)
					nw = append(nw, rest...)
					nw = append(nw, work[i+1:]...)
					work = nw
				}
				if len(pieces) > 0 {
					e = pieces[0]
				}
			}
			e = p + "\n" + e
		}
		lines := strings.Split(e, "\n")
		j := len(lines)
		for j > 0 && strings.TrimSpace(lines[j-1]) == "" {
			j--
		}
		if j >= 1 && isTopicLine(strings.TrimSpace(lines[j-1])) {
			pending = strings.TrimSpace(lines[j-1])
			e = strings.TrimSpace(strings.Join(lines[:j-1], "\n"))
		}
		if e != "" {
			out = append(out, e)
		}
	}
	if pending != "" {
		if len(out) == 0 {
			return []string{pending}
		}
		out = append(out, pending)
	}
	return out
}

// minTopicBudget 悬空主题行顺延后重切下一条正文的最小预算（rune）；低于此值时
// 主题行独立成条（避免把正文切得过碎）。
const minTopicBudget = 20

// isHeadingLine 判定 markdown 标题行（1-6 个 '#' 后跟空格）。
func isHeadingLine(line string) bool {
	n := strspnByte(line, '#')
	return n >= 1 && n <= 6 && len(line) > n && line[n] == ' '
}

// truncateRunes 截断到 n 个 rune（超出时末位替换为省略号）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n || n <= 0 {
		return s
	}
	return string(r[:n-1]) + "…"
}

// stripSynthLines 去除合成行（「（续）…：」承接句、「来源：…」来源行），供无损
// 契约断言使用（SplitEntries/来源注入新增的字符不计入原文保真）。
func stripSynthLines(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "（续）") && strings.HasSuffix(t, "：") {
			continue
		}
		if strings.HasPrefix(t, "来源：") {
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}

// sourceLine 构造规范来源行；来源名超长时截断展示（payload.source_doc 保留完整值）。
func sourceLine(source string, maxLen int) string {
	s := strings.TrimSpace(source)
	if s == "" {
		s = unnamedSource
	}
	if cap := maxLen/2 - 2; cap > 0 {
		s = truncateRunes(s, cap)
	}
	return "来源：" + s
}

// stripSourceLines 去掉条目内全部「来源：」行（模型可能写错/重复），正文按原序保留。
func stripSourceLines(entry string) string {
	var kept []string
	for _, l := range strings.Split(entry, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "来源：") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// peelTrailingApply 摘出条目末尾连续的「适用：」行（保留原样，注入来源行时置于
// 其前）。
func peelTrailingApply(entry string) (body, apply string) {
	lines := strings.Split(entry, "\n")
	i := len(lines)
	for i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "适用：") {
		i--
	}
	return strings.TrimSpace(strings.Join(lines[:i], "\n")), strings.Join(lines[i:], "\n")
}

// injectSourceLines 保证每条目带规范来源行（G01：来源行必出，值取入库文档名）。
//
//   - 去掉模型可能写错/重复的来源行，统一写入「来源：<sourceName>」；
//   - 置于末尾「适用：」行之前；
//   - 注入后超 maxLen 时按预算重切（与 foldMetaOnlyEntries 同法），保证每条 ≤maxLen；
//   - 纯元数据/空正文条目原样保留（不制造纯来源条目）。
func injectSourceLines(entries []string, source string, maxLen int) []string {
	line := sourceLine(source, maxLen)
	var out []string
	for _, e := range entries {
		body, apply := peelTrailingApply(stripSourceLines(e))
		if body == "" {
			out = append(out, e)
			continue
		}
		budget := maxLen - runeLen(line) - 1
		if apply != "" {
			budget -= runeLen(apply) + 1
		}
		if budget < metaBudgetFloor {
			out = append(out, body+"\n"+line+applySuffix(apply))
			continue
		}
		pieces := packUnits(splitUnits(body, budget), budget)
		if len(pieces) == 0 {
			out = append(out, e)
			continue
		}
		out = append(out, pieces[:len(pieces)-1]...)
		out = append(out, pieces[len(pieces)-1]+"\n"+line+applySuffix(apply))
	}
	return out
}

// applySuffix 拼接「适用：」行（非空时前置换行）。
func applySuffix(apply string) string {
	if apply == "" {
		return ""
	}
	return "\n" + apply
}

// metaLinePrefixes 元数据行前缀（trim 后行首匹配）。GLM 清洗 prompt v3 要求每条
// 末尾附「来源：<原文档标题>」行；prompt v3.1 追加「适用：<口语化关键词>」行。
// 只认行首——正文中间出现的「来源：」「适用：」不判元数据（避免误判）。
var metaLinePrefixes = []string{"来源：", "适用："}

// metaBudgetFloor 收尾折叠的最小预算（rune）：预算低于此值时放弃合并、让元数据
// 独立成条（退化条件，见 foldMetaOnlyEntries——强行并入会把正文切得过碎）。
const metaBudgetFloor = 100

// isMetaLine 判定元数据行（trim 后以 来源：/适用： 开头）。
func isMetaLine(line string) bool {
	for _, p := range metaLinePrefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// isMetaOnlyEntry 判定「纯元数据条目」：非空行全部是元数据行（空行不计）。
// 条目为空串时返回 false（SplitEntries 不产出空条目，此处仅防御）。
func isMetaOnlyEntry(entry string) bool {
	seen := false
	for _, line := range strings.Split(entry, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !isMetaLine(line) {
			return false
		}
		seen = true
	}
	return seen
}

// splitTrailingMeta 把条目末尾连续的元数据行剥出，返回（正文, 末尾元数据行）。
// 只剥末尾连续段：正文中间夹的元数据行不在此处理（它们不是缺陷形态）。
func splitTrailingMeta(entry string) (body string, meta []string) {
	lines := strings.Split(entry, "\n")
	i := len(lines)
	for i > 0 && isMetaLine(strings.TrimSpace(lines[i-1])) {
		i--
	}
	return strings.Join(lines[:i], "\n"), lines[i:]
}

// foldMetaOnlyEntries 收尾处理：消除「纯元数据条目」（2026-09-09 线上缺陷修复）。
//
// 缺陷（线上实证）：splitSegments 把「来源：…」「适用：…」行前的空行当段界，
// 元数据自成一段；packUnits 贪心装包放不下时单独成条——sre 库 2577 个 clean 点
// 中 59 个正文为空、只剩元数据行。这类条目会被词法命中、占用 top-k 槽位却返回
// 无内容（浪费 token、误导 agent）。
//
// 修法：左到右扫描，遇到纯元数据条目 meta 时并入前一条 prev——
//   - 无 prev（首个/唯一条目）：原样保留（纯元数据输入的退化形态，无正文可依附）；
//   - prev 本身也是纯元数据（前次折叠已退化）：meta 独立成条（不合并纯元数据）；
//   - 把 prev 末尾连续的元数据行剥出与 meta 拼接得 newMeta（若 prev 已被 packUnits
//     装进过元数据行，直接按 meta 预算重切 prev 会把元数据行留在末片、缺陷复现）；
//   - budget = maxLen - runeLen(newMeta) - 1 < metaBudgetFloor：meta 独立成条（退化）；
//   - 否则把 prev 正文按 budget 重切为 p1..pk（复用 splitUnits+packUnits），
//     输出 …p1..p_{k-1} + [p_k + "\n" + newMeta]：p_k+newMeta ≤ budget+1+runeLen(newMeta)=maxLen。
//
// 无损契约不变：折叠只重排非空白字符（正文重切 + 元数据行原样后置），
// stripSpace(join)==stripSpace(text) 仍成立；每条 ≤maxLen 仍成立。
func foldMetaOnlyEntries(entries []string, maxLen int) []string {
	var out []string
	for _, e := range entries {
		if len(out) == 0 || !isMetaOnlyEntry(e) {
			out = append(out, e)
			continue
		}
		prev := out[len(out)-1]
		body, trailing := splitTrailingMeta(prev)
		if body == "" { // prev 全为元数据：退化，保持 meta 独立成条
			out = append(out, e)
			continue
		}
		meta := e
		if len(trailing) > 0 {
			meta = strings.Join(trailing, "\n") + "\n" + e
		}
		budget := maxLen - runeLen(meta) - 1
		if budget < metaBudgetFloor { // 预算过小：退化，保持 meta 独立成条
			out = append(out, e)
			continue
		}
		pieces := packUnits(splitUnits(body, budget), budget)
		if len(pieces) == 0 { // 防御：正文非空但切分无产出（理论不可达）
			out = append(out, e)
			continue
		}
		out = out[:len(out)-1]
		out = append(out, pieces[:len(pieces)-1]...)
		out = append(out, pieces[len(pieces)-1]+"\n"+meta)
	}
	return out
}

// splitSegments 按边界行把文本拆成段：空行 = 段界；标题行 / 列表项行 = 新段起点。
// 行首尾空白与行尾 \r（CRLF）归一——只动空白分隔符，不丢内容字符。
func splitSegments(text string) []string {
	var segs []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		switch {
		case line == "":
			flush()
		case isBoundaryLine(line):
			flush()
			cur = append(cur, line)
		default:
			cur = append(cur, line)
		}
	}
	flush()
	return segs
}

// isBoundaryLine 判定边界行（新条目起点）：
//   - markdown 标题：1-6 个 '#' 后跟空格（## 标题）；
//   - 无序列表："- " / "* "；
//   - 有序列表："12. "（点后必须空格，防 3.14 这类小数误判）或 "12、"（顿号无需空格）。
func isBoundaryLine(line string) bool {
	if n := strspnByte(line, '#'); n >= 1 && n <= 6 && len(line) > n && line[n] == ' ' {
		return true
	}
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		return true
	}
	i := 0
	for i < len(line) && i < 3 && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(line) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(line[i:])
	switch r {
	case '、':
		return true
	case '.':
		return i+1 < len(line) && line[i+1] == ' '
	}
	return false
}

// strspnByte 统计 s 头部连续出现 c 的字节数（c 限 ASCII）。
func strspnByte(s string, c byte) int {
	i := 0
	for i < len(s) && s[i] == c {
		i++
	}
	return i
}

// splitUnits 把超长段拆成 ≤maxLen 的装包单元：先按句子切，单句仍超长按 rune 硬切。
// 段本身 ≤maxLen 时原样返回（保持贪心打包在统一装包层做）。
func splitUnits(seg string, maxLen int) []string {
	if runeLen(seg) <= maxLen {
		return []string{seg}
	}
	var units []string
	for _, s := range splitSentences(seg) {
		units = append(units, hardCut(s, maxLen)...)
	}
	return units
}

// splitSentences 句子切分：。！？!?与换行为句界，分隔符保留在前句尾部（不丢字符）。
// 不把半角 '.' 作句界——避免小数/版本号（3.14、v1.2）被切碎。
func splitSentences(s string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range s {
		cur.WriteRune(r)
		if r == '。' || r == '！' || r == '？' || r == '!' || r == '?' || r == '\n' {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// hardCut 单元超长时按 rune 切分（每片 ≤maxLen）；尽量把切点落在句界
// （。！？!?与换行），窗口内找不到句界才在 maxLen 处硬切（G14：硬切落非句界）。
func hardCut(s string, maxLen int) []string {
	r := []rune(s)
	if len(r) <= maxLen {
		return []string{s}
	}
	var out []string
	start := 0
	for start < len(r) {
		end := start + maxLen
		if end >= len(r) {
			out = append(out, string(r[start:]))
			break
		}
		cut := end
		for i := end - 1; i > start+maxLen/2; i-- {
			if isSentenceEnd(r[i]) {
				cut = i + 1
				break
			}
		}
		out = append(out, string(r[start:cut]))
		start = cut
	}
	return out
}

// isSentenceEnd 句界符（与 splitSentences 的句界集合一致）。
func isSentenceEnd(r rune) bool {
	switch r {
	case '。', '！', '？', '!', '?', '\n':
		return true
	}
	return false
}

// packUnits 贪心装包：相邻单元以 "\n" 连接成 ≤maxLen 的条目；放不下即封口开新条。
func packUnits(units []string, maxLen int) []string {
	var out []string
	var cur strings.Builder
	curLen := 0
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
			curLen = 0
		}
	}
	for _, u := range units {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		ul := runeLen(u)
		if curLen > 0 && curLen+1+ul > maxLen {
			flush()
		}
		if curLen > 0 {
			cur.WriteByte('\n')
			curLen++
		}
		cur.WriteString(u)
		curLen += ul
	}
	flush()
	return out
}

// runeLen 字数（rune 数）：中文语境「字」按 Unicode 码点计。
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// stripSpace 去除全部空白字符（测试断言 SplitEntries 无损契约用）。
func stripSpace(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
