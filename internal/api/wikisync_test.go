package api

// wiki 同步可观测接线：healthz 的 wiki_sync 段（形状/加法语义）、
// /api/wiki-sync/status（鉴权 + 同内容）、/metrics 的 agmem_wikisync_* 指标、
// 以及目录名/来源名归一纯函数。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sre/agmem/internal/wikisync"
)

type fakeWikiSyncStatus struct{ st wikisync.Status }

func (f fakeWikiSyncStatus) Snapshot() wikisync.Status { return f.st }

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body: %v\n%s", err, rec.Body.String())
	}
	return out
}

func TestHealthzWikiSyncSegment(t *testing.T) {
	s := testServer()
	s.Cfg.Version = "v1"
	h := s.Routes()

	// 未启用（provider nil）：既有 status/version 语义不变，wiki_sync 段 enabled=false。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz 应 200，得到 %d", rec.Code)
	}
	out := decodeBody(t, rec)
	if out["status"] != "ok" || out["version"] != "v1" {
		t.Fatalf("既有字段语义被改: %v", out)
	}
	ws, ok := out["wiki_sync"].(map[string]any)
	if !ok {
		t.Fatalf("healthz 应含 wiki_sync 段: %v", out)
	}
	if ws["enabled"] != false {
		t.Fatalf("未启用应 enabled=false: %v", ws)
	}

	// 启用且有一轮结果：各计数字段形状稳定。
	s.WikiSync = fakeWikiSyncStatus{st: wikisync.Status{
		Enabled: true, LastScanned: 42, LastAdded: 3, LastChanged: 1, LastFailed: 0,
		TreeNodes: 12, MaxDepth: 3,
		LastRunAt: "2026-09-10T12:00:00+08:00", NextRunAt: "2026-09-10T13:00:00+08:00",
	}}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/healthz", nil))
	out = decodeBody(t, rec)
	ws = out["wiki_sync"].(map[string]any)
	if ws["enabled"] != true || ws["last_scanned_nodes"] != float64(42) || ws["last_added"] != float64(3) {
		t.Fatalf("wiki_sync 段字段错误: %v", ws)
	}
	if ws["tree_nodes"] != float64(12) || ws["max_depth"] != float64(3) {
		t.Fatalf("wiki_sync 段应透出 tree_nodes/max_depth: %v", ws)
	}
	if ws["next_run_at"] != "2026-09-10T13:00:00+08:00" {
		t.Fatalf("next_run_at 缺失: %v", ws)
	}
}

func TestWikiSyncStatusEndpoint(t *testing.T) {
	s := testServer()
	s.Cfg.Version = "v1"
	s.WikiSync = fakeWikiSyncStatus{st: wikisync.Status{Enabled: true, LastAdded: 5, Runs: 2}}
	h := s.Routes()

	// 未鉴权 401。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/wiki-sync/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权应 401，得到 %d", rec.Code)
	}

	// 机器 token 放行，返回与 healthz 同内容。
	req := httptest.NewRequest("GET", "/api/wiki-sync/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("鉴权后应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["enabled"] != true || out["last_added"] != float64(5) || out["runs"] != float64(2) {
		t.Fatalf("status 内容错误: %v", out)
	}
}

func TestMetricsWikiSync(t *testing.T) {
	s := testServer()
	// 默认（从未运行）：序列恒在且 enabled=0。
	_, _, body := metricsText(t, s)
	if !strings.Contains(body, "agmem_wikisync_enabled 0") {
		t.Fatalf("默认应输出 enabled=0:\n%s", body)
	}

	s.MetricsRegistry().ObserveWikiSync(wikisync.Status{
		Enabled: true, Runs: 4, TotalAdded: 2, TotalChanged: 1, TotalRemoved: 1, TotalFailed: 3,
		LastScanned: 10, LastRunUnix: 1700000000, LastDurationMS: 1500, LastFailed: 3,
	})
	_, _, body = metricsText(t, s)
	for _, want := range []string{
		"agmem_wikisync_enabled 1",
		"agmem_wikisync_runs_total 4",
		"agmem_wikisync_added_total 2",
		"agmem_wikisync_changed_total 1",
		"agmem_wikisync_removed_total 1",
		"agmem_wikisync_failed_total 3",
		"agmem_wikisync_scanned_nodes 10",
		"agmem_wikisync_last_run_timestamp_seconds 1700000000",
		"agmem_wikisync_last_duration_seconds 1.5",
		"agmem_wikisync_last_run_failed 3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics 缺 %q\n%s", want, body)
		}
	}
}

func TestNormalizeWikiDir(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "wiki"},
		{"   ", "wiki"},
		{"ops", "ops"},
		{"a/b", "a-b"},
		{"  a/b  ", "a-b"},
		{"已存在目录", "已存在目录"},
	}
	for _, c := range cases {
		if got := normalizeWikiDir(c.in); got != c.want {
			t.Errorf("normalizeWikiDir(%q)=%q want %q", c.in, got, c.want)
		}
	}
	// 超长截断到 100 字符
	long := strings.Repeat("x", 300)
	if got := normalizeWikiDir(long); len([]rune(got)) != 100 {
		t.Errorf("超长目录应截断到 100 字符，得到 %d", len([]rune(got)))
	}
}

func TestWikiSourceName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "未命名文档"},
		{"  ", "未命名文档"},
		{"部署手册", "部署手册"},
		{"a\nb\tc", "a b c"},
		{"  前后  ", "前后"},
	}
	for _, c := range cases {
		if got := wikiSourceName(c.in); got != c.want {
			t.Errorf("wikiSourceName(%q)=%q want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("y", 400)
	if got := wikiSourceName(long); len([]rune(got)) != 255 {
		t.Errorf("超长来源名应截断到 255 字符，得到 %d", len([]rune(got)))
	}
}
