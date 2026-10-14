package api

// UC-011 知识生命周期端点测试（表驱动）：
//   - A3 鉴权矩阵：未登录 401 / reader 403 / 非读者 404 掩蔽 / owner 与机器 token 放行；
//   - A1 删除：qdrant 点位（original + 全部 clean entry）被删、MySQL 行清理、版本 +1、
//     二次调用 404；
//   - 重试：failed→pending 成功；cleaned→409 且不改状态；不存在 404；
//   - A4 版本：变更类操作 bump，幂等重放不 bump；
//   - A5 审计：删除写结构化日志（principal/kb/item_id/结果）。
//
// store 用内存 fake、qdrant 用 httptest fake（真实 qdrant.Client wire：scroll + delete），
// 不依赖 MySQL / 外网。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"sre/agmem/internal/auth"
	"sre/agmem/internal/config"
	"sre/agmem/internal/qdrant"
	"sre/agmem/internal/store"
)

// —— 内存 store fake ——

type fakeLifecycleStore struct {
	mu       sync.Mutex
	kbName   string
	ownerSub string
	accessOK bool
	version  uint64
	items    map[int64]*store.TaskItem
	readers  map[string]bool
	// extra 额外可见 kb（G36 跨库测试：第二 kb 只读可解析）。
	extra map[string]*store.KB

	bumps     int
	deleted   []int64
	requeued  []int64
	refreshed []int64
}

func newFakeLifecycleStore() *fakeLifecycleStore {
	return &fakeLifecycleStore{
		kbName:   "sre",
		ownerSub: "owner-1",
		accessOK: true,
		version:  7,
		items:    map[int64]*store.TaskItem{},
		readers:  map[string]bool{},
	}
}

func (f *fakeLifecycleStore) seedItem(id int64, status store.TaskStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[id] = &store.TaskItem{
		ID: id, TaskID: 5, KBID: 1, DirID: 2, Status: status,
		OriginalText: "原始文本", Error: store.NewNullString("boom"),
	}
}

func (f *fakeLifecycleStore) seedReader(sub string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readers[sub] = true
}

// 单库读路径（/api/version/global 等）也需要 fake 满足 singleKBStore。
func (f *fakeLifecycleStore) GetKBDirByName(_ context.Context, _ int64, name string) (*store.Dir, error) {
	return &store.Dir{ID: 2, KBID: 1, Name: name}, nil
}

func (f *fakeLifecycleStore) CreateTaskWithItems(_ context.Context, _, _ int64, _ []string, _ []string) (int64, int, error) {
	return 0, 0, store.ErrAllDuplicates
}

func (f *fakeLifecycleStore) GetKB(_ context.Context, name string) (*store.KB, error) {
	if kb, ok := f.extra[name]; ok {
		return kb, nil
	}
	if name != f.kbName {
		return nil, store.ErrNotFound
	}
	return &store.KB{ID: 1, Name: f.kbName, OwnerSub: f.ownerSub}, nil
}

func (f *fakeLifecycleStore) HasReadAccess(_ context.Context, _ int64, _ string, _ bool) (bool, error) {
	return f.accessOK, nil
}

func (f *fakeLifecycleStore) GetVersion(_ context.Context, _ int64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, nil
}

func (f *fakeLifecycleStore) BumpVersion(_ context.Context, _ int64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version++
	f.bumps++
	return f.version, nil
}

func (f *fakeLifecycleStore) GetItem(_ context.Context, _, itemID int64) (*store.TaskItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[itemID]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *it
	return &cp, nil
}

func (f *fakeLifecycleStore) DeleteItem(_ context.Context, _, itemID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[itemID]; !ok {
		return store.ErrNotFound
	}
	delete(f.items, itemID)
	f.deleted = append(f.deleted, itemID)
	return nil
}

