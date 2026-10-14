package localembed

import (
	"strings"
	"testing"
)

const testVocab = "[PAD]\n[UNK]\n[CLS]\n[SEP]\n!\n##k\n##s\nb\nbin\nbinlog\n日\n志\n节\n点\n污\nk8s\n"

func mustTokenizer(t *testing.T) *Tokenizer {
	t.Helper()
	tok, err := NewTokenizer(testVocab)
	if err != nil {
		t.Fatalf("NewTokenizer: %v", err)
	}
	return tok
}

func TestTokenizer_SpecialTokens(t *testing.T) {
	tok := mustTokenizer(t)
	if tok.vocab["[CLS]"] != 2 || tok.vocab["[SEP]"] != 3 || tok.vocab["[UNK]"] != 1 {
		t.Fatalf("special token ids wrong: %v", tok.vocab)
	}
	if tok.Size() == 0 {
		t.Fatal("empty vocab")
	}
}

func TestTokenizer_MissingSpecialToken(t *testing.T) {
	if _, err := NewTokenizer("hello\nworld\n"); err == nil {
		t.Fatal("want error for missing [CLS]")
	}
}

func TestTokenizer_BasicWordPiece(t *testing.T) {
	tok := mustTokenizer(t)
	in := tok.Encode("binlog 日志")
	// [CLS] bin##log → bin + ##log? vocab 有 bin,binlog → 贪婪取 binlog
	got := strings.Fields(tok.Decode(in.IDs))
	if got[0] != "[CLS]" || got[len(got)-1] != "[SEP]" {
		t.Fatalf("want CLS/SEP wrap, got %v", got)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "binlog") {
		t.Fatalf("want greedy binlog, got %q", joined)
	}
	if !strings.Contains(joined, "日") || !strings.Contains(joined, "志") {
		t.Fatalf("CJK must split per char, got %q", joined)
	}
	for _, id := range in.IDs {
		if id < 0 {
			t.Fatalf("negative id: %d", id)
		}
	}
}

func TestTokenizer_Lowercase(t *testing.T) {
	tok := mustTokenizer(t)
	in := tok.Encode("BINLOG")
	if !strings.Contains(tok.Decode(in.IDs), "binlog") {
		t.Fatalf("want lowercase, got %q", tok.Decode(in.IDs))
	}
}

func TestTokenizer_UnknownFallsToUNK(t *testing.T) {
	tok := mustTokenizer(t)
	in := tok.Encode("zzzz")
	if !strings.Contains(tok.Decode(in.IDs), "[UNK]") {
		t.Fatalf("want [UNK], got %q", tok.Decode(in.IDs))
	}
}

func TestTokenizer_PaddingAndMask(t *testing.T) {
	tok := mustTokenizer(t)
	in := tok.Encode("b k")
	if len(in.IDs) != tok.maxLen {
		t.Fatalf("want padded to maxLen %d, got %d", tok.maxLen, len(in.IDs))
	}
	ones := 0
	for _, m := range in.AttentionMask {
		if m == 1 {
			ones++
		}
	}
	// [CLS] b ##k? 'b'→b, 'k'→##k? k 无独立词条→##k 可以匹配续段? "k" 首段无 "k" 词条 → [UNK]
	// b 与 k 两 word + CLS/SEP = 4
	if ones != 4 {
		t.Fatalf("want 4 masked tokens, got %d (decode=%q)", ones, tok.Decode(in.IDs))
	}
	for i, id := range in.IDs[ones:] {
		if id != 0 {
			t.Fatalf("padding should be 0, pos %d = %d", ones+i, id)
		}
	}
}

func TestTokenizer_TruncateToMaxLen(t *testing.T) {
	tok := mustTokenizer(t)
	long := strings.Repeat("节", 2000)
	in := tok.Encode(long)
	if len(in.IDs) != tok.maxLen {
		t.Fatalf("want %d, got %d", tok.maxLen, len(in.IDs))
	}
	if got := tok.Decode(in.IDs); !strings.HasSuffix(got, "[SEP]") {
		t.Fatalf("truncated seq must end with [SEP], got %q", got[len(got)-20:])
	}
}
