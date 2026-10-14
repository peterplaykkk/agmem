package api

// 契约批新增端点测试：401（无凭证）、/api/me（machine/session）、login（OIDC 未配置/配置后 redirect）、
// 权限过滤与分页需 MySQL —— 集成测试在 internal/store（DSN 守卫），此处覆盖 HTTP 层语义。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sre/agmem/internal/auth"
)

func TestKBList_Unauthorized401(t *testing.T) {
	s := testServer()
	h := s.Routes()
	for _, path := range []string{"/api/kb", "/api/me", "/api/version/sre", "/api/kb/sre/points"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without creds: want 401, got %d", path, rec.Code)
		}
	}
	// 无效 token 同样 401
	req := httptest.NewRequest("GET", "/api/kb", nil)
	req.Header.Set("Authorization", "Bearer bogus")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bogus token: want 401, got %d", rec.Code)
	}
}

func TestMe_MachineToken(t *testing.T) {
	s := testServer()
	h := s.Routes()
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"machine"`) {
		t.Fatalf("want machine role in %s", body)
	}
}

func TestMe_SessionCookie(t *testing.T) {
	s := testServer()
	sid, err := s.Session.Put("user-sub-1")
	if err != nil {
		t.Fatalf("session put: %v", err)
	}
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: "agmem_session", Value: sid})
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "user-sub-1") {
		t.Fatalf("want sub in body: %s", rec.Body.String())
	}
	// 过期/伪造 cookie → 401
	bad := httptest.NewRequest("GET", "/api/me", nil)
	bad.AddCookie(&http.Cookie{Name: "agmem_session", Value: "forged"})
	rec2 := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec2, bad)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("forged session: want 401, got %d", rec2.Code)
	}
}

func TestLogin_OIDCNotConfigured(t *testing.T) {
	s := testServer() // OIDC nil
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
}

func TestLogin_OIDCRedirect(t *testing.T) {
	// 假 IdP discovery：authorize URL 生成不触网（oauth2 Endpoint 来自 discovery）
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"issuer": "` + serverURL(t, r) + `",
				"authorization_endpoint": "` + serverURL(t, r) + `/auth",
				"token_endpoint": "` + serverURL(t, r) + `/token",
				"jwks_uri": "` + serverURL(t, r) + `/keys"
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer idp.Close()
	s := testServer()
	s.OIDC = auth.NewOIDC(auth.OIDCConfig{
		Issuer: idp.URL, ClientID: "cid", ClientSecret: "cs",
	}, "https://agmem-test.example.com")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/auth?") || !strings.Contains(loc, "client_id=cid") ||
		!strings.Contains(loc, "response_type=code") {
		t.Fatalf("authorize URL malformed: %s", loc)
	}
	if !strings.Contains(loc, "redirect_uri=https%3A%2F%2Fagmem-test.example.com%2Fapi%2Fcallback") {
		t.Fatalf("redirect_uri wrong: %s", loc)
	}
	// state cookie 必须下发
	found := false
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "agmem_oidc_state" && ck.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("state cookie missing")
	}
}

func TestCallback_StateMismatch(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer idp.Close()
	s := testServer()
	s.OIDC = auth.NewOIDC(auth.OIDCConfig{Issuer: idp.URL, ClientID: "cid"}, "https://x.example.com")
	req := httptest.NewRequest("GET", "/api/callback?code=abc&state=wrong", nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("state mismatch: want 400, got %d", rec.Code)
	}
}

func serverURL(t *testing.T, r *http.Request) string {
	t.Helper()
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// hostOf 提取 httptest server URL 的 host（未用；保留说明）。
