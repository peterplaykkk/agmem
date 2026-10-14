package cleaner

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// —— SplitEntries 分条（2026-09-07 分条批核心）——

// rep 生成 n 个字 c 的字符串（构造指定 rune 长度文本用）。
func rep(c rune, n int) string { return strings.Repeat(string(c), n) }

// checkSplitInvariants 全 case 通用断言：每条 ≤maxLen、内容无损（去除合成行
// 「（续）…：」「来源：…」后非空白字符按原序全保留）、无空条目。
func checkSplitInvariants(t *testing.T, name, in string, got []string, maxLen int) {
	t.Helper()
	for i, e := range got {
		if e == "" {
			t.Errorf("%s: entry %d 为空串", name, i)
		}
		if n := runeLen(e); n > maxLen {
			t.Errorf("%s: entry %d 长度 %d 超上限 %d", name, i, n, maxLen)
		}
	}
	if want, gotJoin := stripSpace(stripSynthLines(in)), stripSpace(stripSynthLines(strings.Join(got, "\n"))); gotJoin != want {
		t.Errorf("%s: 内容无损破坏\n got=%q\nwant=%q", name, gotJoin, want)
	}
}

func TestSplitEntries(t *testing.T) {
	threeEntries := "## 条目一\n" + rep('甲', 380) + "\n\n## 条目二\n" + rep('乙', 380) +
		"\n\n## 条目三\n" + rep('丙', 380)

	cases := []struct {
		name    string
		in      string
		maxLen  int
		want    []string // 精确期望（nil = 无条目）；wantNum 断言条数
		wantNum int      // -1 = 不断言条数，以 want 为准
	}{
		{"空串", "", 600, nil, -1},
		{"全空白", " \n\n \t ", 600, nil, -1},
		{"短文单段", "一条记录：部署完成", 600, []string{"一条记录：部署完成"}, -1},
		{"maxLen0不切分", rep('字', 900), 0, []string{rep('字', 900)}, -1},
		{"maxLen负不切分", rep('字', 50), -1, []string{rep('字', 50)}, -1},
		// 贪心打包：多标题短内容合入一条（命中长度最小化，条数服从 ≤600）
		{"多标题短内容打包为一条", "## 标题一\n内容一。\n\n## 标题二\n内容二。", 600,
			[]string{"## 标题一\n内容一。\n## 标题二\n内容二。"}, 1},
		// 三条各 ~387 字：两两装不下 → 3 条
		{"三条目各自成点", threeEntries, 600, nil, 3},
		// 无边界超长：硬切兜底
		{"无边界1200字硬切", rep('字', 1200), 600, []string{rep('字', 600), rep('字', 600)}, 2},
		// 句读切分：60 句 × 20 字 = 1200 字；20k+(k-1)≤600 → 每条最多 28 句（587 字）
		// → 28/28/4 共 3 条
		{"句读切分装包", strings.TrimSuffix(strings.Repeat(strings.Repeat("句", 19)+"。", 60), ""), 600, nil, 3},
		// 精确 600 边界：不切
		{"精确600单条", rep('字', 600), 600, []string{rep('字', 600)}, 1},
		// 601 字无边界：硬切 600 + 1
		{"601字硬切600+1", rep('字', 601), 600, []string{rep('字', 600), rep('字', 1)}, 2},
		// 编号列表：每项边界 + 相邻装包
		{"编号列表边界", "1. 第一步\n2. 第二步\n3. 第三步", 600,
			[]string{"1. 第一步\n2. 第二步\n3. 第三步"}, 1},
		{"顿号编号边界", "1、甲项\n2、乙项", 600, []string{"1、甲项\n2、乙项"}, 1},
		// 空行分段
		{"空行分段装包", "段落甲第一行\n段落甲第二行\n\n段落乙", 600,
			[]string{"段落甲第一行\n段落甲第二行\n段落乙"}, 1},
		// CRLF 行尾归一
		{"CRLF归一", "第一条\r\n第二条\r\n", 600, []string{"第一条\n第二条"}, 1},
		// 小数不误判列表项（句界/列表边界都不应在 3.14 处切）
		{"小数不误判边界", "圆周率取 3.14 计算\n半径 2.5 米", 600,
			[]string{"圆周率取 3.14 计算\n半径 2.5 米"}, 1},
		// 单条目超长带句读：句子装包不丢句号
		{"超长段句切保留句号", strings.Repeat("内容句子。", 200), 600, nil, 2},
	}
	for _, c := range cases {
		got := SplitEntries(c.in, c.maxLen)
		if c.wantNum >= 0 {
			if len(got) != c.wantNum {
				t.Errorf("%s: got %d entries, want %d\ngot=%#v", c.name, len(got), c.wantNum, got)
				continue
			}
		}
		if c.want != nil {
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s:\n got=%#v\nwant=%#v", c.name, got, c.want)
				continue
			}
		}
		ml := c.maxLen
		if ml <= 0 {
			ml = runeLen(c.in) + 1 // 不切分模式不做长度断言
		}
		checkSplitInvariants(t, c.name, c.in, got, ml)
	}
}

