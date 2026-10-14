package api

// G36 跨库防护：线上存在第二 kb `otherkb`，而写/删除端点恒取 cfg.KB——历史行为是
// 路径 {kb} 被忽略、DELETE /api/kb/otherkb/items/{id} 实际作用在 sre 上（跨库误删）。
// 本测试钉死：路径 kb != cfg.KB 的写/删除一律 404 且零副作用；只读端点按路径
// {kb} 解析，第二 kb 可读。

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"sre/agmem/internal/store"
)

// 写/删除端点：路径 kb != cfg.KB → 404 明确拒绝，且不触碰 store/qdrant。
func TestCrossKBWriteRejected(t *testing.T) {
	st := newFakeLifecycleStore()
	st.seedItem(42, store.StatusFailed)
	st.seedReader("bob")
	st.extra = map[string]*store.KB{"otherkb": {ID: 2, Name: "otherkb", OwnerSub: "owner-2"}}
	qh := &fakeQdrantLifecycle{points: map[int64][]string{42: {"p-orig"}}}
	_, ts := newLifecycleTestServer(t, st, qh)

	eps := []struct{ method, path, body string }{
		{http.MethodDelete, "/api/kb/otherkb/items/42", ""},
		{http.MethodPost, "/api/kb/otherkb/items/42/retry", ""},
		{http.MethodPost, "/api/kb/otherkb/readers", `{"sub":"eve"}`},
		{http.MethodDelete, "/api/kb/otherkb/readers/bob", ""},
		{http.MethodPost, "/api/kb/otherkb/dir", `{"name":"x"}`},
		{http.MethodPost, "/api/kb/otherkb/docs", `{"dir":"d1","texts":["x"]}`},
	}
	for _, e := range eps {
		code, out := doLifecycle(t, ts.URL, e.method, e.path, e.body, "test-token", nil)
		if code != http.StatusNotFound {
			t.Fatalf("%s %s: want 404 (cross-kb write rejected), got %d: %v", e.method, e.path, code, out)
		}
		msg := fmt.Sprint(out["error"])
		if !strings.Contains(msg, "otherkb") || !strings.Contains(msg, "sre") {
			t.Fatalf("%s %s: error must name path kb and configured kb, got %q", e.method, e.path, msg)
		}
	}

	// 零副作用：cfg.KB 的条目/reader 未被改，qdrant 未被触碰
	if st.deletedCount() != 0 || st.bumpCount() != 0 || len(qh.deletedIDs()) != 0 || qh.scrolls != 0 {
		t.Fatalf("cross-kb write must not touch store/qdrant: deleted=%d bumps=%d points=%v scrolls=%d",
			st.deletedCount(), st.bumpCount(), qh.deletedIDs(), qh.scrolls)
	}
	if !st.hasReader("bob") {
		t.Fatal("cfg.KB reader bob must survive cross-kb remove attempt")
	}
	if st.hasReader("eve") {
		t.Fatal("cross-kb add_reader must not mutate cfg.KB")
	}
	if _, ok := st.statusOf(42); !ok {
		t.Fatal("cfg.KB item 42 must survive cross-kb delete attempt")
	}
}

// 只读端点：路径 {kb}=第二 kb 按实际库解析（不误落到 cfg.KB），owner/机器 token 可读。
func TestCrossKBReadAllowed(t *testing.T) {
	st := newFakeLifecycleStore()
	st.seedReader("bob")
	st.extra = map[string]*store.KB{"otherkb": {ID: 2, Name: "otherkb", OwnerSub: "owner-2"}}
	_, ts := newLifecycleTestServer(t, st, nil)

	code, out := doLifecycle(t, ts.URL, http.MethodGet, "/api/kb/otherkb/readers", "", "test-token", nil)
	if code != http.StatusOK {
		t.Fatalf("read second kb readers: want 200, got %d: %v", code, out)
	}
	if out["kb"] != "otherkb" {
		t.Fatalf("response kb must be the path kb otherkb, got %v", out["kb"])
	}
	// 未知 kb 读 → 404（不回落 cfg.KB）
	code, _ = doLifecycle(t, ts.URL, http.MethodGet, "/api/kb/nope/readers", "", "test-token", nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown kb read: want 404, got %d", code)
	}
}
