package localembed

// Minimal BERT WordPiece tokenizer（纯 Go，替代 HF tokenizers cgo）。
// 适用 BAAI/bge-base-zh（vocab.txt，BERT WordPiece）。
// 覆盖：BasicTokenizer（小写、CJK 切分、按非字母数字切分）+ WordPiece 贪婪匹配
// + [CLS]/[SEP] + attention mask + 512 截断。不做 Wordpiece 全角归一化/带口音剥离
// （中文场景影响可忽略，待评审）。

import (
	"fmt"
	"strings"
	"unicode"
)

// TokenizerInput 一次编码结果。
type TokenizerInput struct {
	IDs           []int64 // token id 序列，含 [CLS]/[SEP]
	AttentionMask []int64 // 1=有效，0=padding
	TokenTypeIDs  []int64 // 全 0（单句）
	MaxLen        int
}

// Tokenizer BERT WordPiece。
type Tokenizer struct {
	vocab    map[string]int
	idToTok  map[int]string
	unkID    int
	clsID    int
	sepID    int
	padID    int
	maxChars int // 单 wordpiece 最大字符数（BERT 默认 100）
	maxLen   int // 序列长度（bge 默认 512）
}

// NewTokenizer 用 vocab.txt 内容构建（每行一个 token，行号即 id）。
func NewTokenizer(vocabText string) (*Tokenizer, error) {
	vocab := map[string]int{}
	idToTok := map[int]string{}
	for i, line := range strings.Split(vocabText, "\n") {
		tok := strings.TrimRight(line, "\r")
		if tok == "" {
			continue
		}
		if _, dup := vocab[tok]; dup {
			continue
		}
		vocab[tok] = i
		idToTok[i] = tok
	}
	get := func(tok string) (int, error) {
		id, ok := vocab[tok]
		if !ok {
			return 0, fmt.Errorf("vocab 缺少特殊 token %q", tok)
		}
		return id, nil
	}
	unk, err := get("[UNK]")
	if err != nil {
		return nil, err
	}
	cls, err := get("[CLS]")
	if err != nil {
		return nil, err
	}
	sep, err := get("[SEP]")
	if err != nil {
		return nil, err
	}
	pad, err := get("[PAD]")
	if err != nil {
		return nil, err
	}
	return &Tokenizer{
		vocab: vocab, idToTok: idToTok,
		unkID: unk, clsID: cls, sepID: sep, padID: pad,
		maxChars: 100, maxLen: 512,
	}, nil
}

// Size vocab 词条数。
func (t *Tokenizer) Size() int { return len(t.vocab) }

// basicTokens 小写、CJK 单字切分、按非字母数字切分。
func basicTokens(text string) []string {
	text = strings.ToLower(text)
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range text {
		switch {
		case isCJK(r):
			flush()
			out = append(out, string(r))
		case unicode.IsLetter(r) && r < 128 || unicode.IsDigit(r):
			cur.WriteRune(r)
		case unicode.IsLetter(r): // 非 ASCII 字母（如带音标）保留
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0xF900 && r <= 0xFAFF) || (r >= 0x3040 && r <= 0x30FF)
}

// wordPiece 单 word 贪婪最长匹配，失败返回 nil（调用方落 [UNK]）。
func (t *Tokenizer) wordPiece(word string) []string {
	if len(word) > t.maxChars {
		return nil
	}
	var out []string
	rest := word
	first := true
	for rest != "" {
		// 从最长前缀往下试
		rs := []rune(rest)
		end := len(rs)
		for end > 0 {
			cand := string(rs[:end])
			if !first {
				cand = "##" + cand
			}
			if _, ok := t.vocab[cand]; ok {
				out = append(out, cand)
				rest = string(rs[end:])
				first = false
				break
			}
			end--
		}
		if end == 0 {
			return nil // 无法匹配 → [UNK]
		}
	}
	return out
}

// Encode 编码单句（含 [CLS]/[SEP]，超长截断）。
func (t *Tokenizer) Encode(text string) TokenizerInput {
	var toks []string
	toks = append(toks, "[CLS]")
	for _, w := range basicTokens(text) {
		if pieces := t.wordPiece(w); pieces != nil {
			toks = append(toks, pieces...)
		} else {
			toks = append(toks, "[UNK]")
		}
	}
	toks = append(toks, "[SEP]")
	if len(toks) > t.maxLen {
		toks = toks[:t.maxLen]
		toks[t.maxLen-1] = "[SEP]"
	}
	in := TokenizerInput{
		IDs:           make([]int64, t.maxLen),
		AttentionMask: make([]int64, t.maxLen),
		TokenTypeIDs:  make([]int64, t.maxLen),
		MaxLen:        t.maxLen,
	}
	for i, tok := range toks {
		in.IDs[i] = int64(t.vocab[tok])
		in.AttentionMask[i] = 1
	}
	// 其余位置已是 0（padID）
	return in
}

// Decode 还原 token 文本（调试用）。
func (t *Tokenizer) Decode(ids []int64) string {
	var parts []string
	for _, id := range ids {
		if tok, ok := t.idToTok[int(id)]; ok && tok != "[PAD]" {
			parts = append(parts, tok)
		}
	}
	return strings.Join(parts, " ")
}