func TestSplitEntries_PackingBoundary(t *testing.T) {
	// 两段各 300 字：300+1+300=601 > 600 → 拆两条；各 299：299+1+299=599 ≤ 600 → 一条
	in300 := rep('甲', 300) + "\n\n" + rep('乙', 300)
	if got := SplitEntries(in300, 600); len(got) != 2 {
		t.Errorf("300+300 必须拆两条（601>600），got %d", len(got))
	}
	in299 := rep('甲', 299) + "\n\n" + rep('乙', 299)
	got := SplitEntries(in299, 600)
	if len(got) != 1 || runeLen(got[0]) != 599 {
		t.Errorf("299+299 应打包为一条 599 字，got %#v", got)
	}
}

func TestSplitEntries_LongListItemsStayComplete(t *testing.T) {
	// 执行步骤场景：单条编号项 ~450 字 ×2 —— 项间边界切分，每项内部（无句读）不硬切
	item1 := "1. " + rep('甲', 450)
	item2 := "2. " + rep('乙', 450)
	got := SplitEntries(item1+"\n\n"+item2, 600)
	if len(got) != 2 {
		t.Fatalf("两个 450 字编号项应各成一条，got %d: %#v", len(got), got)
	}
	if got[0] != item1 || got[1] != item2 {
		t.Errorf("编号项内容被改写:\n got[0]=%q\nwant[0]=%q", got[0][:30], item1[:30])
	}
}

// —— 边界行判定（isBoundaryLine 直接表驱动）——

func TestIsBoundaryLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"二级标题", "## 标题", true},
		{"三级标题", "### 小节", true},
		{"一级标题", "# 标题", true},
		{"六级标题", "###### 深层", true},
		{"井号后无空格非标题", "##标签", false},
		{"七级井号非标题", "####### 过深", false},
		{"无序列表减号", "- 项目", true},
		{"无序列表星号", "* 项目", true},
		{"减号后无空格非列表", "-5 度", false},
		{"编号点空格", "12. 步骤", true},
		{"编号点无空格非列表", "3.14 是圆周率", false},
		{"编号顿号", "1、第一", true},
		{"编号顿号无空格", "12、第十二", true},
		{"纯数字非边界", "123", false},
		{"四位数字点非边界", "1234. 过长编号", false},
		{"普通正文", "普通正文一行", false},
		{"右尖括号引用非边界", "> 引用", false},
	}
	for _, c := range cases {
		if got := isBoundaryLine(c.in); got != c.want {
			t.Errorf("%s: isBoundaryLine(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// —— 占位过滤 × 分条管线（并入上个占位过滤批的全部用例语义）——

func TestFilterPlaceholderEntries(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"空输入", nil, nil},
		{"纯占位条目", []string{`""`, `（空）`}, nil},
		{"占位与正文混合", []string{`""`, "镜像使用方法", `（空）`, "部署脚本见 ops"},
			[]string{"镜像使用方法", "部署脚本见 ops"}},
		{"正文原样保留", []string{"一条记录：部署完成"}, []string{"一条记录：部署完成"}},
		{"带项目符号的占位行不判占位", []string{`- ""`}, []string{`- ""`}},
		{"全部占位返回空", []string{"无", "N/A", `"空"`}, nil},
	}
	for _, c := range cases {
		got := filterPlaceholderEntries(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: filterPlaceholderEntries(%#v) = %#v, want %#v", c.name, c.in, got, c.want)
		}
	}
}

// TestDropPlaceholderLines 行级占位中和：占位行置换为空行（保留段落边界），
// 非占位行原样——上个占位过滤批「行级剔除」的升级语义。
func TestDropPlaceholderLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"占位行置换为空行", "\"\"\n正文甲\n（空）\n正文乙", "\n正文甲\n\n正文乙"},
		{"带项目符号的占位行不中和", "- \"\"", `- ""`},
		{"无占位原样", "正文甲\n正文乙", "正文甲\n正文乙"},
		{"空串", "", ""},
		{"CRLF 占位行", "\"\"\r\n正文", "\n正文"},
	}
	for _, c := range cases {
		if got := dropPlaceholderLines(c.in); got != c.want {
			t.Errorf("%s: dropPlaceholderLines(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// TestCleanPipeline_PlaceholderFiltering：上个占位过滤批（行级 filterCleanEntries）
// 的用例在「行级中和 → 分条 → 条目级过滤」管线下的等价语义回归。
// 注意：分条批起相邻短段会贪心装包（≤600 合一条），故期望值按装包后形态断言——
// 占位被剔除、正文全保留的语义与上个批完全一致，条数由装包规则决定。
func TestCleanPipeline_PlaceholderFiltering(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"空响应", "", nil},
		{"纯占位响应", "\"\"\n（空）", nil},
		{"占位与正文混合（两短段装包为一条）", "\"\"\n镜像使用方法\n\n（空）\n部署脚本见 ops",
			[]string{"镜像使用方法\n部署脚本见 ops"}},
		{"正文原样保留", "一条记录：部署完成", []string{"一条记录：部署完成"}},
		{"CRLF 行尾归一（短行装包为一条）", "第一条\r\n第二条\r\n", []string{"第一条\n第二条"}},
		{"带项目符号的占位行按规格不判占位", "- \"\"", []string{`- ""`}},
		{"占位行与正文无空行分隔也不粘连", "\"\"\n镜像使用方法", []string{"镜像使用方法"}},
	}
	for _, c := range cases {
		got := filterPlaceholderEntries(SplitEntries(dropPlaceholderLines(c.in), MaxEntryLen))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: pipeline(%q) = %#v, want %#v", c.name, c.in, got, c.want)
		}
	}
}

