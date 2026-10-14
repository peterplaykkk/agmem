package cleaner

import (
	"fmt"
	"strings"
	"testing"
)

// —— S1 前缀构建器：稳定性 / 追加 / 预算 / 分层 / 覆盖声明的表驱动测试 ——

func TestPrefixBuilder_StableHash(t *testing.T) {
	entries := []KnowledgeEntry{
		{ItemID: 1, DocName: "a.md", Dir: "a-clean", Text: "条目一"},
		{ItemID: 2, DocName: "b.md", Dir: "b-clean", Text: "条目二"},
	}
	cases := []struct {
		name         string
		targetDir    string
		buildTwice   [2][]KnowledgeEntry
		wantSameHash bool
	}{
		{
			name:         "同 entries 同 targetDir 两次构建 hash 相等",
			targetDir:    "a-clean",
			buildTwice:   [2][]KnowledgeEntry{entries, entries},
			wantSameHash: true,
		},
		{
			name:         "不同 targetDir 改变 L2 → hash 不同",
			targetDir:    "a-clean",
			buildTwice:   [2][]KnowledgeEntry{entries, entries},
			wantSameHash: true, // 见下方专项断言：另一次用 b-clean 比较
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &PrefixBuilder{}
			p1 := b.Build(c.buildTwice[0], c.targetDir)
			p2 := b.Build(c.buildTwice[1], c.targetDir)
			if c.wantSameHash && p1.Hash != p2.Hash {
				t.Fatalf("同输入 hash 不等：%s vs %s", p1.Hash, p2.Hash)
			}
			if len(p1.Hash) != 16 {
				t.Fatalf("hash 长度 = %d, want 16", len(p1.Hash))
			}
			if p1.Text != p2.Text {
				t.Fatalf("同输入 Text 不等")
			}
		})
	}
	// AC2 核心：同轮同目录逐字节相同；换目录（L2 变化）hash 必须不同。
	b := &PrefixBuilder{}
	pa := b.Build(entries, "a-clean")
	pb := b.Build(entries, "b-clean")
	if pa.Hash == pb.Hash {
		t.Fatalf("不同 targetDir 的 hash 不应相等（L2 变化必须反映到前缀）")
	}
}

