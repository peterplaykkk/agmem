package qdrant

// SearchMulti（--global 无 --kb 跨 kb fan-out）行为测试：
// 合并排序、全局 top-k 截断、来源标注（payload.kb 回填 collection）、
// 失败传播（不静默半结果）、并发下发与 per-collection limit。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockQdrant 按 collection 名返回预置 hits；记录收到的请求（path + body.limit）。
type mockQdrant struct {
	mu     sync.Mutex
	hits   map[string]SearchHit // collection → 单条样例 hit
	misses map[string]bool      // collection → 返回 500
	paths  []string
	limits []int
}

func (m *mockQdrant) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.paths = append(m.paths, r.URL.Path)
		var body struct {
			Limit int `json:"limit"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.limits = append(m.limits, body.Limit)
		col := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/collections/"), "/points/search")
		if m.misses[col] {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"err":"boom"}`))
			return
		}
		h, ok := m.hits[col]
		if !ok {
			_, _ = w.Write([]byte(`{"result":[]}`))
			return
		}
		b, _ := json.Marshal(map[string]any{"result": []map[string]any{{
			"id":      h.ID,
			"score":   h.Score,
			"payload": h.Payload,
		}}})
		_, _ = w.Write(b)
	})
}

func TestSearchMulti_MergeSortAndSourceAnnotation(t *testing.T) {
	m := &mockQdrant{hits: map[string]SearchHit{
		"kb-a": {ID: "a1", Score: 0.91, Payload: PointPayload{Text: "A"}},             // payload.kb 缺 → 标注 kb-a
		"kb-b": {ID: "b1", Score: 0.95, Payload: PointPayload{KB: "kb-b", Text: "B"}}, // worker 已写 → 保留
		"kb-c": {ID: "c1", Score: 0.55, Payload: PointPayload{Text: "C"}},
		"kb-d": {ID: "d1", Score: 0.88, Payload: PointPayload{Text: "D"}},
	}}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	c := New(srv.URL, "", time.Second)

	got, err := c.SearchMulti(context.Background(),
		[]string{"kb-a", "kb-b", "kb-c", "kb-d"},
		SearchRequest{Vector: []float32{1}, Limit: 3, PointType: "clean"}, 4)
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	// 排序：0.95 > 0.91 > 0.88（0.55 被全局 top-k=3 截断）
	if len(got) != 3 {
		t.Fatalf("want 3 hits after global top-k, got %d: %+v", len(got), got)
	}
	wantOrder := []string{"b1", "a1", "d1"}
	for i, id := range wantOrder {
		if got[i].ID != id {
			t.Fatalf("rank %d: want %s got %s (%+v)", i, id, got[i].ID, got)
		}
	}
	// 来源标注：缺省回填 collection 名，已写不覆盖
	if got[0].Payload.KB != "kb-b" || got[1].Payload.KB != "kb-a" {
		t.Fatalf("source annotation wrong: %q %q", got[0].Payload.KB, got[1].Payload.KB)
	}
	// 每 collection 恰好一次请求
	if len(m.paths) != 4 {
		t.Fatalf("want 4 fan-out requests, got %d: %v", len(m.paths), m.paths)
	}
	for i, l := range m.limits {
		if l != 3 {
			t.Errorf("per-collection limit request %d: want 3 got %d", i, l)
		}
	}
}

func TestSearchMulti_EmptyCollections(t *testing.T) {
	m := &mockQdrant{hits: map[string]SearchHit{}}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	got, err := c.SearchMulti(context.Background(), nil, SearchRequest{Vector: []float32{1}}, 4)
	if err != nil {
		t.Fatalf("empty collections must not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty hits, got %+v", got)
	}
	if len(m.paths) != 0 {
		t.Fatalf("no HTTP call expected, got %v", m.paths)
	}
}

func TestSearchMulti_FailFastPropagates(t *testing.T) {
	m := &mockQdrant{
		hits:   map[string]SearchHit{"kb-ok": {ID: "ok", Score: 0.9}},
		misses: map[string]bool{"kb-bad": true},
	}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	got, err := c.SearchMulti(context.Background(),
		[]string{"kb-ok", "kb-bad"}, SearchRequest{Vector: []float32{1}, Limit: 2}, 2)
	if err == nil {
		t.Fatalf("collection failure must fail whole fan-out, got %+v", got)
	}
	// 错误须指明来源 collection（可诊断，不静默降级半结果）
	if !strings.Contains(err.Error(), "kb-bad") {
		t.Fatalf("error must name failing collection: %v", err)
	}
}

func TestSearchMulti_DefaultLimit(t *testing.T) {
	// Limit<=0 → per-collection 5（与 Search 缺省一致）；返回 7 条合并后仍截到 5
	m := &mockQdrant{hits: map[string]SearchHit{}}
	for i := 0; i < 7; i++ {
		m.hits[fmt.Sprintf("col-%d", i)] = SearchHit{ID: fmt.Sprintf("p%d", i), Score: float64(i) / 10}
	}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	cols := make([]string, 7)
	for i := range cols {
		cols[i] = fmt.Sprintf("col-%d", i)
	}
	got, err := c.SearchMulti(context.Background(), cols, SearchRequest{Vector: []float32{1}}, 4)
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	if len(m.limits) == 0 {
		t.Fatal("no requests recorded")
	}
	for i, l := range m.limits {
		if l != 5 {
			t.Errorf("default limit request %d: want 5 got %d", i, l)
		}
	}
	if len(got) != 5 {
		t.Fatalf("global top-k default: want 5, got %d", len(got))
	}
	if got[0].ID != "p6" || got[4].ID != "p2" {
		t.Fatalf("merge order wrong: %+v", got)
	}
}
