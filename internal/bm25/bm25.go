// Package bm25：混合检索的稀疏向量编码（qdrant 命名稀疏向量 `bm25`）。
//
// 设计（2026-09-08 混合检索批，任务拍板）：
//   - 分词无 jieba 依赖：CJK 字符滑窗 2-gram + ASCII 字母数字连续段整词。
//     "怎么推代码" → ["怎么","么推","推代","代码"]；"gitlab" → ["gitlab"]。
//     2-gram 的词法召回覆盖口语查询的关键字面（dense 向量会把短查询的词法
//     匹配压下去，这正是混合检索要补的盲区）。
//   - index = term 的 FNV-1a 32 位哈希；value = BM25 的 TF 饱和变换
//     tf*(k1+1)/(tf+k1)（k1=1.2，tf=1 → 1.0，渐近上界 k1+1=2.2）。
//   - 取舍（v1）：完整 BM25 的 IDF 需全库文档频率统计。qdrant 服务端对稀疏
//     向量默认打分是点积（Σ qtf·dtf），无 IDF；qdrant 支持 modifier:"idf"
//     在服务端按 collection 统计算 IDF（v2 备选——会使分数随语料漂移，且
//     需要 collection 配置变更）。v1 用 TF 加权 + 服务端点积：短查询每词元
//     qtf≈1，点积≈「命中词元的文档 TF 饱和和」，区分性已够用。此取舍由
//     任务拍板（TF×1.2 归一），本文档注释即规格。
//   - 哈希碰撞：不同 term 撞同一 index 时按 index 聚合求和（查询与文档两
//     侧同函数同碰撞，点积语义等价于合并词元）。2^32 碰撞域 + 千级词表，
//     生日碰撞期望 ~1e-5/词，可接受；碰撞只损失一点区分度，不产生错误。
package bm25

import (
	"sort"
	"unicode"
	"unicode/utf8"
)

// K1 BM25 TF 饱和参数（标准值 1.2；value(tf) = tf*(K1+1)/(tf+K1)）。
const K1 = 1.2

// Terms 文本分词（编码与查询共用，词元按出现顺序返回）：
//   - CJK（汉字/假名/谚文）连续 run：相邻两字滑窗 2-gram；run 长度为 1 时
//     该单字自成词元（无相邻字可组 2-gram）；
//   - ASCII 字母/数字连续段：整段小写作一个词元（"GitLab" → "gitlab"）；
//   - 其余 rune（空白/标点/emoji 等）只作分隔，不产词元。
func Terms(text string) []string {
	var terms []string
	var cjk []rune // 当前连续 CJK run
	var ascii []byte
	flushCJK := func() {
		for i := 0; i+1 < len(cjk); i += 1 {
			terms = append(terms, string(cjk[i:i+2]))
		}
		if len(cjk) == 1 {
			terms = append(terms, string(cjk))
		}
		cjk = cjk[:0]
	}
	flushASCII := func() {
		if len(ascii) > 0 {
			terms = append(terms, string(ascii))
			ascii = ascii[:0]
		}
	}
	for _, r := range text {
		switch {
		case isCJK(r):
			flushASCII()
			cjk = append(cjk, r)
		case r < utf8.RuneSelf && isASCIITerm(r):
			flushCJK()
			if 'A' <= r && r <= 'Z' {
				r += 'a' - 'A'
			}
			ascii = append(ascii, byte(r))
		default:
			flushCJK()
			flushASCII()
		}
	}
	flushCJK()
	flushASCII()
	return terms
}

// SparseVector 把文本编码为 qdrant 稀疏向量：indices（term 哈希，升序唯一）
// 与 values（TF 饱和权重，同序等长）。查询端与文档端同函数（qdrant 点积
// 打分要求两侧同一词元空间）。
//
// 空文本 / 无词元（纯标点空白）→ 空切片（调用方按「无稀疏分支」处理）。
func SparseVector(text string) (indices []uint32, values []float32) {
	tf := make(map[string]int)
	for _, t := range Terms(text) {
		tf[t]++
	}
	// index → 聚合权重（哈希碰撞的不同 term 求和，见包注释）。
	agg := make(map[uint32]float32, len(tf))
	for t, n := range tf {
		idx := fnv1a32(t)
		agg[idx] += tfSaturation(n)
	}
	indices = make([]uint32, 0, len(agg))
	for idx := range agg {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	values = make([]float32, len(indices))
	for i, idx := range indices {
		values[i] = agg[idx]
	}
	return indices, values
}

// tfSaturation BM25 TF 项：tf*(k1+1)/(tf+k1)。无长度归一（b=0——服务端无
// 全库平均长度统计），仅保留 TF 饱和，防止长文档重复词元线性支配点积。
func tfSaturation(tf int) float32 {
	f := float64(tf)
	return float32(f * (K1 + 1) / (f + K1))
}

// isCJK 参与中文 2-gram 的表意文字范围（汉字/日文假名/韩文谚文；覆盖中文
// 语料主场景，标点/全角符号一律作分隔）。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// isASCIITerm ASCII 词元字符：字母与数字（'_'、'-'、'.' 等作分隔——
// "pub/agmem:v0.3" → ["pub","agmem","v0","3"，与 CLI 查询习惯一致）。
func isASCIITerm(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'
}

// fnv1a32 FNV-1a 32 位哈希（offset 2166136261 / prime 16777619）。
// 确定性跨进程稳定（无随机种子），qdrant 稀疏 index 必须文档/查询两侧一致。
func fnv1a32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