// TestPrefixBuilder_AppendOnly 锁定 CLAUDE.md §3.1 的两个 append-only 断言：
//
//   - 正向（目标目录之外新增）：新条目落在 L1 尾（L2 为空场景）→ 旧 Text 是新 Text
//     的真前缀——这是 DeepSeek 缓存「旧前缀单元仍是新前缀前缀」的前提；
//   - 负向（目标目录之内新增）：新条目只落 L2，**L1 段（L0+L1）逐字节不变**——
//     L1 排除目标目录正是为了让本轮新产出不插进 L1 中段。
func TestPrefixBuilder_AppendOnly(t *testing.T) {
	b := &PrefixBuilder{}

	// —— 正向：目标目录之外追加 ——
	// targetDir=c-clean：库里无 c 目录条目 → L2 为空，Text = L0 + L1。
	base := []KnowledgeEntry{
		{ItemID: 1, DocName: "a.md", Dir: "a-clean", Text: "第一条知识"},
		{ItemID: 2, DocName: "a.md", Dir: "a-clean", Text: "第二条知识"},
	}
	old := b.Build(base, "c-clean")
	grown := append(append([]KnowledgeEntry{}, base...),
		KnowledgeEntry{ItemID: 3, DocName: "a.md", Dir: "a-clean", Text: "第三条知识"})
	nw := b.Build(grown, "c-clean")

	if !strings.HasPrefix(nw.Text, old.Text) {
		t.Fatalf("append-only 不变量被破坏：目标目录之外追加后旧 Text 不是新 Text 的前缀\nold=%q\nnew=%q", old.Text, nw.Text)
	}
	if old.Text == nw.Text {
		t.Fatalf("追加条目后 Text 应增长（真前缀）")
	}
	if !strings.Contains(nw.Text, "第三条知识") {
		t.Fatalf("追加的条目未进入 L1")
	}
	if nw.Hash == old.Hash {
		t.Fatalf("追加后 hash 必须变化")
	}
	// L2 为空时 L1 段 == 全文。
	if nw.L1Text != nw.Text {
		t.Fatalf("L2 为空时 L1Text 应等于 Text")
	}

	// —— 负向：目标目录之内追加 ——
	// base2 跨两个目录，保证 L1 非空（断言才有意义）。targetDir=a-clean：
	// L1=[x-clean 条目]，L2=[a-clean 条目]。
	base2 := []KnowledgeEntry{
		{ItemID: 1, DocName: "x.md", Dir: "x-clean", Text: "别的目录第一条"},
		{ItemID: 2, DocName: "a.md", Dir: "a-clean", Text: "目标目录第一条"},
	}
	old2 := b.Build(base2, "a-clean")
	if !strings.Contains(old2.L1Text, "别的目录第一条") {
		t.Fatalf("前提错误：L1 应为非空（含 x-clean 条目）")
	}
	if strings.Contains(old2.L1Text, "目标目录第一条") {
		t.Fatalf("前提错误：目标目录条目不应出现在 L1")
	}
	grown2 := append(append([]KnowledgeEntry{}, base2...),
		KnowledgeEntry{ItemID: 3, DocName: "a.md", Dir: "a-clean", Text: "目标目录第二条"})
	nw2 := b.Build(grown2, "a-clean")

	if nw2.L1Text != old2.L1Text {
		t.Fatalf("目标目录内新增条目改动了 L1 段（append-only 断裂）\nold L1=%q\nnew L1=%q",
			old2.L1Text, nw2.L1Text)
	}
	if !strings.Contains(nw2.Text, "目标目录第二条") {
		t.Fatalf("目标目录内新增条目未进入 L2")
	}
	// 目标目录内增长只追加在 L2 尾部 → 旧 Text 仍是新 Text 的真前缀。
	if !strings.HasPrefix(nw2.Text, old2.Text) {
		t.Fatalf("目标目录内增长应只在 L2 尾部追加：\nold=%q\nnew=%q", old2.Text, nw2.Text)
	}
	if nw2.Hash == old2.Hash {
		t.Fatalf("L2 增长后 hash 必须变化")
	}
}

// TestPrefixBuilder_Budget 超预算只砍尾部：保留最早条目、丢掉最新条目，EstTokens ≤ Budget。
// 预算按层切分（L1 拿扣除 L0 后余量的 3/4），故给足容纳一条、容不下两条的余量。
func TestPrefixBuilder_Budget(t *testing.T) {
	entries := []KnowledgeEntry{
		{ItemID: 1, DocName: "u1", Dir: "a-clean", Text: strings.Repeat("早", 100)},
		{ItemID: 2, DocName: "u2", Dir: "a-clean", Text: strings.Repeat("中", 100)},
		{ItemID: 3, DocName: "u3", Dir: "a-clean", Text: strings.Repeat("晚", 100)},
	}
	// targetDir=other-clean：三条都落 L1，验证 L1 的尾部裁剪。
	// 预算只够 L0 + 一条多：entryText≈100 rune → 62 tokens/条（含分隔）。
	// 余量 = budget - L0 = 160 → L1 额度 = 160*3/4 = 120：只装 1 条（62），两条（124）装不下。
	l0 := estimateTokens(CleanRulesL0)
	budget := l0 + 160
	b := &PrefixBuilder{Budget: budget}
	p := b.Build(entries, "other-clean")

	if p.EstTokens > budget {
		t.Fatalf("EstTokens=%d 超预算 %d", p.EstTokens, budget)
	}
	if !p.WithinBudget(budget) {
		t.Fatalf("WithinBudget=false, EstTokens=%d budget=%d", p.EstTokens, budget)
	}
	if !strings.Contains(p.Text, "早") {
		t.Fatalf("预算裁剪不应丢掉最早的条目")
	}
	if strings.Contains(p.Text, "晚") {
		t.Fatalf("预算裁剪应从尾部砍（最晚的条目）")
	}
	// L2 无条目（目标目录 other-clean 不在条目里）→ L2Tokens=0，L1Tokens==EstTokens。
	if p.L2Tokens != 0 || p.L1Tokens != p.EstTokens {
		t.Fatalf("无 L2 条目时应有 L2Tokens=0、L1Tokens==EstTokens：%+v", p)
	}
}

