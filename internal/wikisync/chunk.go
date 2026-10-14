// chunk.go：wiki 正文切段（纯函数，便于单测）。
//
// 为什么切段：清洗模型（中档思考）面对长输入会撞网关 504——约 8000 字符长文
// 必现、4000 稳定。本包按同一口径切段，保证入库文本粒度与来源命名一致。
package wikisync

import (
	"fmt"
	"strings"
)

// DocChunkRunes 单段最大 rune 数（按 rune 计，不是字节）。
//
// 4000 的依据：长输入（约 8000 字符）在网关端易触发 504，4000 稳定。调大此值
// 前须先复测所用网关的长输入上限，避免整篇文档在 embed 阶段失败。
const DocChunkRunes = 4000

// SplitDocChunks 把正文切成每段不超过 maxRunes 个 rune 的片段（内容保序、不丢字符）。
//
// 规则：
//   - 优先在换行边界断开：贪心累积整行，尽量把一段填满（多行可落在同一段）；
//   - 单行本身超过 maxRunes 时按 rune 硬切（不中途丢字）；
//   - 不丢内容：除段边界处的分隔换行外，所有字符按原序保留。即满足
//     stripSpace(strings.Join(parts, "\n")) == stripSpace(text)；跨段 join 时
//     段边界可能多一个分隔换行（硬切处），故无损性以「去空白后相等」为判据；
//   - 空 / 全空白正文返回 nil（无内容可入队）；
//   - maxRunes <= 0 视为不限长，整篇作为一段返回（空 / 全空白仍返回 nil）。
func SplitDocChunks(text string, maxRunes int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if maxRunes <= 0 {
		return []string{text}
	}

	var (
		out []string
		b   strings.Builder
		cur int // b 中已写入的 rune 数
	)
	flush := func() {
		if cur > 0 {
			out = append(out, b.String())
			b.Reset()
			cur = 0
		}
	}

	for li, line := range strings.Split(text, "\n") {
		if li > 0 {
			// 与上一行之间的换行：当前段已满则在此断开（分隔换行落到段边界上）；
			// 否则在当前段内原样补回一个换行。
			if cur >= maxRunes {
				flush()
			}
			if cur > 0 {
				b.WriteByte('\n')
				cur++
			}
		}
		for _, r := range line {
			if cur >= maxRunes {
				flush()
			}
			b.WriteRune(r)
			cur++
		}
	}
	flush()
	return out
}

// ChunkName 段名：整篇一段（n<=1）沿用原标题；多段加「（第i+1/n段）」后缀（i 为 0 基）。
//
// 命名与早期导出脚本一致（`${base}（第${seg}/${nch}段）`），使「来源：」行在两套
// 入库路径下可对齐，便于核对与检索。
func ChunkName(title string, i, n int) string {
	if n <= 1 {
		return title
	}
	return fmt.Sprintf("%s（第%d/%d段）", title, i+1, n)
}
