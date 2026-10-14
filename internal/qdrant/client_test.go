package qdrant

// 全请求带 api-key header（secret vector-db/qdrant-api-key）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllRequestsCarryAPIKey(t *testing.T) {
	var paths []string
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		keys = append(keys, r.Header.Get("api-key"))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"result":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "secret-key", 5*time.Second)
	ctx := context.Background()
	if err := c.EnsureCollection(ctx, "kb1", 1024); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	if err := c.Upsert(ctx, "kb1", []Point{{ID: "p1", Vector: []float32{1}, Payload: PointPayload{}}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := c.Search(ctx, "kb1", SearchRequest{Vector: []float32{1}, Limit: 1}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	// EnsureCollection 自混合检索批（2026-09-08）起先 GET 探测再补稀疏配置，
	// 故 4 个请求：GET 探测 + PUT vectors/bm25 + upsert + search。
	if len(paths) != 4 {
		t.Fatalf("want 4 requests, got %d: %v", len(paths), paths)
	}
	for i, k := range keys {
		if k != "secret-key" {
			t.Errorf("request %d (%s): api-key=%q", i, paths[i], k)
		}
	}
}

func TestNoAPIKeyOmitsHeader(t *testing.T) {
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("api-key") != "" {
			t.Errorf("api-key should be omitted, got %q", r.Header.Get("api-key"))
		}
		seen.Add(1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"result":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	if err := c.EnsureCollection(context.Background(), "kb", 4); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	// GET 探测 + PUT vectors/bm25（fake 返回 200 = 存量 collection，走增补路径）
	if seen.Load() != 2 {
		t.Fatalf("want 2 requests, got %d", seen.Load())
	}
}

func TestSearchFilterEncoding(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body = string(buf[:n])
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"result":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	_, err := c.Search(context.Background(), "kb", SearchRequest{
		Vector: []float32{1}, Limit: 5, Dir: "mysql", PointType: "clean",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !contains(body, `"dir"`) || !contains(body, `"mysql"`) {
		t.Fatalf("dir filter missing: %s", body)
	}
	if !contains(body, `"point_type"`) || !contains(body, `"clean"`) {
		t.Fatalf("point_type filter missing: %s", body)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