// TestPrefixBuilder_L1L2Exclusive L1 与 L2 互斥（同一目录条目只出现一次）+ 覆盖文档
// 去重计数（覆盖标识是文档名，见 prefix.go 文件头「已知偏差」）。
func TestPrefixBuilder_L1L2Exclusive(t *testing.T) {
	entries := []KnowledgeEntry{
		{ItemID: 1, DocName: "a.md", Dir: "a-clean", Text: "AAA"},
		{ItemID: 2, DocName: "b.md", Dir: "b-clean", Text: "BBB"},
		{ItemID: 3, DocName: "b.md", Dir: "b-clean", Text: "CCC"},
	}
	b := &PrefixBuilder{}
	p := b.Build(entries, "b-clean")

	// L1=[AAA]，L2=[BBB,CCC]：每个条目只出现一次（旧实现 L1+L2 重复 BBB/CCC）。
	if strings.Count(p.Text, "BBB") != 1 || strings.Count(p.Text, "CCC") != 1 {
		t.Fatalf("L1/L2 未互斥（目标目录条目重复计入）：%q", p.Text)
	}
	if strings.Count(p.Text, "AAA") != 1 {
		t.Fatalf("L1 条目数异常：%q", p.Text)
	}
	// 顺序：L1(AAA) 在前，L2(BBB,CCC) 在后，且 L2 按产出时间正序。
	if !(strings.Index(p.Text, "AAA") < strings.Index(p.Text, "BBB") &&
		strings.Index(p.Text, "BBB") < strings.Index(p.Text, "CCC")) {
		t.Fatalf("L1/L2 顺序异常：AAA<BBB<CCC 应成立：%q", p.Text)
	}
	// L1Text 只含 L1。
	if !strings.Contains(p.L1Text, "AAA") || strings.Contains(p.L1Text, "BBB") {
		t.Fatalf("L1Text 应只含 L1 段：%q", p.L1Text)
	}

	// 覆盖文档：去重、排序、唯一条目数（b.md 两条各计一次）。
	want := []string{"a.md", "b.md"}
	if len(p.CoveredDocs) != len(want) || p.CoveredDocs[0] != want[0] || p.CoveredDocs[1] != want[1] {
		t.Fatalf("CoveredDocs = %v, want %v", p.CoveredDocs, want)
	}
	if p.DocCounts["a.md"] != 1 || p.DocCounts["b.md"] != 2 {
		t.Fatalf("DocCounts = %v, want a.md=1 b.md=2", p.DocCounts)
	}
	lines := p.CoverageLines()
	if len(lines) != 2 || !strings.Contains(lines[0], "（1 条）") || !strings.Contains(lines[1], "（2 条）") {
		t.Fatalf("CoverageLines = %v", lines)
	}
}