func (f *fakeLifecycleStore) RequeueItem(_ context.Context, _, itemID int64) (store.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[itemID]
	if !ok {
		return "", store.ErrNotFound
	}
	if it.Status != store.StatusFailed {
		return it.Status, store.ErrInvalidState
	}
	it.Status = store.StatusPending
	it.Error = store.NullString{}
	f.requeued = append(f.requeued, itemID)
	return store.StatusPending, nil
}

func (f *fakeLifecycleStore) RefreshTaskStatus(_ context.Context, taskID int64) (store.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = append(f.refreshed, taskID)
	return store.StatusPending, nil
}

func (f *fakeLifecycleStore) AddReader(_ context.Context, _ int64, sub string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readers[sub] {
		return false, nil
	}
	f.readers[sub] = true
	return true, nil
}

func (f *fakeLifecycleStore) RemoveReader(_ context.Context, _ int64, sub string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.readers[sub] {
		return false, nil
	}
	delete(f.readers, sub)
	return true, nil
}

func (f *fakeLifecycleStore) ListReaders(_ context.Context, _ int64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.readers))
	for s := range f.readers {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeLifecycleStore) statusOf(id int64) (store.TaskStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[id]
	if !ok {
		return "", false
	}
	return it.Status, true
}

func (f *fakeLifecycleStore) bumpCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bumps
}

func (f *fakeLifecycleStore) deletedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deleted)
}

func (f *fakeLifecycleStore) hasReader(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readers[sub]
}

func (f *fakeLifecycleStore) setAccess(ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessOK = ok
}

// —— qdrant fake（scroll + delete 两个真实 wire 端点）——

type fakeQdrantLifecycle struct {
	mu         sync.Mutex
	points     map[int64][]string // item_id → 点位 id 清单
	deleted    []string
	scrolls    int
	failScroll bool
	failDelete bool
	// deleteUsedFilter 记录删除请求是否带 filter 段：生命周期删除必须走显式点 id
	// 清单（DeleteByID 语义），出现 filter 即 delete-by-filter 回归。
	deleteUsedFilter bool
}

