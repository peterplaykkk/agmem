package embedding

// 主通道 aigateway → fallback 千帆直连 链路测试。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func aigwServer(t *testing.T, status int, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}
		if r.URL.Path != "/embeddings" {
			t.Errorf("aigateway path = %s, want /embeddings", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer gw-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			t.Errorf("content-type missing")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(okBody(2)))
	}))
}

func TestAIGatewayPrimary_Success(t *testing.T) {
	var gwCalls, qfCalls int32
	gw := aigwServer(t, 200, &gwCalls)
	defer gw.Close()
	qf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qfCalls++
		w.WriteHeader(500)
	}))
	defer qf.Close()
	c := New(gw.URL, "gw-token", qf.URL, "qf-key", "m", 16, 2)
	vecs, err := c.EmbedBatch(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("want 2 vecs, got %d", len(vecs))
	}
	if gwCalls != 1 || qfCalls != 0 {
		t.Fatalf("primary should be used: gw=%d qf=%d", gwCalls, qfCalls)
	}
}

func TestFallbackOnPrimaryFailure(t *testing.T) {
	var gwCalls, qfCalls int32
	gw := aigwServer(t, 429, &gwCalls) // 主通道 429
	defer gw.Close()
	qf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qfCalls++
		if r.Header.Get("Authorization") != "Bearer qf-key" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path != "/v2/embeddings" {
			t.Errorf("qianfan path = %s", r.URL.Path)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(okBody(1)))
	}))
	defer qf.Close()
	c := New(gw.URL, "gw-token", qf.URL, "qf-key", "m", 16, 0)
	vecs, err := c.EmbedBatch(context.Background(), []string{"a"})
	if err != nil {
		t.Fatalf("fallback should succeed: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != Dim {
		t.Fatalf("fallback vecs: %dx%d", len(vecs), len(vecs[0]))
	}
	if gwCalls != 1 || qfCalls != 1 {
		t.Fatalf("gw=%d qf=%d", gwCalls, qfCalls)
	}
}

func TestBothChannelsFail(t *testing.T) {
	gw := aigwServer(t, 500, nil)
	defer gw.Close()
	qf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer qf.Close()
	c := New(gw.URL, "gw-token", qf.URL, "qf-key", "m", 16, 1)
	if _, err := c.EmbedBatch(context.Background(), []string{"a"}); err == nil {
		t.Fatal("both failing must error")
	}
}

func TestNoChannelsConfigured(t *testing.T) {
	c := New("", "", "", "", "m", 16, 1)
	_, err := c.EmbedBatch(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "无可用通道") {
		t.Fatalf("want no-channel error, got %v", err)
	}
}

func TestPrimaryFailNoFallbackConfigured(t *testing.T) {
	gw := aigwServer(t, 500, nil)
	defer gw.Close()
	c := New(gw.URL, "gw-token", "", "", "m", 16, 1) // 未配千帆
	_, err := c.EmbedBatch(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("want no-fallback error, got %v", err)
	}
}

func TestContextCanceledBetweenChannels(t *testing.T) {
	gw := aigwServer(t, 500, nil)
	defer gw.Close()
	c := New(gw.URL, "gw-token", "http://127.0.0.1:1", "qf", "m", 16, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.EmbedBatch(ctx, []string{"a"})
	if err == nil {
		t.Fatal("canceled ctx must error")
	}
}