// TestPrefixBuilder_ParentDirHook 父目录钩子：L2 取当前目录 + 父目录；这些目录的
// 条目从 L1 排除（L1 只留无关目录）。
func TestPrefixBuilder_ParentDirHook(t *testing.T) {
	entries := []KnowledgeEntry{
		{ItemID: 1, DocName: "u1", Dir: "root-clean", Text: "ROOT"},
		{ItemID: 2, DocName: "u2", Dir: "mid-clean", Text: "MID"},
		{ItemID: 3, DocName: "u3", Dir: "leaf-clean", Text: "LEAF"},
		{ItemID: 4, DocName: "u4", Dir: "other-clean", Text: "OTHER"},
	}
	parent := func(d string) string {
		switch d {
		case "leaf-clean":
			return "mid-clean"
		case "mid-clean":
			return "root-clean"
		}
		return ""
	}
	b := &PrefixBuilder{ParentDirFunc: parent}
	p := b.Build(entries, "leaf-clean")
	// L2 含 leaf + mid + root（各一次）；无关目录 other 留在 L1（一次）。
	for _, want := range []string{"LEAF", "MID", "ROOT", "OTHER"} {
		if strings.Count(p.Text, want) != 1 {
			t.Fatalf("%s 应恰好出现一次（L1/L2 互斥）：%q", want, p.Text)
		}
	}
	// L1（OTHER）在 L2（ROOT/MID/LEAF）之前。
	if strings.Index(p.Text, "OTHER") > strings.Index(p.Text, "ROOT") {
		t.Fatalf("L1 应排在 L2 之前：%q", p.Text)
	}
	if !strings.Contains(p.L1Text, "OTHER") {
		t.Fatalf("L1Text 应含无关目录条目：%q", p.L1Text)
	}
	for _, notL1 := range []string{"ROOT", "MID", "LEAF"} {
		if strings.Contains(p.L1Text, notL1) {
			t.Fatalf("目标目录（含父）条目不应出现在 L1：%s in %q", notL1, p.L1Text)
		}
	}

	// 钩子为空 → 降级为仅同目录：L2 只含 leaf，root/mid/other 留在 L1（各一次）。
	nb := &PrefixBuilder{}
	np := nb.Build(entries, "leaf-clean")
	if strings.Count(np.Text, "LEAF") != 1 || strings.Count(np.Text, "MID") != 1 || strings.Count(np.Text, "ROOT") != 1 {
		t.Fatalf("无父目录钩子时应降级为仅同目录：%q", np.Text)
	}
	if !strings.Contains(np.L1Text, "MID") || strings.Contains(np.L1Text, "LEAF") {
		t.Fatalf("无钩子时 L1 应含 mid、不含 leaf：%q", np.L1Text)
	}
}

// TestPrefixBuilder_ParentDirUnbounded 父链上溯**不封顶**：构造 5 层目录链（目标 + 4 层
// 祖先），额度足够时 L2 全部取到（DirDepth==5）——不再有「最多上溯 2 层」的人为上限，
// 能装多少由 L2 额度决定。
func TestPrefixBuilder_ParentDirUnbounded(t *testing.T) {
	dirs := []string{"d0-clean", "d1-clean", "d2-clean", "d3-clean", "d4-clean"}
	entries := make([]KnowledgeEntry, 0, len(dirs)+1)
	for i, d := range dirs {
		// 文本用可区分标记 T<dir>，避免互相包含。
		entries = append(entries, KnowledgeEntry{ItemID: int64(i + 1), DocName: d, Dir: d, Text: "T" + d})
	}
	entries = append(entries, KnowledgeEntry{ItemID: 99, DocName: "other", Dir: "other-clean", Text: "OTHER"})

	// 链：d0 → d1 → d2 → d3 → d4 → 根（d4 的父为空）。
	chain := map[string]string{
		"d0-clean": "d1-clean", "d1-clean": "d2-clean",
		"d2-clean": "d3-clean", "d3-clean": "d4-clean",
	}
	p := (&PrefixBuilder{ParentDirFunc: func(k string) string { return chain[k] }}).Build(entries, "d0-clean")

	if p.DirDepth != 5 {
		t.Fatalf("DirDepth=%d, want 5（额度足够时应上溯到根，不封顶）", p.DirDepth)
	}
	for _, d := range dirs {
		if strings.Count(p.Text, "T"+d) != 1 {
			t.Fatalf("%s 应恰好出现一次（L1/L2 互斥）：%q", d, p.Text)
		}
		if strings.Contains(p.L1Text, "T"+d) {
			t.Fatalf("L2 目录条目不应出现在 L1：%s in %q", d, p.L1Text)
		}
	}
	if !strings.Contains(p.L1Text, "OTHER") {
		t.Fatalf("无关目录条目应留在 L1：%q", p.L1Text)
	}
	if !p.WithinBudget(DefaultPrefixBudgetTokens) {
		t.Fatalf("EstTokens=%d 超默认预算 %d", p.EstTokens, DefaultPrefixBudgetTokens)
	}

	// 空映射（模拟 ParentDirFunc 取不到 wiki 父子关系）：返回空串、不 panic，降级仅同目录。
	ep := (&PrefixBuilder{ParentDirFunc: func(string) string { return "" }}).Build(entries, "d0-clean")
	if ep.DirDepth != 1 || strings.Contains(ep.L1Text, "Td0-clean") {
		t.Fatalf("父缺失应降级为仅同目录（DirDepth=1）：depth=%d L1=%q", ep.DirDepth, ep.L1Text)
	}
	for _, l1 := range []string{"Td1-clean", "Td2-clean", "Td3-clean", "Td4-clean"} {
		if !strings.Contains(ep.L1Text, l1) {
			t.Fatalf("父缺失时其余目录应留在 L1：%s 不在 %q", l1, ep.L1Text)
		}
	}
}

