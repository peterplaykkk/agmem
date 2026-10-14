package api

import (
	"strings"
	"testing"
)

// textDefaultName：texts[] 没有文件名时的派生名称。
//
// 该值会成为 clean 条目的「来源：」行与 payload.source_doc，所以断言的是
// 「可核实」而不是「好看」：必须来自内容而不是数组下标。
func TestTextDefaultName(t *testing.T) {
	cases := []struct {
		name string
		text string
		i    int
		want string
	}{
		{"markdown 标题去井号", "# 采集节点接入验收步骤\n\n正文…", 0, "采集节点接入验收步骤"},
		{"无标题取首行", "统一出口代理走 proxy.example.com:3128\n更多…", 0, "统一出口代理走 proxy.example.com:3128"},
		{"跳过前导空行与空白行", "\n\n   \n真正的首行\n第二行", 0, "真正的首行"},
		{"前导空格被裁剪", "   缩进的首行   \n", 0, "缩进的首行"},
		{"超长截断到 60 字", "# " + strings.Repeat("字", 80), 0, strings.Repeat("字", 60) + "…"},
		{"恰好 60 字不截断", strings.Repeat("字", 60), 0, strings.Repeat("字", 60)},
		{"全空白回退下标名", "   \n\n \t ", 2, "未命名文本[2]"},
		{"空串回退下标名", "", 0, "未命名文本[0]"},
		{"多级标题只剥前缀井号", "### 三级标题", 0, "三级标题"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := textDefaultName(c.text, c.i); got != c.want {
				t.Fatalf("textDefaultName(%q, %d) = %q, want %q", c.text, c.i, got, c.want)
			}
		})
	}

	// 旧行为回归护栏：数组下标绝不能出现在有内容文本的默认名里。
	if got := textDefaultName("有内容的文本", 0); strings.Contains(got, "texts[") {
		t.Fatalf("默认名不得退化为数组下标: %q", got)
	}
}