func (f *fakeQdrantLifecycle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/points/scroll"):
		f.scrolls++
		if f.failScroll {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":{"error":"boom"}}`))
			return
		}
		var body struct {
			Filter struct {
				Must []struct {
					Key   string `json:"key"`
					Match struct {
						Value any `json:"value"`
					} `json:"match"`
				} `json:"must"`
			} `json:"filter"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var itemID int64
		for _, m := range body.Filter.Must {
			if m.Key == "item_id" {
				if v, ok := m.Match.Value.(float64); ok {
					itemID = int64(v)
				}
			}
		}
		pts := make([]map[string]any, 0, len(f.points[itemID]))
		for _, id := range f.points[itemID] {
			pts = append(pts, map[string]any{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"points": pts, "next_page_offset": nil},
		})
	case strings.HasSuffix(r.URL.Path, "/points/delete"):
		if f.failDelete {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":{"error":"boom"}}`))
			return
		}
		var body struct {
			Points []string        `json:"points"`
			Filter json.RawMessage `json:"filter"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Filter) > 0 {
			f.deleteUsedFilter = true
		}
		f.deleted = append(f.deleted, body.Points...)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"status": "completed"}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeQdrantLifecycle) deletedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// —— 测试脚手架 ——

func newLifecycleTestServer(t *testing.T, st *fakeLifecycleStore, qh http.Handler) (*Server, *httptest.Server) {
	t.Helper()
	s := &Server{
		Machine:   auth.NewMachineAuth("test-token"),
		Session:   auth.NewSession(time.Hour),
		Cfg:       config.Config{KB: "sre"},
		Lifecycle: st,
		SingleKB:  st, // /api/version/global 等读路径共用同一 fake
	}
	if qh != nil {
		qsrv := httptest.NewServer(qh)
		t.Cleanup(qsrv.Close)
		s.Qdrant = qdrant.New(qsrv.URL, "", 2*time.Second)
	}
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func doLifecycle(t *testing.T, url, method, path, body, token string, cookie *http.Cookie) (int, map[string]any) {
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
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func sessionCookie(t *testing.T, s *Server, sub string) *http.Cookie {
	t.Helper()
	sid, err := s.Session.Put(sub)
	if err != nil {
		t.Fatalf("session put: %v", err)
	}
	return &http.Cookie{Name: "agmem_session", Value: sid}
}

// —— A3 鉴权矩阵（表驱动，每格独立 store 快照）——

func TestLifecycleAuthMatrix(t *testing.T) {
	type ep struct {
		name   string
		method string
		path   string
		body   string
	}
	eps := []ep{
		{"delete_item", http.MethodDelete, "/api/kb/sre/items/42", ""},
		{"retry_item", http.MethodPost, "/api/kb/sre/items/42/retry", ""},
		{"list_readers", http.MethodGet, "/api/kb/sre/readers", ""},
		{"add_reader", http.MethodPost, "/api/kb/sre/readers", `{"sub":"bob"}`},
		{"remove_reader", http.MethodDelete, "/api/kb/sre/readers/bob", ""},
	}
	cases := []struct {
		name      string
		principal string // machine | owner | reader | stranger | none
		want      int
	}{
		{"未登录401", "none", http.StatusUnauthorized},
		{"reader403", "reader", http.StatusForbidden},
		{"非读者404掩蔽", "stranger", http.StatusNotFound},
		{"owner放行", "owner", http.StatusOK},
		{"机器token放行", "machine", http.StatusOK},
	}
	for _, e := range eps {
		for _, c := range cases {
			t.Run(e.name+"/"+c.name, func(t *testing.T) {
				st := newFakeLifecycleStore()
				st.seedItem(42, store.StatusFailed) // delete/retry 均可达业务分支
				st.seedReader("bob")
				qh := &fakeQdrantLifecycle{points: map[int64][]string{42: {"p-orig", "p-clean0"}}}
				s, ts := newLifecycleTestServer(t, st, qh)

				token := ""
				var cookie *http.Cookie
				switch c.principal {
				case "machine":
					token = "test-token"
				case "owner":
					cookie = sessionCookie(t, s, "owner-1")
				case "reader":
					cookie = sessionCookie(t, s, "reader-1")
				case "stranger":
					st.setAccess(false)
					cookie = sessionCookie(t, s, "stranger")
				}
				code, out := doLifecycle(t, ts.URL, e.method, e.path, e.body, token, cookie)
				if code != c.want {
					t.Fatalf("%s %s as %s: want %d, got %d (%v)", e.method, e.path, c.principal, c.want, code, out)
				}
				if c.want == http.StatusUnauthorized || c.want == http.StatusForbidden {
					if st.bumpCount() != 0 || st.deletedCount() != 0 {
						t.Fatalf("rejected request must not mutate store: bumps=%d deleted=%d",
							st.bumpCount(), st.deletedCount())
					}
				}
			})
		}
	}
}

// —— A1：删除点位 + 元数据 + 版本 +1 + 二次 404 ——

func TestDeleteItem_RemovesPointsAndBumpsVersion(t *testing.T) {
	st := newFakeLifecycleStore()
	st.seedItem(42, store.StatusCleaned)
	qh := &fakeQdrantLifecycle{points: map[int64][]string{42: {"p-orig", "p-clean0", "p-clean1"}}}
	_, ts := newLifecycleTestServer(t, st, qh)

	code, out := doLifecycle(t, ts.URL, http.MethodDelete, "/api/kb/sre/items/42", "", "test-token", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, out)
	}
	if out["deleted_points"] != float64(3) || out["version"] != float64(8) {
		t.Fatalf("want deleted_points=3 version=8, got %v", out)
	}
	if got := qh.deletedIDs(); strings.Join(got, ",") != "p-orig,p-clean0,p-clean1" {
		t.Fatalf("qdrant delete ids wrong: %v", got)
	}
	if qh.deleteUsedFilter {
		t.Fatal("delete must use explicit point ids, not delete-by-filter")
	}
	if _, ok := st.statusOf(42); ok {
		t.Fatal("mysql row must be deleted")
	}
	if st.bumpCount() != 1 {
		t.Fatalf("version must bump exactly once, got %d", st.bumpCount())
	}

	// 幂等：二次删除 404，且不再触碰 qdrant / 版本
	code, out = doLifecycle(t, ts.URL, http.MethodDelete, "/api/kb/sre/items/42", "", "test-token", nil)
	if code != http.StatusNotFound {
		t.Fatalf("second delete: want 404, got %d: %v", code, out)
	}
	if !strings.Contains(fmt.Sprint(out["error"]), "条目不存在") {
		t.Fatalf("404 must carry explicit error body, got %v", out)
	}
	if len(qh.deletedIDs()) != 3 || st.bumpCount() != 1 {
		t.Fatalf("second delete must be a no-op: deleted=%v bumps=%d", qh.deletedIDs(), st.bumpCount())
	}
}

func TestDeleteItem_ItemNotFound404(t *testing.T) {
	st := newFakeLifecycleStore() // 无 42
	qh := &fakeQdrantLifecycle{}
	_, ts := newLifecycleTestServer(t, st, qh)
	code, out := doLifecycle(t, ts.URL, http.MethodDelete, "/api/kb/sre/items/42", "", "test-token", nil)
	if code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %v", code, out)
	}
	if qh.scrolls != 0 || st.bumpCount() != 0 {
		t.Fatalf("missing item must not touch qdrant/version: scrolls=%d bumps=%d", qh.scrolls, st.bumpCount())
	}
}

func TestDeleteItem_QdrantFailureKeepsMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		qh   *fakeQdrantLifecycle
	}{
		{"scroll失败", &fakeQdrantLifecycle{failScroll: true, points: map[int64][]string{42: {"p1"}}}},
		{"delete失败", &fakeQdrantLifecycle{failDelete: true, points: map[int64][]string{42: {"p1"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeLifecycleStore()
			st.seedItem(42, store.StatusCleaned)
			_, ts := newLifecycleTestServer(t, st, tc.qh)
			code, out := doLifecycle(t, ts.URL, http.MethodDelete, "/api/kb/sre/items/42", "", "test-token", nil)
			if code != http.StatusBadGateway {
				t.Fatalf("want 502, got %d: %v", code, out)
			}
			// 点位删除失败时不得删元数据/不得 bump（否则缓存/检索半完成）
			if _, ok := st.statusOf(42); !ok {
				t.Fatal("metadata must survive qdrant failure")
			}
			if st.bumpCount() != 0 {
				t.Fatalf("version must not bump on qdrant failure, got %d", st.bumpCount())
			}
		})
	}
}

// —— 重试：failed→pending；非 failed 409；不存在 404 ——

func TestRetryItem(t *testing.T) {
	t.Run("failed转pending并bump", func(t *testing.T) {
		st := newFakeLifecycleStore()
		st.seedItem(42, store.StatusFailed)
		_, ts := newLifecycleTestServer(t, st, &fakeQdrantLifecycle{})
		code, out := doLifecycle(t, ts.URL, http.MethodPost, "/api/kb/sre/items/42/retry", "", "test-token", nil)
		if code != http.StatusOK {
			t.Fatalf("want 200, got %d: %v", code, out)
		}
		if out["status"] != "pending" || out["version"] != float64(8) {
			t.Fatalf("want status=pending version=8, got %v", out)
		}
		if stStr, _ := st.statusOf(42); stStr != store.StatusPending {
			t.Fatalf("item status must be pending, got %s", stStr)
		}
		if st.bumpCount() != 1 {
			t.Fatalf("retry must bump version once, got %d", st.bumpCount())
		}
	})
	t.Run("cleaned返回409且不改状态", func(t *testing.T) {
		st := newFakeLifecycleStore()
		st.seedItem(42, store.StatusCleaned)
		_, ts := newLifecycleTestServer(t, st, &fakeQdrantLifecycle{})
		code, out := doLifecycle(t, ts.URL, http.MethodPost, "/api/kb/sre/items/42/retry", "", "test-token", nil)
		if code != http.StatusConflict {
			t.Fatalf("want 409, got %d: %v", code, out)
		}
		if !strings.Contains(fmt.Sprint(out["error"]), "cleaned") {
			t.Fatalf("409 must name current status, got %v", out)
		}
		if stStr, _ := st.statusOf(42); stStr != store.StatusCleaned {
			t.Fatalf("status must stay cleaned, got %s", stStr)
		}
		if st.bumpCount() != 0 {
			t.Fatalf("409 must not bump version, got %d", st.bumpCount())
		}
	})
	t.Run("不存在404", func(t *testing.T) {
		st := newFakeLifecycleStore()
		_, ts := newLifecycleTestServer(t, st, &fakeQdrantLifecycle{})
		code, out := doLifecycle(t, ts.URL, http.MethodPost, "/api/kb/sre/items/7/retry", "", "test-token", nil)
		if code != http.StatusNotFound {
			t.Fatalf("want 404, got %d: %v", code, out)
		}
	})
}

// —— reader 增删查幂等（A4：变更才 bump）——

func TestReadersCRUDIdempotent(t *testing.T) {
	st := newFakeLifecycleStore()
	_, ts := newLifecycleTestServer(t, st, nil)

	code, out := doLifecycle(t, ts.URL, http.MethodGet, "/api/kb/sre/readers", "", "test-token", nil)
	if code != http.StatusOK {
		t.Fatalf("list: want 200, got %d: %v", code, out)
	}
	if r, ok := out["readers"].([]any); !ok || len(r) != 0 {
		t.Fatalf("empty list must be [] not null, got %v", out["readers"])
	}

	code, out = doLifecycle(t, ts.URL, http.MethodPost, "/api/kb/sre/readers", `{"sub":"bob"}`, "test-token", nil)
	if code != http.StatusOK || out["added"] != true || out["version"] != float64(8) {
		t.Fatalf("add: got %d %v", code, out)
	}
	// 幂等重放：added=false 且版本不变（不抖动缓存）
	code, out = doLifecycle(t, ts.URL, http.MethodPost, "/api/kb/sre/readers", `{"sub":"bob"}`, "test-token", nil)
	if code != http.StatusOK || out["added"] != false || out["version"] != float64(8) {
		t.Fatalf("re-add must be no-op: got %d %v", code, out)
	}
	if st.bumpCount() != 1 {
		t.Fatalf("only real change bumps, got %d", st.bumpCount())
	}

	code, out = doLifecycle(t, ts.URL, http.MethodGet, "/api/kb/sre/readers", "", "test-token", nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d %v", code, out)
	}
	if got := fmt.Sprint(out["readers"]); got != "[bob]" {
		t.Fatalf("want [bob], got %s", got)
	}

	code, out = doLifecycle(t, ts.URL, http.MethodDelete, "/api/kb/sre/readers/bob", "", "test-token", nil)
	if code != http.StatusOK || out["removed"] != true || out["version"] != float64(9) {
		t.Fatalf("remove: got %d %v", code, out)
	}
	code, out = doLifecycle(t, ts.URL, http.MethodDelete, "/api/kb/sre/readers/bob", "", "test-token", nil)
	if code != http.StatusOK || out["removed"] != false || out["version"] != float64(9) {
		t.Fatalf("re-remove must be no-op: got %d %v", code, out)
	}
	if st.bumpCount() != 2 {
		t.Fatalf("bumps must be 2 (add+remove), got %d", st.bumpCount())
	}
}

// UC-011 术语别名：请求体 feishu_user 等价 sub。
func TestReaders_FeishuUserAlias(t *testing.T) {
	st := newFakeLifecycleStore()
	_, ts := newLifecycleTestServer(t, st, nil)
	code, out := doLifecycle(t, ts.URL, http.MethodPost, "/api/kb/sre/readers", `{"feishu_user":"ou_x"}`, "test-token", nil)
	if code != http.StatusOK || out["sub"] != "ou_x" || out["added"] != true {
		t.Fatalf("feishu_user alias: got %d %v", code, out)
	}
	if !st.hasReader("ou_x") {
		t.Fatal("reader ou_x must be stored")
	}
}

func TestAddReader_EmptySub400(t *testing.T) {
	st := newFakeLifecycleStore()
	_, ts := newLifecycleTestServer(t, st, nil)
	for _, body := range []string{`{}`, `{"sub":"  "}`, `{"sub":"a/b"}`} {
		code, out := doLifecycle(t, ts.URL, http.MethodPost, "/api/kb/sre/readers", body, "test-token", nil)
		if code != http.StatusBadRequest {
			t.Fatalf("body %s: want 400, got %d: %v", body, code, out)
		}
	}
}

// 安全头不得因新增写端点被绕过（与 server_test.go 同门禁，覆盖生命周期路由）。
func TestLifecycleSecurityHeaders(t *testing.T) {
	st := newFakeLifecycleStore()
	st.seedItem(42, store.StatusCleaned)
	_, ts := newLifecycleTestServer(t, st, &fakeQdrantLifecycle{points: map[int64][]string{42: {"p1"}}})

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/kb/sre/items/42", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for k, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'; object-src 'none'",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("lifecycle route header %s=%q want %q", k, got, want)
		}
	}
}

// —— A5：删除写结构化审计日志 ——

func TestDeleteItem_AuditLog(t *testing.T) {
	st := newFakeLifecycleStore()
	st.seedItem(42, store.StatusCleaned)
	qh := &fakeQdrantLifecycle{points: map[int64][]string{42: {"p1"}}}
	_, ts := newLifecycleTestServer(t, st, qh)

	var buf bytes.Buffer
	old := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(old); log.SetFlags(oldFlags) })

	code, _ := doLifecycle(t, ts.URL, http.MethodDelete, "/api/kb/sre/items/42", "", "test-token", nil)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	line := buf.String()
	for _, want := range []string{"action=delete_item", "principal=machine", "kb=sre", "item_id=42", "result=ok"} {
		if !strings.Contains(line, want) {
			t.Fatalf("audit log missing %q:\n%s", want, line)
		}
	}
}

// 版本一致性（A4）：/api/version/global 读到 bump 后的值。
// 单库 fake 的 GetVersion 与 BumpVersion 共享 version 字段，删除后 global 端点必须变。
func TestDeleteItem_VersionGlobalChanges(t *testing.T) {
	st := newFakeLifecycleStore()
	st.seedItem(42, store.StatusCleaned)
	qh := &fakeQdrantLifecycle{points: map[int64][]string{42: {"p1"}}}
	_, ts := newLifecycleTestServer(t, st, qh)

	_, before := doLifecycle(t, ts.URL, http.MethodGet, "/api/version/global", "", "test-token", nil)
	if before["version"] != float64(7) {
		t.Fatalf("before: want 7, got %v", before)
	}
	doLifecycle(t, ts.URL, http.MethodDelete, "/api/kb/sre/items/42", "", "test-token", nil)
	_, after := doLifecycle(t, ts.URL, http.MethodGet, "/api/version/global", "", "test-token", nil)
	if after["version"] != float64(8) {
		t.Fatalf("after delete: /api/version/global must change to 8, got %v", after)
	}
}