// TestPrefixBuilder_L2QuotaTruncatesNearToFar 额度不足时按「近→远」截断：L2 额度只够
// 目标目录 + 父目录时，取 d0 + d1 且 DirDepth==2；更远的祖先因额度耗尽不装入 L2（它们
// 仍在父链排除集合内，故也不进 L1——直接不出现）。
func TestPrefixBuilder_L2QuotaTruncatesNearToFar(t *testing.T) {
	dirs := []string{"d0-clean", "d1-clean", "d2-clean", "d3-clean"}
	marks := []string{"A", "B", "C", "D"}
	entries := []KnowledgeEntry{{ItemID: 1, DocName: "other", Dir: "other-clean", Text: strings.Repeat("O", 100)}}
	for i, d := range dirs {
		entries = append(entries, KnowledgeEntry{ItemID: int64(i + 2), DocName: d, Dir: d, Text: strings.Repeat(marks[i], 100)})
	}
	chain := map[string]string{"d0-clean": "d1-clean", "d1-clean": "d2-clean", "d2-clean": "d3-clean"}
	// 余量 500 → L1 额度 375、L2 额度 125；每条 100 rune → 62 tokens，装 2 条（124）即满。
	b := &PrefixBuilder{Budget: estimateTokens(CleanRulesL0) + 500, ParentDirFunc: func(k string) string { return chain[k] }}
	p := b.Build(entries, "d0-clean")

	if p.DirDepth != 2 {
		t.Fatalf("DirDepth=%d, want 2（L2 额度只够近两目录）", p.DirDepth)
	}
	if !strings.Contains(p.Text, strings.Repeat("A", 100)) || !strings.Contains(p.Text, strings.Repeat("B", 100)) {
		t.Fatalf("近两目录（d0,d1）应装入 L2")
	}
	if strings.Contains(p.Text, strings.Repeat("C", 100)) || strings.Contains(p.Text, strings.Repeat("D", 100)) {
		t.Fatalf("额度耗尽后更远的祖先（d2,d3）不应装入 L2：depth=%d", p.DirDepth)
	}
	// 近端优先：L2 段内 A 在 B 之前。
	if strings.Index(p.Text, strings.Repeat("A", 100)) > strings.Index(p.Text, strings.Repeat("B", 100)) {
		t.Fatalf("L2 应按「近→远」装填：A 应在 B 之前")
	}
	if !strings.Contains(p.L1Text, strings.Repeat("O", 100)) {
		t.Fatalf("无关目录条目应在 L1")
	}
	if !p.WithinBudget(b.Budget) {
		t.Fatalf("EstTokens=%d 超预算 %d", p.EstTokens, b.Budget)
	}
}

// TestContextDirChain 上溯安全：nil 钩子降级、父缺失即停、成环检测（不 panic、不死循环）。
func TestContextDirChain(t *testing.T) {
	if got := contextDirChain("a", nil); len(got) != 1 || got[0] != "a" {
		t.Fatalf("nil parent chain = %v, want [a]", got)
	}
	root := map[string]string{"a": "b", "b": "c"} // c 无父 → 根
	got := contextDirChain("a", func(k string) string { return root[k] })
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("chain = %v, want [a b c]", got)
	}
	cyc := map[string]string{"a": "b", "b": "c", "c": "a"} // 环
	if got := contextDirChain("a", func(k string) string { return cyc[k] }); len(got) != 3 {
		t.Fatalf("成环链应停在重复目录，chain = %v, want [a b c]", got)
	}
	self := map[string]string{"a": "a"} // 自环
	if got := contextDirChain("a", func(k string) string { return self[k] }); len(got) != 1 {
		t.Fatalf("自环链 = %v, want [a]", got)
	}
}

