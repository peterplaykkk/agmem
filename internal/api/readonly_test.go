package api

// 只读客户端令牌（AGMEM_CLIENT_READONLY_TOKEN）鉴权矩阵：
//   - 白名单端点（POST /api/ask、GET /api/version/{kb}、GET /api/version/global、
//     GET /api/me）可达；
//   - 其余全部端点按 404 掩蔽拒绝（逐个断言，不泄露端点存在）；
//   - 机器 token 行为不变（不受白名单限制）；
//   - 无凭证仍 401。
//
// store 用 sqlite（modernc，纯 Go 免 MySQL）承载只读 SQL；ask 用 httptest
// embedding/qdrant fake。不依赖外网。

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"sre/agmem/internal/auth"
	"sre/agmem/internal/config"
	"sre/agmem/internal/qdrant"
	"sre/agmem/internal/store"
)

const (
	roToken      = "ro-token-1"
	roTokenAlt   = "ro-token-2"
	machineToken = "test-token"
)

// newReadonlyTestServer 组装一个带只读令牌 + 机器 token 的 Server：
// Store 用 sqlite 提供只读查询所需表；Embedder/Qdrant 用 fake 支撑 /api/ask。
func newReadonlyTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ro.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, ddl := range []string{
		`CREATE TABLE knowledge_bases (id INTEGER PRIMARY KEY, name TEXT, owner_sub TEXT)`,
		`CREATE TABLE kb_versions (kb_id INTEGER PRIMARY KEY, version INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE kb_readers (kb_id INTEGER, sub TEXT)`,
		`CREATE TABLE directories (id INTEGER PRIMARY KEY, kb_id INTEGER, name TEXT)`,
		`CREATE TABLE task_items (id INTEGER PRIMARY KEY, kb_id INTEGER, task_id INTEGER, status TEXT)`,
		`CREATE TABLE tasks (id INTEGER PRIMARY KEY, kb_id INTEGER, status TEXT)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO knowledge_bases (id, name, owner_sub) VALUES (1, 'sre', 'owner-1')`); err != nil {
		t.Fatalf("seed kb: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO kb_versions (kb_id, version) VALUES (1, 7)`); err != nil {
		t.Fatalf("seed version: %v", err)
	}

	qh := &fakeQdrantFanout{scores: map[string]float64{"sre": 0.9}}
	qsrv := httptest.NewServer(qh)
	t.Cleanup(qsrv.Close)

	s := &Server{
		Machine:  auth.NewMachineAuth(machineToken),
		Readonly: auth.NewReadonlyAuth([]string{roToken, roTokenAlt}),
		Session:  auth.NewSession(time.Hour),
		Store:    &store.Store{DB: db},
		Embedder: fakeEmbedder(t),
		Qdrant:   qdrant.New(qsrv.URL, "", time.Second),
		Cfg:      config.Config{KB: "sre", HybridEnabled: false, ScoreThreshold: 0.5},
	}
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return ts
}

func doRO(t *testing.T, url, method, path, body, token string) int {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url+path, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// 白名单端点：只读令牌可达（非 404/401）。
func TestReadonlyToken_AllowedEndpoints(t *testing.T) {
	ts := newReadonlyTestServer(t)
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"me", http.MethodGet, "/api/me", "", http.StatusOK},
		{"ask检索", http.MethodPost, "/api/ask", `{"query":"部署流程"}`, http.StatusOK},
		{"ask空query仍可达(400非404)", http.MethodPost, "/api/ask", `{}`, http.StatusBadRequest},
		{"version单库", http.MethodGet, "/api/version/sre", "", http.StatusOK},
		{"version-global", http.MethodGet, "/api/version/global", "", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := doRO(t, ts.URL, c.method, c.path, c.body, roToken)
			if got != c.want {
				t.Fatalf("%s %s: want %d, got %d", c.method, c.path, c.want, got)
			}
			if got == http.StatusNotFound || got == http.StatusUnauthorized {
				t.Fatalf("%s %s masked as %d — 白名单端点不得被拒", c.method, c.path, got)
			}
		})
	}
	// 第二个令牌同样放行（多令牌配置）。
	if got := doRO(t, ts.URL, http.MethodGet, "/api/version/global", "", roTokenAlt); got != http.StatusOK {
		t.Fatalf("alt token version-global: want 200, got %d", got)
	}
}

