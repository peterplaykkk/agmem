package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func fakeEmbedServer(t *testing.T, status int, body string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"401"}}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func okBody(n int) string {
	var parts []string
	for i := 0; i < n; i++ {
		var dims []string
		for j := 0; j < Dim; j++ {
			dims = append(dims, "0.1")
		}
		parts = append(parts, `{"index":`+itoa(i)+`,"embedding":[`+strings.Join(dims, ",")+`]}`)
	}
	return `{"data":[` + strings.Join(parts, ",") + `]}`
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestEmbedBatch_Success(t *testing.T) {
	calls := 0
	srv := fakeEmbedServer(t, 200, okBody(2), &calls)
	defer srv.Close()
	c := NewQianfan(srv.URL, "test-key", "m", 16, 0)
	vecs, err := c.EmbedBatch(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) != Dim {
		t.Fatalf("want 2x%d, got %dx%d", Dim, len(vecs), len(vecs[0]))
	}
}

func TestEmbedBatch_TooLarge(t *testing.T) {
	c := NewQianfan("http://unused", "k", "m", 2, 0)
	if _, err := c.EmbedBatch(context.Background(), []string{"a", "b", "c"}); err == nil {
		t.Fatal("want error for oversize batch")
	}
}

func TestEmbedBatch_RateLimitedRetries(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(okBody(1)))
	}))
	defer srv.Close()
	c := NewQianfan(srv.URL, "test-key", "m", 16, 3)
	if _, err := c.EmbedBatch(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("want success after retry: %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

func TestEmbedBatch_UnauthorizedNoRetry(t *testing.T) {
	calls := 0
	srv := fakeEmbedServer(t, 401, `{"error":{"code":"401"}}`, &calls)
	defer srv.Close()
	c := NewQianfan(srv.URL, "bad-key", "m", 16, 3)
	_, err := c.EmbedBatch(context.Background(), []string{"a"})
	if !IsUnauthorized(err) {
		t.Fatalf("want unauthorized, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("401 must not retry, calls=%d", calls)
	}
}

func TestEmbedBatch_Exhausted(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := NewQianfan(srv.URL, "test-key", "m", 16, 2)
	_, err := c.EmbedBatch(context.Background(), []string{"a"})
	if !IsRateLimited(err) {
		t.Fatalf("want rate-limited error, got %v", err)
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("want *HTTPError, got %T", err)
	}
	if calls != 3 { // 1 + 2 retries
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

func TestEmbed_EmptyInput(t *testing.T) {
	c := NewQianfan("http://unused", "k", "m", 16, 1)
	vecs, err := c.Embed(context.Background(), nil)
	if err != nil || len(vecs) != 0 {
		t.Fatalf("want empty,nil, got %v,%v", vecs, err)
	}
}

func TestEmbed_MultiBatchOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(okBody(1)))
	}))
	defer srv.Close()
	c := NewQianfan(srv.URL, "test-key", "m", 1, 0) // 每批 1 条，强制多批
	vecs, err := c.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("want 3 vectors, got %d", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != Dim {
			t.Fatalf("vec[%d] dim=%d", i, len(v))
		}
	}
}

// captureInputs 记录每次 /embeddings 请求的 input 文本并回 1024d 固定向量。
func captureInputs(t *testing.T, got *[][]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		*got = append(*got, req.Input)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(okBody(len(req.Input))))
	}))
}

// EmbedQuery 查询侧：QueryInstruction 非空 → 每个文本前置指令前缀（非对称检索）。
func TestEmbedQuery_PrefixesInstruction(t *testing.T) {
	var got [][]string
	srv := captureInputs(t, &got)
	defer srv.Close()
	c := NewQianfan(srv.URL, "test-key", "m", 16, 0)
	c.QueryInstruction = "Instruct: X\nQuery: "
	if _, err := c.EmbedQuery(context.Background(), []string{"怎么推代码", "b"}); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("want 1 batch of 2, got %v", got)
	}
	if got[0][0] != "Instruct: X\nQuery: 怎么推代码" || got[0][1] != "Instruct: X\nQuery: b" {
		t.Fatalf("query texts must be instruction-prefixed, got %q", got[0])
	}
}

// EmbedQuery 空指令 = 禁用 → 直通 Embed（不加任何前缀）。
func TestEmbedQuery_EmptyInstructionPassthrough(t *testing.T) {
	var got [][]string
	srv := captureInputs(t, &got)
	defer srv.Close()
	c := NewQianfan(srv.URL, "test-key", "m", 16, 0)
	c.QueryInstruction = ""
	if _, err := c.EmbedQuery(context.Background(), []string{"怎么推代码"}); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != "怎么推代码" {
		t.Fatalf("empty instruction must pass through unprefixed, got %v", got)
	}
}

// Embed 永不读 QueryInstruction：写入/ingest 路径（worker 用 Embed）不受指令影响，
// 与查询侧非对称——这是存量 2577 向量无需重索引的代码保证。
func TestEmbed_IgnoresQueryInstruction(t *testing.T) {
	var got [][]string
	srv := captureInputs(t, &got)
	defer srv.Close()
	c := NewQianfan(srv.URL, "test-key", "m", 16, 0)
	c.QueryInstruction = "Instruct: X\nQuery: "
	if _, err := c.Embed(context.Background(), []string{"文档正文"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != "文档正文" {
		t.Fatalf("Embed must not apply QueryInstruction, got %v", got)
	}
}