// TestPrefixBuilder_L1ConstantAcrossTargetDirs L1 跨目录恒定（缓存连续性回归护栏）：
// 同一批条目下，两个不同目标目录 p/q（兄弟，共享有内容的祖先 r；p/q 自身尚无条目——
// 目录刚建、还没清洗过的常见态）的 L1Text（L0+L1）必须逐字节相同。
//
// 注意范围（设计使然，非缺陷）：L1 = 全库条目**排除目标目录及其整条父链**，故 L1Text
// 依赖「父链中有内容的目录集合」。若两个目标目录自身都有条目，其中一方的条目会落入
// 另一方的 L1，L1Text 本就会不同——那种情况下再断言恒等是错的。本护栏锁定的是：
// 在二者的「有内容父链」相同的真实形态（同一父下的兄弟子树）下，L1 不随目标目录或
// L2 体积变化。更强的「L2 体积不压缩 L1」断言见
// TestPrefixBuilder_L1IndependentOfL2Quota。
func TestPrefixBuilder_L1ConstantAcrossTargetDirs(t *testing.T) {
	entries := []KnowledgeEntry{
		{ItemID: 1, DocName: "g", Dir: "g-clean", Text: "G1"},
		{ItemID: 2, DocName: "g", Dir: "g-clean", Text: "G2"},
		{ItemID: 3, DocName: "h", Dir: "h-clean", Text: "H1"},
		{ItemID: 4, DocName: "r", Dir: "r-clean", Text: "R1"},
		{ItemID: 5, DocName: "r", Dir: "r-clean", Text: "R2"},
	}
	chain := map[string]string{"p-clean": "r-clean", "q-clean": "r-clean"}
	b := &PrefixBuilder{ParentDirFunc: func(k string) string { return chain[k] }}

	pp := b.Build(entries, "p-clean")
	pq := b.Build(entries, "q-clean")

	if pp.L1Text != pq.L1Text {
		t.Fatalf("L1Text 随目标目录变化（预算切分让 L2 挤占 L1，缓存连续性断裂）:\np=%q\nq=%q", pp.L1Text, pq.L1Text)
	}
	if !strings.Contains(pp.L1Text, "G1") || !strings.Contains(pp.L1Text, "H1") {
		t.Fatalf("L1 应含链外目录条目：%q", pp.L1Text)
	}
	for _, notL1 := range []string{"R1", "R2"} {
		if strings.Contains(pp.L1Text, notL1) {
			t.Fatalf("共享祖先条目不应出现在 L1：%s in %q", notL1, pp.L1Text)
		}
	}
}

// TestPrefixBuilder_L1IndependentOfL2Quota 预算切分核心护栏：L2 体积暴涨到**撑满 L2
// 额度**也不得压缩 L1——同一目标目录下，L1Text 与 L1Tokens 必须与 L2 很小时逐字节/数值
// 相同。若 L1/L2 共用一个池子（或 L2 无上限先占），L2 饱和就会把 L1 挤小，跨目录缓存
// 连续性随之断裂——本测试即锁定该风险。
func TestPrefixBuilder_L1IndependentOfL2Quota(t *testing.T) {
	base := []KnowledgeEntry{
		{ItemID: 1, DocName: "g", Dir: "g-clean", Text: strings.Repeat("G", 100)},
		{ItemID: 2, DocName: "g", Dir: "g-clean", Text: strings.Repeat("H", 100)},
	}
	small := append(append([]KnowledgeEntry{}, base...),
		KnowledgeEntry{ItemID: 3, DocName: "r", Dir: "r-clean", Text: strings.Repeat("R", 100)})
	big := append([]KnowledgeEntry{}, base...)
	for i := 0; i < 2000; i++ { // 2000×62 ≈ 124k tokens ≫ L2 额度（默认 1/4 ≈ 50k）→ L2 饱和
		big = append(big, KnowledgeEntry{ItemID: int64(100 + i), DocName: "r", Dir: "r-clean", Text: strings.Repeat("r", 100)})
	}
	b := &PrefixBuilder{}
	ps := b.Build(small, "r-clean")
	pb := b.Build(big, "r-clean")

	if pb.L2Tokens <= ps.L2Tokens {
		t.Fatalf("测试前提错误：big 的 L2 应更大：%d vs %d", pb.L2Tokens, ps.L2Tokens)
	}
	if ps.L1Text != pb.L1Text || ps.L1Tokens != pb.L1Tokens {
		t.Fatalf("L2 饱和压缩了 L1（预算未按层切分）：L1Tokens %d → %d", ps.L1Tokens, pb.L1Tokens)
	}
	if !strings.Contains(pb.L1Text, strings.Repeat("G", 100)) {
		t.Fatalf("L1 内容缺失")
	}
	if !pb.WithinBudget(DefaultPrefixBudgetTokens) {
		t.Fatalf("L2 饱和后 EstTokens=%d 超预算", pb.EstTokens)
	}
}