// —— 微型条目过滤（2026-09-08 混合检索批：MinEntryLen 下限）——

// TestFilterMicroEntries <15 字条目丢弃；=15 保留；顺序保持；全微 → nil。
func TestFilterMicroEntries(t *testing.T) {
	long := rep('长', 20) // 20 字
	exact := rep('刚', 15)
	short6 := "镜像使用方法" // 6 字：旧测试中的「正常短条目」，本批起不足下限
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "空入参", in: nil, want: nil},
		{name: "全部微型全剔除", in: []string{"好短", short6, rep('短', 14)}, want: nil},
		{name: "恰好15字保留", in: []string{exact}, want: []string{exact}},
		{name: "混合顺序保持", in: []string{short6, long, "步骤一", exact}, want: []string{long, exact}},
		{name: "空白不抵长度", in: []string{"  " + short6 + "  "}, want: nil}, // 6 字非空白
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterMicroEntries(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("filterMicroEntries(%#v) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}

// TestCleanPipeline_MicroEntryFiltering：完整条目管线（行级占位中和 → 分条 →
// 占位过滤 → 微型过滤）语义。上个批的占位管线用例仍钉在
// TestCleanPipeline_PlaceholderFiltering（只测内层占位语义，未含微型过滤）。
//
// 语义注意：SplitEntries 会把相邻短段贪心打包（≤600），微型段与正文粘连后
// 整体条目 ≥MinEntryLen 即保留——过滤对象是「最终成条」的条目，不是原始段；
// 微型段单独成条（打包后仍不足下限）才丢弃。
func TestCleanPipeline_MicroEntryFiltering(t *testing.T) {
	long := rep('长', 30)
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"微型条目单条丢弃", "镜像使用方法", nil},
		{"占位短行本就剔除且不足下限", "- \"\"", nil},
		{"微型段与正文粘包成条目保留（条目信息量足）", "好短\n\n" + long, []string{"好短\n" + long}},
		{"单段内换行不分单元整体不足下限丢弃", "第一条说明\n第二条说明", nil},
		{"全部微型走dedup语义", "第一步\n第二步\n第三步", nil},
		{"打包后恰15字保留", "部署脚本步骤一\n\n部署脚本步骤二", []string{"部署脚本步骤一\n部署脚本步骤二"}},
		{"打包后13字丢弃", "部署脚本步骤\n\n部署脚本步骤", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CleanEntries(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("CleanEntries(%q) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}

	// MinEntryLen 契约值（<15 丢弃，=15 保留）
	if MinEntryLen != 15 {
		t.Fatalf("MinEntryLen = %d, want 15（规格拍板值）", MinEntryLen)
	}
}

// —— 纯元数据块折叠（2026-09-09 线上缺陷修复：59/2577 clean 点正文为空）——

// TestIsMetaLine 元数据行判定：只认行首 来源：/适用：，正文中间的词不误判。
// 前导空白由调用方 TrimSpace 归一（本函数不做 trim）。
func TestIsMetaLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"来源行", "来源：统一出口代理", true},
		{"适用行", "适用：怎么推代码", true},
		{"带前导空白未归一不匹配", "  来源：文档X", false},
		{"正文含来源词", "详见来源：wiki 条目", false},
		{"正文含适用词", "本方案适用：生产环境", false},
		{"来源后无全角冒号", "来源 文档X", false},
		{"空行", "", false},
		{"普通正文", "部署脚本见 ops", false},
	}
	for _, c := range cases {
		if got := isMetaLine(c.in); got != c.want {
			t.Errorf("%s: isMetaLine(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestIsMetaOnlyEntry 纯元数据条目判定：非空行全为元数据行才 true。
func TestIsMetaOnlyEntry(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"仅来源行", "来源：文档X", true},
		{"来源+适用", "来源：文档X\n适用：推代码", true},
		{"元数据行间夹空行", "来源：文档X\n\n适用：推代码", true},
		{"正文+元数据", "部署步骤见 wiki\n来源：文档X", false},
		{"正文含来源词", "详见来源：wiki", false},
		{"空串", "", false},
		{"纯空行", " \n\t", false},
	}
	for _, c := range cases {
		if got := isMetaOnlyEntry(c.in); got != c.want {
			t.Errorf("%s: isMetaOnlyEntry(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestSplitEntries_NoMetaOnlyEntries 缺陷修复核心：元数据行不得自成无正文条目。
// 三种线上形态（元数据同段 / 元数据被空行分开 / 多段正文后置元数据）均须折叠。
func TestSplitEntries_NoMetaOnlyEntries(t *testing.T) {
	body590 := rep('甲', 590)
	cases := []struct {
		name    string
		in      string
		wantNum int // -1 = 只断言无纯元数据条目
	}{
		{"长正文+元数据同段", body590 + "\n\n来源：某文档\n适用：生产环境", -1},
		{"长正文+元数据被空行分开", body590 + "\n\n来源：某文档\n\n适用：生产环境", -1},
		{"多段长正文+元数据", rep('甲', 590) + "\n\n" + rep('乙', 595) + "\n\n来源：文档X", 3},
		{"正文中来源词不误判", "详见来源：wiki 条目\n\n" + rep('丁', 590), 2},
	}
	for _, c := range cases {
		got := SplitEntries(c.in, 600)
		if c.wantNum >= 0 && len(got) != c.wantNum {
			t.Errorf("%s: got %d entries, want %d\ngot=%#v", c.name, len(got), c.wantNum, got)
		}
		for i, e := range got {
			if isMetaOnlyEntry(e) {
				t.Errorf("%s: entry %d 仍是纯元数据块（正文为空）: %q", c.name, i, e)
			}
		}
		checkSplitInvariants(t, c.name, c.in, got, 600)
	}
}

// TestSplitEntries_MetaFoldsIntoLastContentBlock 元数据随最后一个内容块：折叠后
// 末条含元数据后缀且正文非空，总条数不变、每条 ≤600。
func TestSplitEntries_MetaFoldsIntoLastContentBlock(t *testing.T) {
	in := rep('甲', 590) + "\n\n" + rep('乙', 595) + "\n\n来源：文档X"
	got := SplitEntries(in, 600)
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d: %#v", len(got), got)
	}
	last := got[len(got)-1]
	if !strings.HasSuffix(last, "来源：文档X") {
		t.Errorf("末条未带元数据后缀: %q", last)
	}
	if isMetaOnlyEntry(last) {
		t.Errorf("末条正文为空（元数据未依附内容）: %q", last)
	}
	checkSplitInvariants(t, "meta-folds", in, got, 600)
}

// TestSplitEntries_PureMetaInput 纯元数据输入（无正文可依附）→ 单条原样保留
// （退化输入，不做改写；调用方由条目过滤/人工处置）。
func TestSplitEntries_PureMetaInput(t *testing.T) {
	in := "来源：某文档\n适用：生产环境"
	got := SplitEntries(in, 600)
	if !reflect.DeepEqual(got, []string{in}) {
		t.Errorf("纯元数据输入应单条原样保留, got %#v", got)
	}
	checkSplitInvariants(t, "pure-meta", in, got, 600)
}

// TestSplitEntries_MetaBudgetDegenerate 预算退化：元数据过长（budget<100）时保持
// 独立成条——不把正文切得过碎，且每条仍 ≤600、无损契约成立。
func TestSplitEntries_MetaBudgetDegenerate(t *testing.T) {
	meta := "来源：" + rep('文', 546) // 549 字 → budget = 600-549-1 = 50 < 100
	in := rep('甲', 500) + "\n\n" + meta
	got := SplitEntries(in, 600)
	if len(got) != 2 {
		t.Fatalf("退化分支应保留 2 条, got %d: %#v", len(got), got)
	}
	if got[1] != meta {
		t.Errorf("退化分支应原样保留元数据条目, got %q", got[1])
	}
	checkSplitInvariants(t, "budget-degenerate", in, got, 600)
}

// TestFoldMetaOnlyEntries_Unit foldMetaOnlyEntries 直接表驱动（含无 prev 与
// 前一条也是纯元数据的退化路径）。
func TestFoldMetaOnlyEntries_Unit(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		wantNum int
	}{
		{"无纯元数据条目原样", []string{"正文甲", "正文乙"}, 2},
		{"首条纯元数据保留", []string{"来源：X", "正文甲"}, 2},
		{"元数据并入前一条", []string{rep('甲', 590), "来源：X"}, 1},
		{"前一条也是纯元数据退化", []string{"来源：X", "适用：Y"}, 2},
	}
	for _, c := range cases {
		got := foldMetaOnlyEntries(c.in, 600)
		if len(got) != c.wantNum {
			t.Errorf("%s: got %d entries, want %d: %#v", c.name, len(got), c.wantNum, got)
		}
	}
}

// TestSplitEntries_MetaFoldProperty 边界扫描：正文长度 0..620 步进 7 × 多种元数据
// 形态，逐一断言无损契约（stripSpace(join)==stripSpace(in)）、每条 ≤600；折叠预算
// 充足（runeLen(meta)≤499）且正文非空时不得残留纯元数据块。退化预算（549 字元数据）
// 只断言无损与长度上限。
func TestSplitEntries_MetaFoldProperty(t *testing.T) {
	metas := []string{
		"来源：文档X",
		"适用：怎么推代码",
		"来源：文档X\n适用：怎么推代码",
		"来源：" + rep('文', 200),
		"来源：" + rep('文', 549), // budget = 600-549-1 = 50 < 100：退化分支
	}
	for _, meta := range metas {
		metaLen := runeLen(meta)
		for bodyLen := 0; bodyLen <= 620; bodyLen += 7 {
			in := rep('甲', bodyLen) + "\n\n" + meta
			name := fmt.Sprintf("body=%d meta=%d", bodyLen, metaLen)
			got := SplitEntries(in, 600)
			checkSplitInvariants(t, name, in, got, 600)
			if bodyLen > 0 && metaLen <= 499 {
				for i, e := range got {
					if isMetaOnlyEntry(e) {
						t.Errorf("%s: entry %d 残留纯元数据块: %q", name, i, e)
					}
				}
			}
		}
	}
}

// —— G14：续片承接主题句 / 悬空末行 / 硬切落句界 ——

// TestSplitEntries_ContinuationTopicPrefix：同一段被切成多条时，续片以
// 「（续）<段主题>：」承接式主题句开头（首条保留原标题行）。
func TestSplitEntries_ContinuationTopicPrefix(t *testing.T) {
	body := strings.Repeat("步骤说明。", 200) // 200 句 × 5 字 = 1000 字
	in := "## 部署流程\n" + body
	got := SplitEntries(in, 600)
	if len(got) < 2 {
		t.Fatalf("1007 字段落应切多条, got %d: %#v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "## 部署流程") {
		t.Errorf("首条应保留标题行: %q", got[0])
	}
	for i := 1; i < len(got); i++ {
		if !strings.HasPrefix(got[i], "（续）部署流程：") {
			t.Errorf("续片 %d 缺承接主题句: %q", i, got[i])
		}
	}
	checkSplitInvariants(t, "continuation-topic", in, got, 600)
}

// TestSplitEntries_NoDanglingTopicLine：条目末行不得悬空主题行（标题/结论句），
// 主题行顺延为下一条目首行。
func TestSplitEntries_NoDanglingTopicLine(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		mustKeep string
	}{
		{"悬空标题", rep('甲', 590) + "\n\n## 后续标题\n\n" + rep('乙', 590), "## 后续标题"},
		{"悬空结论句", rep('甲', 595) + "\n\n配置步骤如下：\n\n" + rep('乙', 595), "配置步骤如下："},
	}
	for _, c := range cases {
		got := SplitEntries(c.in, 600)
		for i, e := range got {
			lines := strings.Split(strings.TrimSpace(e), "\n")
			if len(lines) == 0 {
				continue
			}
			if isTopicLine(strings.TrimSpace(lines[len(lines)-1])) {
				t.Errorf("%s: entry %d 末行悬空主题行: %q", c.name, i, e)
			}
		}
		// 主题行必须仍在（内容无损）
		if !strings.Contains(strings.Join(got, "\n"), c.mustKeep) {
			t.Errorf("%s: 主题行丢失: %q", c.name, strings.Join(got, "\n"))
		}
		checkSplitInvariants(t, c.name, c.in, got, 600)
	}
}

// TestHardCut_SentenceBoundary：硬切优先落句界；无句界才等宽切。
func TestHardCut_SentenceBoundary(t *testing.T) {
	got := hardCut("一二三四五。六七八九十。", 8)
	if len(got) != 2 || got[0] != "一二三四五。" {
		t.Errorf("硬切应落句界, got %#v", got)
	}
	noBoundary := hardCut(rep('字', 20), 8)
	if !reflect.DeepEqual(noBoundary, []string{rep('字', 8), rep('字', 8), rep('字', 4)}) {
		t.Errorf("无句界应等宽硬切, got %#v", noBoundary)
	}
}

// —— G01：来源行注入（injectSourceLines / cleanEntriesWithSource）——

// TestInjectSourceLines：来源行必出且置于「适用：」行之前；已有来源行被规范值替换。
func TestInjectSourceLines(t *testing.T) {
	cases := []struct {
		name   string
		in     []string
		source string
		want   []string
	}{
		{
			name:   "无来源行追加",
			in:     []string{"## 步骤\n" + rep('甲', 30)},
			source: "文档A.md",
			want:   []string{"## 步骤\n" + rep('甲', 30) + "\n来源：文档A.md"},
		},
		{
			name:   "置于适用行之前",
			in:     []string{"## 步骤\n" + rep('甲', 30) + "\n适用：怎么推代码, git push"},
			source: "文档A.md",
			want:   []string{"## 步骤\n" + rep('甲', 30) + "\n来源：文档A.md\n适用：怎么推代码, git push"},
		},
		{
			name:   "模型写错来源行被替换",
			in:     []string{"## 步骤\n" + rep('甲', 30) + "\n来源：旧标题"},
			source: "文档A.md",
			want:   []string{"## 步骤\n" + rep('甲', 30) + "\n来源：文档A.md"},
		},
		{
			name:   "空来源名回落未命名文档",
			in:     []string{"## 步骤\n" + rep('甲', 30)},
			source: "",
			want:   []string{"## 步骤\n" + rep('甲', 30) + "\n来源：未命名文档"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := injectSourceLines(c.in, c.source, 600)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("injectSourceLines = %#v, want %#v", got, c.want)
			}
		})
	}
}

// TestCleanEntriesWithSource_AlwaysHasSourceLine：清洗管线 + 来源注入后每条目都带
// 来源行，且每条 ≤600。
func TestCleanEntriesWithSource_AlwaysHasSourceLine(t *testing.T) {
	in := "## 主题一\n" + rep('甲', 400) + "\n\n## 主题二\n" + rep('乙', 400)
	got := cleanEntriesWithSource(in, "k8s-deploy.md")
	if len(got) == 0 {
		t.Fatal("no entries")
	}
	for i, e := range got {
		if !strings.Contains(e, "来源：k8s-deploy.md") {
			t.Errorf("entry %d 缺来源行: %q", i, e)
		}
		if runeLen(e) > MaxEntryLen {
			t.Errorf("entry %d 超长 %d: %q", i, runeLen(e), e)
		}
	}
}

// TestCleanEntriesWithSource_EmptySourceKeepsLegacy：来源名为空时保持旧形态
// （供迁移侧按需调用），不注入来源行。
func TestCleanEntriesWithSource_EmptySourceKeepsLegacy(t *testing.T) {
	in := "## 主题一\n" + rep('甲', 30)
	got := cleanEntriesWithSource(in, "")
	if len(got) != 1 || strings.Contains(got[0], "来源：") {
		t.Errorf("空来源名不应注入来源行: %#v", got)
	}
}
