package qdrant

// ScrollPointIDs 用例：分页翻页、filter/wire 形状、点 id 归一、错误传播。
// 全部用 httptest 替身，不依赖真实 qdrant。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestScrollPointIDsPaginationAndFilter(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/collections/sre/points/scroll" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			_, _ = w.Write([]byte(`{"result":{"points":[{"id":"p-orig"},{"id":"p-clean0"}],"next_page_offset":"off-1"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":{"points":[{"id":"p-clean1"}],"next_page_offset":null}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", time.Second)
	ids, err := c.ScrollPointIDs(context.Background(), "sre", SearchRequest{ItemID: 42})
	if err != nil {
		t.Fatalf("ScrollPointIDs: %v", err)
	}
	if got := strings.Join(ids, ","); got != "p-orig,p-clean0,p-clean1" {
		t.Fatalf("ids = %q", got)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 scroll calls (pagination), got %d", len(bodies))
	}

	// 第一页：wire 形状 + item_id filter，且不带 offset。
	first := bodies[0]
	if first["limit"] != float64(256) || first["with_payload"] != false || first["with_vector"] != false {
		t.Fatalf("scroll body shape wrong: %v", first)
	}
	if _, ok := first["offset"]; ok {
		t.Fatalf("first page must not carry offset: %v", first)
	}
	must, ok := first["filter"].(map[string]any)["must"].([]any)
	if !ok || len(must) != 1 {
		t.Fatalf("filter.must = %v", first["filter"])
	}
	m := must[0].(map[string]any)
	if m["key"] != "item_id" {
		t.Fatalf("filter key = %v, want item_id", m["key"])
	}
	if m["match"].(map[string]any)["value"] != float64(42) {
		t.Fatalf("filter match.value = %v, want 42", m["match"])
	}

	// 第二页：offset 原样回传上一页 next_page_offset。
	if bodies[1]["offset"] != "off-1" {
		t.Fatalf("second page offset = %v, want off-1", bodies[1]["offset"])
	}
}

func TestScrollPointIDsNumericIDAndEmpty(t *testing.T) {
	t.Run("数字id归一为字符串", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"result":{"points":[{"id":7}],"next_page_offset":null}}`))
		}))
		defer srv.Close()
		ids, err := New(srv.URL, "", time.Second).ScrollPointIDs(context.Background(), "sre", SearchRequest{ItemID: 7})
		if err != nil {
			t.Fatalf("ScrollPointIDs: %v", err)
		}
		if len(ids) != 1 || ids[0] != "7" {
			t.Fatalf("ids = %v, want [7]", ids)
		}
	})
	t.Run("空结果无错误", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"result":{"points":[],"next_page_offset":null}}`))
		}))
		defer srv.Close()
		ids, err := New(srv.URL, "", time.Second).ScrollPointIDs(context.Background(), "sre", SearchRequest{ItemID: 99})
		if err != nil {
			t.Fatalf("ScrollPointIDs: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("ids = %v, want empty", ids)
		}
	})
}

func TestScrollPointIDsErrors(t *testing.T) {
	t.Run("非2xx上抛status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":{"error":"boom"}}`))
		}))
		defer srv.Close()
		_, err := New(srv.URL, "", time.Second).ScrollPointIDs(context.Background(), "sre", SearchRequest{ItemID: 1})
		if err == nil || !strings.Contains(err.Error(), "status 500") {
			t.Fatalf("want status error, got %v", err)
		}
	})
	t.Run("响应解码失败", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"result":"not-an-object"}`))
		}))
		defer srv.Close()
		_, err := New(srv.URL, "", time.Second).ScrollPointIDs(context.Background(), "sre", SearchRequest{ItemID: 1})
		if err == nil || !strings.Contains(err.Error(), "decode scroll") {
			t.Fatalf("want decode error, got %v", err)
		}
	})
	t.Run("传输失败", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"result":{"points":[],"next_page_offset":null}}`))
		}))
		url := srv.URL
		srv.Close() // 关闭后连接失败
		_, err := New(url, "", time.Second).ScrollPointIDs(context.Background(), "sre", SearchRequest{ItemID: 1})
		if err == nil {
			t.Fatal("want transport error, got nil")
		}
	})
}