// 非白名单端点：只读令牌一律 404 掩蔽（逐个断言）。
func TestReadonlyToken_DeniedEndpointsMasked404(t *testing.T) {
	ts := newReadonlyTestServer(t)
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"wiki状态", http.MethodGet, "/api/wiki-sync/status", ""},
		{"kb列表", http.MethodGet, "/api/kb", ""},
		{"建kb", http.MethodPost, "/api/kb", `{"name":"x"}`},
		{"目录列表", http.MethodGet, "/api/kb/sre/dirs", ""},
		{"建目录", http.MethodPost, "/api/kb/sre/dir", `{"name":"d"}`},
		{"上传文档", http.MethodPost, "/api/kb/sre/docs", `{"dir":"d","texts":["t"]}`},
		{"任务状态", http.MethodGet, "/api/kb/sre/tasks/1", ""},
		{"条目分页", http.MethodGet, "/api/kb/sre/points", ""},
		{"删除条目", http.MethodDelete, "/api/kb/sre/items/1", ""},
		{"重试条目", http.MethodPost, "/api/kb/sre/items/1/retry", ""},
		{"reader列表", http.MethodGet, "/api/kb/sre/readers", ""},
		{"新增reader", http.MethodPost, "/api/kb/sre/readers", `{"sub":"bob"}`},
		{"删reader", http.MethodDelete, "/api/kb/sre/readers/bob", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := doRO(t, ts.URL, c.method, c.path, c.body, roToken)
			if got != http.StatusNotFound {
				t.Fatalf("%s %s: 只读令牌应 404 掩蔽, got %d", c.method, c.path, got)
			}
		})
	}
}

// 机器 token 行为不变：不受只读白名单限制（非白名单端点照常可达）。
func TestReadonlyToken_MachineUnchanged(t *testing.T) {
	ts := newReadonlyTestServer(t)
	// GET /api/kb 对只读令牌 404；机器 token 应 200（ListKBs 走 sqlite）。
	if got := doRO(t, ts.URL, http.MethodGet, "/api/kb", "", roToken); got != http.StatusNotFound {
		t.Fatalf("readonly /api/kb: want 404, got %d", got)
	}
	if got := doRO(t, ts.URL, http.MethodGet, "/api/kb", "", machineToken); got != http.StatusOK {
		t.Fatalf("machine /api/kb: want 200, got %d", got)
	}
	if got := doRO(t, ts.URL, http.MethodGet, "/api/me", "", machineToken); got != http.StatusOK {
		t.Fatalf("machine /api/me: want 200, got %d", got)
	}
}

// 无凭证：白名单/非白名单端点均 401（未认证优先于掩蔽）。
func TestReadonlyToken_Unauthenticated401(t *testing.T) {
	ts := newReadonlyTestServer(t)
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/me"},
		{http.MethodPost, "/api/ask"},
		{http.MethodGet, "/api/version/global"},
		{http.MethodGet, "/api/kb"},
	}
	for _, c := range cases {
		if got := doRO(t, ts.URL, c.method, c.path, "", ""); got != http.StatusUnauthorized {
			t.Fatalf("%s %s no creds: want 401, got %d", c.method, c.path, got)
		}
	}
}

// /api/me 对只读令牌返回可区分于 machine 的 role。
func TestReadonlyToken_MeRole(t *testing.T) {
	ts := newReadonlyTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+roToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, `"client-readonly"`) {
		t.Fatalf("want role client-readonly, got %s", body)
	}
	if strings.Contains(body, `"machine"`) {
		t.Fatalf("readonly must not be reported as machine: %s", body)
	}
}
