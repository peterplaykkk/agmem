package auth

import (
	"net/http/httptest"
	"testing"
)

// 只读客户端令牌：多令牌任一命中、空集合恒 false、错误令牌 false。
func TestReadonlyAuth(t *testing.T) {
	cases := []struct {
		name   string
		tokens []string
		header string
		want   bool
	}{
		{"首个令牌命中", []string{"ro-1", "ro-2"}, "Bearer ro-1", true},
		{"次个令牌命中", []string{"ro-1", "ro-2"}, "Bearer ro-2", true},
		{"错误令牌", []string{"ro-1", "ro-2"}, "Bearer nope", false},
		{"无 Authorization", []string{"ro-1"}, "", false},
		{"非 Bearer 前缀", []string{"ro-1"}, "Basic ro-1", false},
		{"空集合", nil, "Bearer ro-1", false},
		{"空串令牌被忽略", []string{"", "ro-x"}, "Bearer ro-x", true},
		{"空串令牌不匹配空 Bearer", []string{""}, "Bearer ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := NewReadonlyAuth(c.tokens)
			req := httptest.NewRequest("GET", "/api/me", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			if got := a.Check(req); got != c.want {
				t.Fatalf("Check=%v want %v (tokens=%v header=%q)", got, c.want, c.tokens, c.header)
			}
		})
	}
}

// nil 接收者不得 panic（未配置 AGMEM_CLIENT_READONLY_TOKEN 时 Server.Readonly 为 nil）。
func TestReadonlyAuthNilReceiver(t *testing.T) {
	var a *ReadonlyAuth
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	if a.Check(req) {
		t.Fatal("nil ReadonlyAuth must never authenticate")
	}
}
