package wikisync

// 切段纯函数：边界（恰好/超限/单行硬切/多行累积/换行优先/空/不限长）、
// 无损性（去空白后与原文一致）、段名命名。

import (
	"strings"
	"testing"
	"unicode"
)

// stripSpace 去掉全部空白字符（含换行）。切段允许在段边界增删分隔换行，
// 故无损性以「去空白后相等」为判据。
func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func TestSplitDocChunks(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		maxRunes int
		want     []string
	}{
		{"恰好 maxRunes 一段", strings.Repeat("a", 5), 5, []string{"aaaaa"}},
		{"超一段一个 rune", strings.Repeat("a", 6), 5, []string{"aaaaa", "a"}},
		{"单行超长硬切", strings.Repeat("a", 12), 5, []string{"aaaaa", "aaaaa", "aa"}},
		{"多行累积填满一段", "aa\nbb\ncc", 5, []string{"aa\nbb", "cc"}},
		{"换行边界优先", "abc\ndef", 3, []string{"abc", "def"}},
		{"空正文", "", 10, nil},
		{"全空白正文", "  \n\t\n ", 10, nil},
		{"maxRunes<=0 整篇一段", "a\nb\nc", 0, []string{"a\nb\nc"}},
		{"maxRunes 负值整篇一段", "hello", -1, []string{"hello"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SplitDocChunks(c.text, c.maxRunes)
			if len(got) != len(c.want) {
				t.Fatalf("段数=%d want %d（got=%q）", len(got), len(c.want), got)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("第 %d 段=%q want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// 无损 + 每段不超限 + 无空白空段：覆盖多种形状。
func TestSplitDocChunksLossless(t *testing.T) {
	texts := []string{
		"短文本",
		"第一行\n第二行\n第三行",
		"行内容比较长的内容\n" + strings.Repeat("x", 25) + "\n尾",
		strings.Repeat("无换行的超长单行", 30),
		"a\n\n\nb\n",
		"leading\n\ntrailing\n\n",
	}
	for _, maxRunes := range []int{1, 3, 7, 16} {
		for _, text := range texts {
			got := SplitDocChunks(text, maxRunes)
			if len(got) == 0 {
				t.Fatalf("非空白正文不得返回空（text=%q）", text)
			}
			var joined strings.Builder
			for i, p := range got {
				if len([]rune(p)) > maxRunes {
					t.Fatalf("第 %d 段 %d rune 超限 %d（text=%q）", i, len([]rune(p)), maxRunes, text)
				}
				if p == "" {
					t.Fatalf("第 %d 段为空串（text=%q）", i, text)
				}
				joined.WriteString(p)
			}
			if stripSpace(joined.String()) != stripSpace(text) {
				t.Fatalf("无损性失败 maxRunes=%d：join=%q 去空白 != %q 去空白", maxRunes, joined.String(), text)
			}
		}
	}
}

func TestChunkName(t *testing.T) {
	cases := []struct {
		title string
		i, n  int
		want  string
	}{
		{"标题", 0, 1, "标题"},
		{"标题", 0, 0, "标题"}, // n<1 视为整篇一段
		{"标题", 0, 2, "标题（第1/2段）"},
		{"标题", 1, 3, "标题（第2/3段）"},
		{"华南理工", 9, 10, "华南理工（第10/10段）"},
	}
	for _, c := range cases {
		if got := ChunkName(c.title, c.i, c.n); got != c.want {
			t.Errorf("ChunkName(%q,%d,%d)=%q want %q", c.title, c.i, c.n, got, c.want)
		}
	}
}
