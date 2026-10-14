package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sre/agmem/internal/auth"
)

func testServer() *Server {
	return &Server{
		Machine: auth.NewMachineAuth("test-token"),
		Session: auth.NewSession(time.Hour),
	}
}

func TestSecurityHeadersOnAllRoutes(t *testing.T) {
	s := testServer()
	h := s.Routes()
	for _, path := range []string{"/api/healthz", "/api/kb", "/api/ask", "/nope"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		for k, want := range map[string]string{
			"X-Content-Type-Options":  "nosniff",
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "no-referrer",
			"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'; object-src 'none'",
		} {
			if got := rec.Header().Get(k); got != want {
				t.Errorf("%s %s: header %s=%q want %q", "GET", path, k, got, want)
			}
		}
	}
}

func TestAuthMachineToken(t *testing.T) {
	s := testServer()
	h := s.Routes()
	req := httptest.NewRequest("GET", "/api/version/kb1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", rec.Code)
	}

	req = httptest.NewRequest("POST", "/api/ask", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("valid token rejected: %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/api/version/kb1", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: want 401, got %d", rec.Code)
	}
}

func TestContainsSlash(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"sre", false},
		{"a/b", true},
		{"/lead", true},
		{"", false},
	}
	for _, c := range cases {
		if got := containsSlash(c.in); got != c.want {
			t.Errorf("containsSlash(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestInt64Path(t *testing.T) {
	req := httptest.NewRequest("GET", "/x/123", nil)
	req.SetPathValue("task_id", "123")
	w := httptest.NewRecorder()
	v, ok := int64Path(w, req, "task_id")
	if !ok || v != 123 {
		t.Fatalf("want 123 ok, got %d %v", v, ok)
	}

	req2 := httptest.NewRequest("GET", "/x/abc", nil)
	req2.SetPathValue("task_id", "abc")
	w2 := httptest.NewRecorder()
	if _, ok := int64Path(w2, req2, "task_id"); ok {
		t.Fatal("abc must fail")
	}
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w2.Code)
	}
}

// 防御：安全头不得被任何 handler 覆盖删除
func TestSecurityHeadersNotOverridden(t *testing.T) {
	s := testServer()
	h := s.Routes()
	req := httptest.NewRequest("GET", "/api/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.HasPrefix(rec.Header().Get("Content-Security-Policy"), "default-src") {
		t.Fatal("CSP missing after handler chain")
	}
}