// TestPrefixBuilder_WithinBudgetExhaustive 穷举目录/条目规模组合：总量恒 ≤ 默认预算，
// 且 L1Tokens+L2Tokens==EstTokens。
func TestPrefixBuilder_WithinBudgetExhaustive(t *testing.T) {
	// 8 层目录链 d0→d1→…→d7。
	chain := map[string]string{}
	for i := 0; i < 7; i++ {
		chain[fmt.Sprintf("d%d-clean", i)] = fmt.Sprintf("d%d-clean", i+1)
	}
	parent := func(k string) string { return chain[k] }
	for _, perDir := range []int{0, 1, 5, 50} {
		for _, textLen := range []int{0, 10, 100, 4000} {
			for _, ndirs := range []int{1, 3, 8} {
				var entries []KnowledgeEntry
				id := int64(0)
				for d := 0; d < ndirs; d++ {
					dir := fmt.Sprintf("d%d-clean", d)
					for j := 0; j < perDir; j++ {
						id++
						entries = append(entries, KnowledgeEntry{
							ItemID: id, DocName: dir, Dir: dir, Text: strings.Repeat("字", textLen),
						})
					}
				}
				// 再补一批「链外」条目填充 L1，使 L1 与 L2 同时受压（只填 L2 时无法
				// 暴露 L1+L2 共池导致的超预算）。
				for j := 0; j < perDir; j++ {
					id++
					entries = append(entries, KnowledgeEntry{
						ItemID: id, DocName: "global", Dir: "global-clean", Text: strings.Repeat("全", textLen),
					})
				}
				p := (&PrefixBuilder{ParentDirFunc: parent}).Build(entries, "d0-clean")
				if !p.WithinBudget(DefaultPrefixBudgetTokens) {
					t.Fatalf("perDir=%d textLen=%d ndirs=%d: EstTokens=%d 超预算 %d",
						perDir, textLen, ndirs, p.EstTokens, DefaultPrefixBudgetTokens)
				}
				if p.L1Tokens+p.L2Tokens != p.EstTokens {
					t.Fatalf("perDir=%d textLen=%d ndirs=%d: L1(%d)+L2(%d) != Est(%d)",
						perDir, textLen, ndirs, p.L1Tokens, p.L2Tokens, p.EstTokens)
				}
			}
		}
	}
}

// TestPrefix_WithinBudgetBoundary 硬断言边界：恰好等于预算为真、超 1 为假；budget<=0 用默认。
func TestPrefix_WithinBudgetBoundary(t *testing.T) {
	cases := []struct {
		est, budget int
		want        bool
	}{
		{est: 200000, budget: 200000, want: true},
		{est: 200001, budget: 200000, want: false},
		{est: 200000, budget: 0, want: true},  // 0 → 默认 200000
		{est: 200001, budget: 0, want: false}, // 0 → 默认 200000
		{est: 0, budget: 1, want: true},
	}
	for _, c := range cases {
		if got := (Prefix{EstTokens: c.est}).WithinBudget(c.budget); got != c.want {
			t.Errorf("WithinBudget(est=%d,budget=%d) = %v, want %v", c.est, c.budget, got, c.want)
		}
	}
	if DefaultPrefixBudgetTokens != 200000 {
		t.Fatalf("DefaultPrefixBudgetTokens = %d, want 200000", DefaultPrefixBudgetTokens)
	}
}

