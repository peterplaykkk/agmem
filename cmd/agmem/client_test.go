package main

// G07：CLI 401/服务端不可达的可行动错误文案（UC-001 B1 / UC-009 B1）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 401 → 保留服务端错误原文 + 追加 AGMEM_URL/AGMEM_TOKEN 与凭据文件提示。
func TestAgmemClient_401ActionableMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized: 需要 Bearer token 或登录 session"}`))
	}))
	defer srv.Close()

	c := &agmemClient{base: srv.URL, token: "bogus", http: srv.Client()}
	err := c.do(http.MethodGet, "/api/kb", nil, nil)
	if err == nil {
		t.Fatal("want 401 error")
	}
	msg := err.Error()
	for _, want := range []string{"401", "unauthorized", "AGMEM_URL", "AGMEM_TOKEN", "~/.config/agmem/agmem.env"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("401 message missing %q:\n%s", want, msg)
		}
	}
}

// 服务端不可达 → 提示 AGMEM_URL 与网络连通性。
func TestAgmemClient_UnreachableMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // 关闭后连接必失败

	c := &agmemClient{base: url, token: "t", http: &http.Client{Timeout: 2 * time.Second}}
	err := c.do(http.MethodGet, "/api/kb", nil, nil)
	if err == nil {
		t.Fatal("want connection error")
	}
	msg := err.Error()
	for _, want := range []string{"不可达", "AGMEM_URL", url} {
		if !strings.Contains(msg, want) {
			t.Fatalf("unreachable message missing %q:\n%s", want, msg)
		}
	}
}

// 非 401 错误保持既有格式（不误加凭据提示）。
func TestAgmemClient_OtherErrorUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"kb 不存在: x"}`))
	}))
	defer srv.Close()

	c := &agmemClient{base: srv.URL, token: "t", http: srv.Client()}
	err := c.do(http.MethodGet, "/api/kb/x/dirs", nil, nil)
	if err == nil || err.Error() != "agmem [404]: kb 不存在: x" {
		t.Fatalf("non-401 format changed: %v", err)
	}
}
