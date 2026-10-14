package wikisync

// 飞书客户端（httptest 假飞书）：令牌缓存/提前刷新、节点翻页、raw_content、
// 错误码、obj_edit_time 字符串解析。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFeishu 最小飞书服务端假件（同一份供 feishu_test 与 syncer_test 使用）。
type fakeFeishu struct {
	mu         sync.Mutex
	tokenCalls int
	tree       map[string][]map[string]any // parent node_token（""=顶层）→ 子节点
	content    map[string]string           // obj_token → raw_content
	badObj     map[string]bool             // 指定 obj_token 的 raw_content 返回错误码
	spaceName  string
	failNodes  bool
}

func newFakeFeishu() *fakeFeishu {
	return &fakeFeishu{
		tree:      map[string][]map[string]any{},
		content:   map[string]string{},
		badObj:    map[string]bool{},
		spaceName: "SRE 知识库",
	}
}

func (f *fakeFeishu) server() *httptest.Server { return httptest.NewServer(f) }

func (f *fakeFeishu) client(base string) *FeishuClient {
	return &FeishuClient{BaseURL: base, AppID: "cli_x", AppSecret: "s3cr3t", HTTP: &http.Client{Timeout: 5 * time.Second}}
}

func writeEnv(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeFeishu) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/auth/v3/tenant_access_token/internal"):
		f.mu.Lock()
		f.tokenCalls++
		f.mu.Unlock()
		writeEnv(w, map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "tok-1", "expire": 7200})
	case strings.HasPrefix(p, "/open-apis/wiki/v2/spaces/"):
		rest := strings.TrimPrefix(p, "/open-apis/wiki/v2/spaces/")
		if strings.HasSuffix(rest, "/nodes") {
			f.handleNodes(w, r)
			return
		}
		f.mu.Lock()
		name := f.spaceName
		f.mu.Unlock()
		writeEnv(w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"name": name}})
	case strings.HasPrefix(p, "/open-apis/docx/v1/documents/"):
		obj := strings.TrimSuffix(strings.TrimPrefix(p, "/open-apis/docx/v1/documents/"), "/raw_content")
		f.mu.Lock()
		bad := f.badObj[obj]
		text, ok := f.content[obj]
		f.mu.Unlock()
		if bad || !ok {
			writeEnv(w, map[string]any{"code": 99991663, "msg": "no permission or not found"})
			return
		}
		writeEnv(w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"content": text}})
	default:
		http.NotFound(w, r)
	}
}

// handleNodes 分页返回某父节点下的子节点（page_size 固定 2，用于翻页测试）。
func (f *fakeFeishu) handleNodes(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	fail := f.failNodes
	parent := r.URL.Query().Get("parent_node_token")
	items := append([]map[string]any(nil), f.tree[parent]...)
	f.mu.Unlock()
	if fail {
		writeEnv(w, map[string]any{"code": 99999, "msg": "boom"})
		return
	}
	const pageSize = 2
	offset := 0
	if pt := r.URL.Query().Get("page_token"); pt != "" {
		offset, _ = strconv.Atoi(pt)
	}
	end := offset + pageSize
	if end > len(items) {
		end = len(items)
	}
	page := items[offset:end]
	hasMore := end < len(items)
	next := ""
	if hasMore {
		next = strconv.Itoa(end)
	}
	writeEnv(w, map[string]any{"code": 0, "msg": "ok", "data": map[string]any{
		"has_more": hasMore, "page_token": next, "items": page,
	}})
}

func TestFeishuTokenCacheAndRefresh(t *testing.T) {
	f := newFakeFeishu()
	ts := f.server()
	defer ts.Close()
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, cst)
	c := f.client(ts.URL)
	c.Now = func() time.Time { return now }

	ctx := context.Background()
	tok, err := c.Token(ctx)
	if err != nil || tok != "tok-1" {
		t.Fatalf("Token=%q err=%v", tok, err)
	}
	if _, err := c.Token(ctx); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if f.tokenCalls != 1 {
		t.Fatalf("token 应缓存，期望 1 次调用，得到 %d", f.tokenCalls)
	}
	// 进入提前刷新窗口（expire 7200s，提前 5 分钟）→ 再取应刷新
	now = now.Add(7200*time.Second - 4*time.Minute)
	if _, err := c.Token(ctx); err != nil {
		t.Fatalf("refresh Token: %v", err)
	}
	if f.tokenCalls != 2 {
		t.Fatalf("到期前应刷新，期望 2 次调用，得到 %d", f.tokenCalls)
	}
}

func TestFeishuListNodesPagination(t *testing.T) {
	f := newFakeFeishu()
	for i := 0; i < 5; i++ {
		f.tree[""] = append(f.tree[""], map[string]any{
			"node_token": "n" + strconv.Itoa(i), "obj_token": "d" + strconv.Itoa(i),
			"obj_type": "docx", "title": "doc" + strconv.Itoa(i), "has_child": false,
			"obj_edit_time": strconv.Itoa(1000 + i), // 字符串型时间戳
		})
	}
	ts := f.server()
	defer ts.Close()
	c := f.client(ts.URL)

	nodes, err := c.ListNodes(context.Background(), "space1", "")
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 5 {
		t.Fatalf("翻页应取 5 个节点，得到 %d", len(nodes))
	}
	if nodes[4].Title != "doc4" || int64(nodes[4].ObjEditTime) != 1004 {
		t.Fatalf("末节点解析错误: %+v", nodes[4])
	}
}

func TestFeishuRawContentAndError(t *testing.T) {
	f := newFakeFeishu()
	f.content["good"] = "正文 hello"
	f.badObj["bad"] = true
	ts := f.server()
	defer ts.Close()
	c := f.client(ts.URL)

	got, err := c.RawContent(context.Background(), "good")
	if err != nil || got != "正文 hello" {
		t.Fatalf("RawContent=%q err=%v", got, err)
	}
	if _, err := c.RawContent(context.Background(), "bad"); err == nil {
		t.Fatal("错误码应返回 error")
	}
}

func TestFeishuFlexInt64(t *testing.T) {
	var n Node
	if err := json.Unmarshal([]byte(`{"obj_edit_time":"1609296809"}`), &n); err != nil {
		t.Fatalf("unmarshal string: %v", err)
	}
	if int64(n.ObjEditTime) != 1609296809 {
		t.Fatalf("string 时间戳解析错误: %d", n.ObjEditTime)
	}
	if err := json.Unmarshal([]byte(`{"obj_edit_time":42}`), &n); err != nil {
		t.Fatalf("unmarshal number: %v", err)
	}
	if int64(n.ObjEditTime) != 42 {
		t.Fatalf("number 时间戳解析错误: %d", n.ObjEditTime)
	}
}