// TestPrefixQuotas 预算切分：L1+L2 == budget-L0（L0 恒定预留），L1 拿 3/4 余量。
func TestPrefixQuotas(t *testing.T) {
	l0 := estimateTokens(CleanRulesL0)
	for _, budget := range []int{200000, 10000, l0 + 4, l0, l0 - 1, 0} {
		l1, l2 := prefixQuotas(budget)
		if l1 < 0 || l2 < 0 {
			t.Fatalf("budget=%d: 额度为负 l1=%d l2=%d", budget, l1, l2)
		}
		b := budget
		if b <= 0 {
			b = DefaultPrefixBudgetTokens
		}
		if l0 <= b && l1+l2 != b-l0 {
			t.Fatalf("budget=%d: l1(%d)+l2(%d) != budget-L0(%d)", budget, l1, l2, b-l0)
		}
		if l1+l2 > b {
			t.Fatalf("budget=%d: l1+l2(%d) 超预算 %d", budget, l1+l2, b)
		}
	}
	// 比例：4 余量 → L1=3、L2=1。
	if l1, l2 := prefixQuotas(l0 + 4); l1 != 3 || l2 != 1 {
		t.Fatalf("prefixQuotas(L0+4) = (%d,%d), want (3,1)", l1, l2)
	}
}

// —— S2 L3 组装：目标文档 + 覆盖声明（标识用文档名，非 URL）——

func TestPrefixUserBlock(t *testing.T) {
	entries := []KnowledgeEntry{
		{ItemID: 1, DocName: "a.md", Dir: "a-clean", Text: "AAA"},
		{ItemID: 2, DocName: "a.md", Dir: "a-clean", Text: "BBB"},
		{ItemID: 3, DocName: "b.md", Dir: "b-clean", Text: "CCC"},
	}
	b := &PrefixBuilder{}
	p := b.Build(entries, "b-clean")
	user := PrefixUserBlock(p, "target.md", "target.md", "本次正文内容", nil)

	for _, want := range []string{
		"【本前缀覆盖的文档】",
		"a.md（2 条）",
		"b.md（1 条）",
		"**不要**从这些文档提取条目",
		"【本次提取目标文档】target.md",
		"只针对该文档的正文输出知识条目",
		"【本次入库文档名】target.md",
		"{{SOURCE}}",
		"【正文】\n本次正文内容",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("L3 缺 %q\n%s", want, user)
		}
	}
	// 系统段（前缀 Text）绝不含目标/正文——否则前缀逐请求变化，缓存归零。
	for _, forbidden := range []string{"target.md", "本次正文内容"} {
		if strings.Contains(p.Text, forbidden) {
			t.Errorf("前缀 Text 混入每请求唯一变的内容 %q", forbidden)
		}
	}

	// 无覆盖文档时仍给出声明块（round 0，L0-only）。
	empty := (&PrefixBuilder{}).Build(nil, "x-clean")
	u2 := PrefixUserBlock(empty, "t.md", "", "正文", nil)
	if !strings.Contains(u2, "（无）") || !strings.Contains(u2, "**不要**从这些文档提取条目") {
		t.Fatalf("空前缀 L3 未给出覆盖声明：\n%s", u2)
	}
	if !strings.Contains(u2, "未命名文档") {
		t.Fatalf("空 source 应回落未命名文档：\n%s", u2)
	}

	// 锚点约束进 L3（user），不进前缀 Text。
	u3 := PrefixUserBlock(empty, "t.md", "s.md", "正文", []string{"1.2.3.4", "1194"})
	if !strings.Contains(u3, StrictAnchorPrefix) || !strings.Contains(u3, "1.2.3.4") {
		t.Fatalf("L3 未包含锚点约束：\n%s", u3)
	}
}

func TestEstimateTokens_Conservative(t *testing.T) {
	// 0.6 token/rune，向上取整；保守方向 = 高估。
	got := estimateTokens(strings.Repeat("字", 100))
	if got != 60 {
		t.Fatalf("estimateTokens(100 rune) = %d, want 60", got)
	}
	if estimateTokens("") != 0 {
		t.Fatalf("空串 token 应为 0")
	}
}
