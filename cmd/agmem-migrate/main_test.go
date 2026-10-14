package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"sre/agmem/internal/bm25"
	"sre/agmem/internal/cleaner"
	"sre/agmem/internal/qdrant"
)

func rep(c rune, n int) string { return strings.Repeat(string(c), n) }

// metaOnlyEntry 复刻 cleaner 内部判定（跨包测试用）：非空行全为 来源：/适用： 行。
func metaOnlyEntry(e string) bool {
	seen := false
	for _, line := range strings.Split(e, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "来源：") && !strings.HasPrefix(line, "适用：") {
			return false
		}
		seen = true
	}
	return seen
}

// —— 内存版 qdrant（scroll/set payload/显式 id delete 走 HTTP 假服务）——

type memPt struct {
	itemID    int64
	text      string
	idx       int
	cnt       int
	sourceDoc string
}

type memQdrant struct {
	mu          sync.Mutex
	pts         map[string]memPt
	scrollCalls int
	upsertCalls int
	deleteCalls int
	deleted     []string
	upserted    []string
	payloadSet  []string // set payload 的显式点 id（REINDEX/SET_SOURCE）
	seq         []string // upsert/delete 调用顺序
	// 稀疏向量闭环证据：EnsureSparse 调用次数、upsert 时是否已 ensure（顺序契约）、
	// 携带 bm25 稀疏向量的点写入次数、每次 upsert 点的 wire JSON（校验命名向量结构）。
	ensureCalls        int
	ensureDone         bool
	upsertBeforeEnsure bool
	sparseWrites       int
	upsertWire         []string
}

type scrollReq struct {
	Filter struct {
		Must []struct {
			Key   string `json:"key"`
			Match struct {
				Value any   `json:"value"`
				Any   []any `json:"any"`
			} `json:"match"`
		} `json:"must"`
	} `json:"filter"`
}

// handler 实现迁移工具用到的三个 HTTP 端点：
//   - POST /collections/{c}/points/scroll（读）
//   - POST /collections/{c}/points/delete（显式 id 删除；含 filter 键直接 400 —— 行为门禁）
//   - POST /collections/{c}/points/payload（set payload 合并）
func (m *memQdrant) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected "+r.Method, http.StatusNotFound)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/scroll"):
			m.handleScroll(w, r)
		case strings.HasSuffix(r.URL.Path, "/points/delete"):
			m.handleDelete(w, r)
		case strings.HasSuffix(r.URL.Path, "/points/payload"):
			m.handleSetPayload(w, r)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	})
}

func (m *memQdrant) handleScroll(w http.ResponseWriter, r *http.Request) {
	var req scrollReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.scrollCalls++
	var pts []map[string]any
	for id, p := range m.pts {
		if !scrollMatch(req, p.itemID) {
			continue
		}
		payload := map[string]any{"item_id": p.itemID, "point_type": "clean", "text": p.text}
		if p.cnt > 0 {
			payload["entry_count"] = p.cnt
			payload["entry_index"] = p.idx
		}
		if p.sourceDoc != "" {
			payload["source_doc"] = p.sourceDoc
		}
		pts = append(pts, map[string]any{"id": id, "payload": payload})
	}
	m.mu.Unlock()
	sort.Slice(pts, func(i, j int) bool { return pts[i]["id"].(string) < pts[j]["id"].(string) })
	_ = json.NewEncoder(w).Encode(map[string]any{
		"result": map[string]any{"points": pts, "next_page_offset": nil},
	})
}

// handleDelete 只接受显式点 id（body.points）；出现 filter 键即 400 —— 与静态门禁
// TestNoFilterDeleteInPackage 双保险（行为层）。
func (m *memQdrant) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Points []string        `json:"points"`
		Filter json.RawMessage `json:"filter"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Filter) > 0 {
		http.Error(w, "filter delete forbidden", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.deleteCalls++
	m.seq = append(m.seq, "delete")
	for _, id := range req.Points {
		delete(m.pts, id)
		m.deleted = append(m.deleted, id)
	}
	m.mu.Unlock()
	_, _ = w.Write([]byte(`{"result":{"status":"completed"}}`))
}

func (m *memQdrant) handleSetPayload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Payload map[string]any `json:"payload"`
		Points  []string       `json:"points"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	for _, id := range req.Points {
		p, ok := m.pts[id]
		if !ok {
			continue
		}
		if v, ok := req.Payload["entry_count"].(float64); ok {
			p.cnt = int(v)
		}
		if v, ok := req.Payload["entry_index"].(float64); ok {
			p.idx = int(v)
		}
		if v, ok := req.Payload["source_doc"].(string); ok {
			p.sourceDoc = v
		}
		m.pts[id] = p
		m.payloadSet = append(m.payloadSet, id)
	}
	m.mu.Unlock()
	_, _ = w.Write([]byte(`{"result":{"status":"completed"}}`))
}

// scrollMatch 复刻 filter 语义：item_id ∈ any 且 point_type=clean。
func scrollMatch(req scrollReq, itemID int64) bool {
	for _, c := range req.Filter.Must {
		switch c.Key {
		case "item_id":
			found := false
			for _, v := range c.Match.Any {
				if f, ok := v.(float64); ok && int64(f) == itemID {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "point_type":
			if s, ok := c.Match.Value.(string); ok && s != "clean" {
				return false
			}
		}
	}
	return true
}

// deps 构造假依赖（Embed 固定向量；Upsert/Delete 改内存状态并记序；EnsureSparse 记次数）。
func (m *memQdrant) deps() resplitDeps {
	return resplitDeps{
		Embed: func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range texts {
				out[i] = []float32{1, 0, 0}
			}
			return out, nil
		},
		EnsureSparse: func(_ context.Context, _ string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.ensureCalls++
			m.ensureDone = true
			return nil
		},
		Upsert: func(_ context.Context, _ string, points []qdrant.Point) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.upsertCalls++
			if !m.ensureDone {
				m.upsertBeforeEnsure = true
			}
			m.seq = append(m.seq, "upsert")
			for _, p := range points {
				if _, ok := p.Sparse[qdrant.SparseVectorName]; ok {
					m.sparseWrites++
				}
				if b, err := json.Marshal(p); err == nil {
					m.upsertWire = append(m.upsertWire, string(b))
				}
				m.pts[p.ID] = memPt{
					itemID: p.Payload.ItemID, text: p.Payload.Text,
					idx: p.Payload.EntryIndex, cnt: p.Payload.EntryCount,
				}
				m.upserted = append(m.upserted, p.ID)
			}
			return nil
		},
		Delete: func(_ context.Context, _ string, ids []string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.deleteCalls++
			m.seq = append(m.seq, "delete")
			for _, id := range ids {
				delete(m.pts, id)
				m.deleted = append(m.deleted, id)
			}
			return nil
		},
	}
}

func (m *memQdrant) rewriteDeps() rewriteDeps {
	d := m.deps()
	return rewriteDeps{Embed: d.Embed, Upsert: d.Upsert, Delete: d.Delete}
}

func (m *memQdrant) recleanDeps(clean func(context.Context, string) (string, error), conc int) recleanDeps {
	d := m.deps()
	return recleanDeps{
		Clean: clean, Embed: d.Embed, Upsert: d.Upsert, Delete: d.Delete,
		EnsureSparse: d.EnsureSparse, Concurrency: conc,
		Sleep: func(time.Duration) {},
	}
}

func writeSRC(t *testing.T, items []resplitItem) string {
	t.Helper()
	return writeJSON(t, items, "resplit.json")
}

func writeJSON(t *testing.T, v any, name string) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// captureStdout 捕获 fn 执行期间的 os.Stdout（runReindexEntry 可观测性断言用）。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() {
		_ = w.Close()
		os.Stdout = old
	}()
	fn()
	// 必须先关写端再收 done：io.ReadAll 只有见到 EOF（w.Close）才返回，
	// 若把它放进 defer，`return <-done` 会与 defer 互相等待而死锁。
	_ = w.Close()
	return <-done
}

// —— G03 默认迁移模式：显式 id 差集删除（禁 filter 删除）——

// TestRunDefaultRewrite_DeletesOnlyStaleIDs 旧点与新确定性 id 差集删除：新点保留、
// 旧点删除、顺序先 upsert 后 delete（2026-09-07 item_id filter 删除会把刚写的点
// 一起删掉 → 永久零 clean 点）。
func TestRunDefaultRewrite_DeletesOnlyStaleIDs(t *testing.T) {
	const itemID = 7
	keepID := cleaner.EntryPointID(itemID, "clean", 0)
	mq := &memQdrant{pts: map[string]memPt{
		"old-a": {itemID: itemID, text: rep('甲', 590)},
		"old-b": {itemID: itemID, text: "残留旧点"},
		keepID:  {itemID: itemID, text: "已存在的确定性新点"},
	}}
	items := []rewriteItem{{
		ID: itemID, Dir: "k8s-clean", TaskID: 42, Count: 3,
		Text:   rep('甲', 590) + "\n\n来源：X",
		OldIDs: []string{"old-a", "old-b", keepID},
	}}
	if err := runDefaultRewrite(context.Background(), items, "sre", mq.rewriteDeps(), false); err != nil {
		t.Fatalf("runDefaultRewrite: %v", err)
	}
	if got := sortedCopy(mq.deleted); !reflect.DeepEqual(got, []string{"old-a", "old-b"}) {
		t.Errorf("删除集合 = %v, want [old-a old-b]（新 id %s 不得被删）", got, keepID)
	}
	if _, ok := mq.pts[keepID]; !ok {
		t.Errorf("确定性新点 %s 被误删（不空窗契约破坏）", keepID)
	}
	if len(mq.seq) != 2 || mq.seq[0] != "upsert" || mq.seq[1] != "delete" {
		t.Errorf("调用顺序 = %v, want [upsert delete]", mq.seq)
	}
	for _, id := range mq.deleted {
		if id == keepID {
			t.Fatalf("差集删除包含新 id %s", keepID)
		}
	}
}

// TestNoFilterDeleteInPackage 静态门禁（C2 / G03）：本包非测试源码不得出现按
// item_id filter 删除——既不得有 rawDeleteByFilter 标识，也不得在任何构造
// points/delete 请求的函数体里出现 "filter" 键。
func TestNoFilterDeleteInPackage(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned, deleteSites := 0, 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		if strings.Contains(string(src), "rawDeleteByFilter") {
			t.Errorf("%s: 出现 rawDeleteByFilter（按 filter 删除，2026-09-07 事故根因）", f)
		}
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, decl := range af.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			body := string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
			if !strings.Contains(body, "points/delete") {
				continue
			}
			deleteSites++
			if strings.Contains(body, `"filter"`) {
				t.Errorf("%s: 函数 %s 构造 points/delete 时出现 \"filter\" 键（禁止 filter 语义删除）", f, fd.Name.Name)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("未扫描到非测试源码——门禁失效")
	}
	if deleteSites == 0 {
		t.Fatal("未发现显式 id 删除点（应为 rawDeletePoints）——门禁失效")
	}
	t.Logf("静态门禁: scanned_files=%d delete_sites=%d", scanned, deleteSites)
}

// —— G04/G38/G39/G40/G50 RECLEAN ——

// TestRunReclean_IdempotentTwoRuns 连续两次同输入：第二次 upsert/delete/bump 全 0、
// 点集合不变（幂等短路）。真实 GLM 条目集合非确定时幂等仅在条目集合相同时成立。
func TestRunReclean_IdempotentTwoRuns(t *testing.T) {
	const itemID = 7
	mq := &memQdrant{pts: map[string]memPt{
		"old": {itemID: itemID, text: "旧文本"},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	path := writeJSON(t, []recleanItem{{ID: itemID, Dir: "k8s", TaskID: 42, Text: "原文"}}, "reclean.json")
	deps := mq.recleanDeps(func(context.Context, string) (string, error) {
		return "条目一正文内容足够长用于通过微型条目过滤\n\n条目二正文内容足够长用于通过微型条目过滤", nil
	}, 2)
	bumps := 0
	deps.Bump = func(context.Context) error { bumps++; return nil }

	if err := runReclean(context.Background(), path, "sre", srv.URL, "k", deps, false, "", false); err != nil {
		t.Fatalf("runReclean 第一次: %v", err)
	}
	if mq.upsertCalls != 1 || mq.deleteCalls != 1 {
		t.Fatalf("第一次 upsert=%d delete=%d, want 1/1", mq.upsertCalls, mq.deleteCalls)
	}
	if got := sortedCopy(mq.deleted); !reflect.DeepEqual(got, []string{"old"}) {
		t.Fatalf("第一次删除集合 = %v, want [old]", got)
	}
	if bumps != 1 {
		t.Fatalf("第一次 bump = %d, want 1", bumps)
	}
	before := len(mq.pts)

	// 第二次：条目集合/文本一致 → 幂等短路，零写零删零 bump。
	if err := runReclean(context.Background(), path, "sre", srv.URL, "k", deps, false, "", false); err != nil {
		t.Fatalf("runReclean 第二次: %v", err)
	}
	if mq.upsertCalls != 1 {
		t.Errorf("第二次 upsert 次数 = %d, want 1（幂等短路）", mq.upsertCalls)
	}
	if mq.deleteCalls != 1 {
		t.Errorf("第二次 delete 次数 = %d, want 1（差集为空）", mq.deleteCalls)
	}
	if bumps != 1 {
		t.Errorf("第二次 bump = %d, want 1（无变更不 bump）", bumps)
	}
	if len(mq.pts) != before {
		t.Errorf("第二次点集合变化: %d → %d", before, len(mq.pts))
	}
}

// TestRunReclean_EmptyCleanedDeletesOldPoints 判空条目（GLM 输出空串）必须删除该
// item 旧 clean 点（G04：原实现直接 continue 留下旧点），且二次运行零删除。
func TestRunReclean_EmptyCleanedDeletesOldPoints(t *testing.T) {
	const itemID = 7
	mq := &memQdrant{pts: map[string]memPt{
		"old-1": {itemID: itemID, text: "旧条目一"},
		"old-2": {itemID: itemID, text: "旧条目二"},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	path := writeJSON(t, []recleanItem{{ID: itemID, Dir: "k8s", TaskID: 42, Text: "原文"}}, "reclean.json")
	deps := mq.recleanDeps(func(context.Context, string) (string, error) { return "", nil }, 1)
	bumps := 0
	deps.Bump = func(context.Context) error { bumps++; return nil }

	if err := runReclean(context.Background(), path, "sre", srv.URL, "k", deps, false, "", false); err != nil {
		t.Fatalf("runReclean: %v", err)
	}
	if got := sortedCopy(mq.deleted); !reflect.DeepEqual(got, []string{"old-1", "old-2"}) {
		t.Fatalf("判空条目删除集合 = %v, want [old-1 old-2]", got)
	}
	if mq.upsertCalls != 0 {
		t.Errorf("判空条目 upsert 次数 = %d, want 0", mq.upsertCalls)
	}
	if bumps != 1 {
		t.Errorf("bump = %d, want 1（清理旧点也是变更）", bumps)
	}
	// 二次运行：无旧点、无新条目 → 幂等短路，零删除。
	if err := runReclean(context.Background(), path, "sre", srv.URL, "k", deps, false, "", false); err != nil {
		t.Fatalf("runReclean 二次: %v", err)
	}
	if mq.deleteCalls != 1 {
		t.Errorf("二次 delete 次数 = %d, want 1（无旧点可删）", mq.deleteCalls)
	}
	if bumps != 1 {
		t.Errorf("二次 bump = %d, want 1", bumps)
	}
}

// TestRunReclean_StaleDeleteFailureCountsFailed 差集删除失败必须计入 failed 并返回
// error、落盘失败清单（G40：原实现只打印错误、终态谎报 failed=0）。
func TestRunReclean_StaleDeleteFailureCountsFailed(t *testing.T) {
	const itemID = 7
	mq := &memQdrant{pts: map[string]memPt{
		"old": {itemID: itemID, text: "旧文本"},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	path := writeJSON(t, []recleanItem{{ID: itemID, Dir: "k8s", TaskID: 42, Text: "原文"}}, "reclean.json")
	deps := mq.recleanDeps(func(context.Context, string) (string, error) {
		return "条目一正文内容足够长用于通过微型条目过滤\n\n条目二正文内容足够长用于通过微型条目过滤", nil
	}, 1)
	deps.Delete = func(context.Context, string, []string) error { return context.DeadlineExceeded }
	failedPath := filepath.Join(t.TempDir(), "failed.json")

	err := runReclean(context.Background(), path, "sre", srv.URL, "k", deps, false, failedPath, false)
	if err == nil {
		t.Fatal("差集删除失败应返回非 nil error")
	}
	b, rerr := os.ReadFile(failedPath)
	if rerr != nil {
		t.Fatalf("失败清单未落盘: %v", rerr)
	}
	var failures []recleanFailure
	if err := json.Unmarshal(b, &failures); err != nil {
		t.Fatalf("失败清单解析失败: %v\n%s", err, b)
	}
	if len(failures) != 1 || failures[0].ItemID != itemID || failures[0].Stage != "delete-stale" {
		t.Errorf("失败清单 = %+v, want 1 条 item=%d stage=delete-stale", failures, itemID)
	}
}

// TestRunReclean_FailedItemKeepsOldPoints GLM 失败：旧点原样保留、返回 error、
// 失败清单含该 item。
func TestRunReclean_FailedItemKeepsOldPoints(t *testing.T) {
	const itemID = 7
	mq := &memQdrant{pts: map[string]memPt{
		"old": {itemID: itemID, text: "旧文本"},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	path := writeJSON(t, []recleanItem{{ID: itemID, Dir: "k8s", TaskID: 42, Text: "原文"}}, "reclean.json")
	deps := mq.recleanDeps(func(context.Context, string) (string, error) {
		return "", context.DeadlineExceeded
	}, 1)
	failedPath := filepath.Join(t.TempDir(), "failed.json")

	err := runReclean(context.Background(), path, "sre", srv.URL, "k", deps, false, failedPath, false)
	if err == nil {
		t.Fatal("GLM 失败应返回非 nil error")
	}
	if mq.upsertCalls != 0 || mq.deleteCalls != 0 {
		t.Errorf("失败 item 不应写库: upsert=%d delete=%d", mq.upsertCalls, mq.deleteCalls)
	}
	if _, ok := mq.pts["old"]; !ok {
		t.Error("失败 item 旧点被误删")
	}
	if b, rerr := os.ReadFile(failedPath); rerr != nil || !strings.Contains(string(b), `"item_id": 7`) {
		t.Errorf("失败清单缺 item 7: err=%v content=%s", rerr, b)
	}
}

// TestRunReclean_DryRunNoWrites DRY_RUN：调 GLM 预览但零写库/零 bump。
func TestRunReclean_DryRunNoWrites(t *testing.T) {
	const itemID = 7
	mq := &memQdrant{pts: map[string]memPt{
		"old": {itemID: itemID, text: "旧文本"},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	path := writeJSON(t, []recleanItem{{ID: itemID, Dir: "k8s", TaskID: 42, Text: "原文"}}, "reclean.json")
	deps := mq.recleanDeps(func(context.Context, string) (string, error) {
		return "条目一正文内容足够长用于通过微型条目过滤\n\n条目二正文内容足够长用于通过微型条目过滤", nil
	}, 1)

	if err := runReclean(context.Background(), path, "sre", srv.URL, "k", deps, true, "", false); err != nil {
		t.Fatalf("runReclean dry: %v", err)
	}
	if mq.upsertCalls != 0 || mq.deleteCalls != 0 {
		t.Errorf("dry 模式写入: upsert=%d delete=%d, want 0/0", mq.upsertCalls, mq.deleteCalls)
	}
	if len(mq.pts) != 1 {
		t.Errorf("dry 模式改变了库状态: %d 点", len(mq.pts))
	}
}

// TestRunReclean_RequireFullDump REQUIRE_FULL_DUMP=1：库内有 item 未被 dump 覆盖
// 直接拒绝（G40 覆盖率门禁），且不触发 GLM 调用。
func TestRunReclean_RequireFullDump(t *testing.T) {
	mq := &memQdrant{pts: map[string]memPt{
		"p7": {itemID: 7, text: "a"},
		"p8": {itemID: 8, text: "b"},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	path := writeJSON(t, []recleanItem{{ID: 7, Dir: "k8s", TaskID: 42, Text: "原文"}}, "reclean.json")
	cleanCalls := 0
	deps := mq.recleanDeps(func(context.Context, string) (string, error) {
		cleanCalls++
		return "x", nil
	}, 1)

	err := runReclean(context.Background(), path, "sre", srv.URL, "k", deps, false, "", true)
	if err == nil {
		t.Fatal("dump 未覆盖全部 item 时应返回 error")
	}
	if cleanCalls != 0 {
		t.Errorf("覆盖率门禁应在调 GLM 前拒绝，cleanCalls=%d", cleanCalls)
	}
}

// —— G15 REINDEX_ENTRY ——

// TestPlanEntryNormalization G15 规范化计划表驱动。覆盖正常/边界/退化：
//   - 无空洞路径保持既有行为（legacy 补 entry_count、高位残留删除、重复 idx 重排）；
//   - 空洞路径（DEDUP 删重复点后 idx 缺口）按保留点实际数量连续重编号，不再报错；
//   - 全空洞/单点退化（保留点 0 或 1 个）。
//
// 变异测试思路（要求 (d)）：把 planEntryNormalization 中 `dup || k != n` 的重编号分支
// 删掉（退回原「len(keep) != n → error」语义），「空洞重编号」用例立即失败——它期望
// 无 error 且 updates 把 idx=[1,2,4,5] 改写为 0..3、entry_count=4；同理去掉 holes 计算
// 会让 wantHoles 断言失败。TestRunReindexEntry_RenumbersHoles 从端到端侧再钉一次。
func TestPlanEntryNormalization(t *testing.T) {
	cases := []struct {
		name        string
		pts         []entryNormPoint
		wantN       int
		wantUpdates map[string][2]int
		wantResidue []string
		wantHoles   int
	}{
		{
			name:        "legacy 单点缺 entry_count",
			pts:         []entryNormPoint{{ID: "l0", Idx: 0, Cnt: 0}},
			wantN:       1,
			wantUpdates: map[string][2]int{"l0": {0, 1}},
		},
		{
			name: "混值高位残留（item 487 形态）",
			pts: []entryNormPoint{
				{ID: "a0", Idx: 0, Cnt: 5}, {ID: "a1", Idx: 1, Cnt: 5},
				{ID: "a2", Idx: 2, Cnt: 5}, {ID: "a3", Idx: 3, Cnt: 5},
				{ID: "a4", Idx: 4, Cnt: 5}, {ID: "a5", Idx: 5, Cnt: 6},
			},
			wantN:       5,
			wantUpdates: map[string][2]int{},
			wantResidue: []string{"a5"},
		},
		{
			name:        "已一致零变更",
			pts:         []entryNormPoint{{ID: "b0", Idx: 0, Cnt: 1}},
			wantN:       1,
			wantUpdates: map[string][2]int{},
		},
		{
			name: "序号重复按序重排",
			pts: []entryNormPoint{
				{ID: "c1", Idx: 0, Cnt: 2}, {ID: "c0", Idx: 0, Cnt: 2},
			},
			wantN:       2,
			wantUpdates: map[string][2]int{"c0": {0, 2}, "c1": {1, 2}},
		},
		{
			// 线上 item 530/564/692 形态：canonical entry_count=6，DEDUP 删掉 idx=0,3 的
			// 重复点后只剩 [1,2,4,5]。要求 (a)：保留 4 点、连续重编号 0..3、entry_count=4。
			name: "空洞重编号（item 530 形态 idx=[1,2,4,5]）",
			pts: []entryNormPoint{
				{ID: "p1", Idx: 1, Cnt: 6}, {ID: "p2", Idx: 2, Cnt: 6},
				{ID: "p4", Idx: 4, Cnt: 6}, {ID: "p5", Idx: 5, Cnt: 6},
			},
			wantN:       4,
			wantUpdates: map[string][2]int{"p1": {0, 4}, "p2": {1, 4}, "p4": {2, 4}, "p5": {3, 4}},
			wantHoles:   2,
		},
		{
			// 空洞与高位残留并存：idx=[1,2,4,7]，canonical=5 → 保留 3 点重编号 0..2，
			// idx=7 仍按残留删除（残留判定不受空洞容错影响）。
			name: "空洞+高位残留并存",
			pts: []entryNormPoint{
				{ID: "q1", Idx: 1, Cnt: 5}, {ID: "q2", Idx: 2, Cnt: 5},
				{ID: "q4", Idx: 4, Cnt: 5}, {ID: "q7", Idx: 7, Cnt: 5},
			},
			wantN:       3,
			wantUpdates: map[string][2]int{"q1": {0, 3}, "q2": {1, 3}, "q4": {2, 3}},
			wantResidue: []string{"q7"},
			wantHoles:   2,
		},
		{
			// 全空洞退化：唯一保留槽位缺失，唯一点本身是高位残留 → 保留 0 点、
			// entry_count=0、无 payload 改写，残留照删（残留规则不变）。
			name:        "全空洞无保留点",
			pts:         []entryNormPoint{{ID: "x9", Idx: 9, Cnt: 1}},
			wantN:       0,
			wantUpdates: map[string][2]int{},
			wantResidue: []string{"x9"},
			wantHoles:   1,
		},
		{
			// 单点退化：唯一 idx=2、canonical=3 → 保留 1 点重编号为 0、entry_count=1。
			name:        "单点退化（idx 非 0）",
			pts:         []entryNormPoint{{ID: "s2", Idx: 2, Cnt: 3}},
			wantN:       1,
			wantUpdates: map[string][2]int{"s2": {0, 1}},
			wantHoles:   2,
		},
		{
			name:        "空输入",
			pts:         nil,
			wantN:       0,
			wantUpdates: nil,
		},
	}
	for _, c := range cases {
		n, updates, residue, holes := planEntryNormalization(c.pts)
		if n != c.wantN {
			t.Errorf("%s: n = %d, want %d", c.name, n, c.wantN)
		}
		if c.wantUpdates != nil && !reflect.DeepEqual(updates, c.wantUpdates) {
			t.Errorf("%s: updates = %v, want %v", c.name, updates, c.wantUpdates)
		}
		if !reflect.DeepEqual(residue, c.wantResidue) {
			t.Errorf("%s: residue = %v, want %v", c.name, residue, c.wantResidue)
		}
		if holes != c.wantHoles {
			t.Errorf("%s: holes = %d, want %d", c.name, holes, c.wantHoles)
		}
	}
}

// TestRunReindexEntry_NormalizesAndDeletesResidue 端到端：legacy 单点补 entry 字段、
// 混值 item 删高位残留、已一致 item 不动；二次运行幂等零写。
func TestRunReindexEntry_NormalizesAndDeletesResidue(t *testing.T) {
	mq := &memQdrant{pts: map[string]memPt{
		"legacy": {itemID: 7, text: "legacy 单点"},
		"a0":     {itemID: 8, text: "a0", idx: 0, cnt: 5},
		"a1":     {itemID: 8, text: "a1", idx: 1, cnt: 5},
		"a2":     {itemID: 8, text: "a2", idx: 2, cnt: 5},
		"a3":     {itemID: 8, text: "a3", idx: 3, cnt: 5},
		"a4":     {itemID: 8, text: "a4", idx: 4, cnt: 5},
		"a5":     {itemID: 8, text: "a5", idx: 5, cnt: 6},
		"b0":     {itemID: 9, text: "b0", idx: 0, cnt: 1},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	bumps := 0
	bump := func(context.Context) error { bumps++; return nil }
	out := captureStdout(t, func() {
		if err := runReindexEntry(context.Background(), "", "sre", srv.URL, "k", false, bump); err != nil {
			t.Fatalf("runReindexEntry: %v", err)
		}
	})
	if strings.Contains(out, ": renumbered ") {
		t.Errorf("无空洞路径不应触发重编号，stdout=%q", out)
	}
	if got := sortedCopy(mq.deleted); !reflect.DeepEqual(got, []string{"a5"}) {
		t.Errorf("残留删除 = %v, want [a5]", got)
	}
	if got := sortedCopy(mq.payloadSet); !reflect.DeepEqual(got, []string{"legacy"}) {
		t.Errorf("payload 改写 = %v, want [legacy]", got)
	}
	if p := mq.pts["legacy"]; p.cnt != 1 || p.idx != 0 {
		t.Errorf("legacy 点 entry 字段 = cnt=%d idx=%d, want cnt=1 idx=0", p.cnt, p.idx)
	}
	if bumps != 1 {
		t.Errorf("bump = %d, want 1", bumps)
	}
	// 二次运行：全部一致 → 零删除/零 payload/零 bump。
	beforeDel, beforeSet, beforeBump := mq.deleteCalls, len(mq.payloadSet), bumps
	if err := runReindexEntry(context.Background(), "", "sre", srv.URL, "k", false, bump); err != nil {
		t.Fatalf("runReindexEntry 二次: %v", err)
	}
	if mq.deleteCalls != beforeDel || len(mq.payloadSet) != beforeSet || bumps != beforeBump {
		t.Errorf("二次运行非幂等: delete=%d→%d payload=%d→%d bump=%d→%d",
			beforeDel, mq.deleteCalls, beforeSet, len(mq.payloadSet), beforeBump, bumps)
	}
}

// TestRunReindexEntry_RenumbersHoles 要求 (a) 端到端：item 530 形态（canonical
// entry_count=6、DEDUP 后 idx=[1,2,4,5]）不再报错——保留 4 点、entry_index 连续 0..3、
// entry_count=4、零删除；点 id 不变（重编号只改 payload）；二次运行幂等零写零 bump。
func TestRunReindexEntry_RenumbersHoles(t *testing.T) {
	mq := &memQdrant{pts: map[string]memPt{
		"i530-1": {itemID: 530, text: "e1", idx: 1, cnt: 6},
		"i530-2": {itemID: 530, text: "e2", idx: 2, cnt: 6},
		"i530-4": {itemID: 530, text: "e4", idx: 4, cnt: 6},
		"i530-5": {itemID: 530, text: "e5", idx: 5, cnt: 6},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	bumps := 0
	bump := func(context.Context) error { bumps++; return nil }
	out := captureStdout(t, func() {
		if err := runReindexEntry(context.Background(), "", "sre", srv.URL, "k", false, bump); err != nil {
			t.Fatalf("runReindexEntry: %v", err)
		}
	})
	if !strings.Contains(out, "item 530: renumbered 4 points (holes=2)") {
		t.Errorf("缺少重编号日志，stdout=%q", out)
	}
	if !strings.Contains(out, "renumbered_items=1") {
		t.Errorf("终态缺少 renumbered_items=1，stdout=%q", out)
	}
	if mq.deleteCalls != 0 || len(mq.deleted) != 0 {
		t.Errorf("空洞 item 不应删除任何点: deleteCalls=%d deleted=%v", mq.deleteCalls, mq.deleted)
	}
	if len(mq.pts) != 4 {
		t.Fatalf("保留点数 = %d, want 4", len(mq.pts))
	}
	wantIdx := map[string]int{"i530-1": 0, "i530-2": 1, "i530-4": 2, "i530-5": 3}
	for id, want := range wantIdx {
		p, ok := mq.pts[id]
		if !ok {
			t.Fatalf("点 %s 被删除（重编号只改 payload，不应删点）", id)
		}
		if p.idx != want || p.cnt != 4 {
			t.Errorf("点 %s: idx=%d cnt=%d, want idx=%d cnt=4", id, p.idx, p.cnt, want)
		}
	}
	if bumps != 1 {
		t.Errorf("bump = %d, want 1", bumps)
	}
	// 二次运行：重编号后 idx/cnt 自洽 → 零 payload/零删除/零 bump，且不再打印重编号。
	beforeSet, beforeDel, beforeBump := len(mq.payloadSet), mq.deleteCalls, bumps
	out2 := captureStdout(t, func() {
		if err := runReindexEntry(context.Background(), "", "sre", srv.URL, "k", false, bump); err != nil {
			t.Fatalf("runReindexEntry 二次: %v", err)
		}
	})
	if len(mq.payloadSet) != beforeSet || mq.deleteCalls != beforeDel || bumps != beforeBump {
		t.Errorf("二次运行非幂等: payload=%d→%d delete=%d→%d bump=%d→%d",
			beforeSet, len(mq.payloadSet), beforeDel, mq.deleteCalls, beforeBump, bumps)
	}
	if strings.Contains(out2, ": renumbered ") {
		t.Errorf("二次运行不应再重编号，stdout=%q", out2)
	}
}

// TestRunReindexEntry_DryRunRenumbers 要求 (a) dry-run 同样输出重编号行，且零写库。
// 覆盖 item 530/564/692 三种空洞形态（canonical 6/4/8 → holes 2/1/3）。
func TestRunReindexEntry_DryRunRenumbers(t *testing.T) {
	mq := &memQdrant{pts: map[string]memPt{
		"i530-1": {itemID: 530, text: "e1", idx: 1, cnt: 6},
		"i530-2": {itemID: 530, text: "e2", idx: 2, cnt: 6},
		"i530-4": {itemID: 530, text: "e4", idx: 4, cnt: 6},
		"i530-5": {itemID: 530, text: "e5", idx: 5, cnt: 6},
		"i564-0": {itemID: 564, text: "f0", idx: 0, cnt: 4},
		"i564-2": {itemID: 564, text: "f2", idx: 2, cnt: 4},
		"i564-3": {itemID: 564, text: "f3", idx: 3, cnt: 4},
		"i692-0": {itemID: 692, text: "g0", idx: 0, cnt: 8},
		"i692-1": {itemID: 692, text: "g1", idx: 1, cnt: 8},
		"i692-3": {itemID: 692, text: "g3", idx: 3, cnt: 8},
		"i692-5": {itemID: 692, text: "g5", idx: 5, cnt: 8},
		"i692-7": {itemID: 692, text: "g7", idx: 7, cnt: 8},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := runReindexEntry(context.Background(), "", "sre", srv.URL, "k", true, nil); err != nil {
			t.Fatalf("runReindexEntry dry: %v", err)
		}
	})
	for _, want := range []string{
		"item 530: renumbered 4 points (holes=2)",
		"item 564: renumbered 3 points (holes=1)",
		"item 692: renumbered 5 points (holes=3)",
		"renumbered_items=3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run 输出缺少 %q\nstdout=%s", want, out)
		}
	}
	if mq.deleteCalls != 0 || len(mq.payloadSet) != 0 {
		t.Errorf("dry-run 不应写库: deleteCalls=%d payloadSet=%v", mq.deleteCalls, mq.payloadSet)
	}
	t.Logf("dry-run stdout:\n%s", out)
}

// —— G04 DEDUP ——

func TestRunDedup_DryThenApply(t *testing.T) {
	mq := &memQdrant{pts: map[string]memPt{
		"p1": {itemID: 1, text: "A B\n  C", idx: 0, cnt: 1},
		"p2": {itemID: 2, text: "A B C", idx: 0, cnt: 1},
		"p3": {itemID: 3, text: "唯一文本", idx: 0, cnt: 1},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	// dry：只报告不删除。
	if err := runDedup(context.Background(), "sre", srv.URL, "k", true, nil); err != nil {
		t.Fatalf("runDedup dry: %v", err)
	}
	if mq.deleteCalls != 0 {
		t.Fatalf("dry 模式删除了 %d 次", mq.deleteCalls)
	}
	// apply：保留 (item_id,idx,id) 最小者 p1，删除 p2。
	bumps := 0
	if err := runDedup(context.Background(), "sre", srv.URL, "k", false, func(context.Context) error { bumps++; return nil }); err != nil {
		t.Fatalf("runDedup apply: %v", err)
	}
	if got := sortedCopy(mq.deleted); !reflect.DeepEqual(got, []string{"p2"}) {
		t.Errorf("去重删除 = %v, want [p2]（保留 item_id 最小者）", got)
	}
	if _, ok := mq.pts["p1"]; !ok {
		t.Error("保留点 p1 被误删")
	}
	if bumps != 1 {
		t.Errorf("bump = %d, want 1", bumps)
	}
}

// —— G01 SET_SOURCE 回填 ——

func TestRunSetSource_SetsPayloadAndBumps(t *testing.T) {
	mq := &memQdrant{pts: map[string]memPt{
		"p1": {itemID: 7, text: "条目一", idx: 0, cnt: 2},
		"p2": {itemID: 7, text: "条目二", idx: 1, cnt: 2},
		"p9": {itemID: 9, text: "无关", idx: 0, cnt: 1},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	path := writeJSON(t, []sourceItem{{ID: 7, SourceDoc: "k8s 部署手册.md"}}, "source.json")
	bumps := 0
	if err := runSetSource(context.Background(), path, "sre", srv.URL, "k", false, func(context.Context) error { bumps++; return nil }); err != nil {
		t.Fatalf("runSetSource: %v", err)
	}
	if got := sortedCopy(mq.payloadSet); !reflect.DeepEqual(got, []string{"p1", "p2"}) {
		t.Errorf("payload 更新点 = %v, want [p1 p2]", got)
	}
	for _, id := range []string{"p1", "p2"} {
		if mq.pts[id].sourceDoc != "k8s 部署手册.md" {
			t.Errorf("%s source_doc = %q, want %q", id, mq.pts[id].sourceDoc, "k8s 部署手册.md")
		}
	}
	if mq.pts["p9"].sourceDoc != "" {
		t.Error("无关 item 的 source_doc 被改写")
	}
	if bumps != 1 {
		t.Errorf("bump = %d, want 1", bumps)
	}
}

func TestRunSetSource_RejectsEmptyDoc(t *testing.T) {
	path := writeJSON(t, []sourceItem{{ID: 7, SourceDoc: "  "}}, "source.json")
	if err := runSetSource(context.Background(), path, "sre", "http://127.0.0.1:1", "k", true, nil); err == nil {
		t.Fatal("空 source_doc 应返回 error")
	}
}

// —— G49 注入条目 id 门禁 ——

func TestValidateInjectedIDs(t *testing.T) {
	cases := []struct {
		name      string
		ids       []int64
		allowReal bool
		wantErr   bool
	}{
		{"保留段放行", []int64{900000, 900001}, false, false},
		{"低位拒绝", []int64{900000, 955}, false, true},
		{"零 id 拒绝", []int64{0}, false, true},
		{"显式放行真实 id", []int64{955}, true, false},
	}
	for _, c := range cases {
		err := validateInjectedIDs(c.ids, c.allowReal)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr = %v", c.name, err, c.wantErr)
		}
	}
}

// —— 既有 RESPLIT 用例（回归）——

// TestRunResplit_RewritesMetaOnlyBlocksAndIsIdempotent 核心路径：
// 存量 3 点（正文 + 2 个纯元数据块）→ 重排为 2 点、无纯元数据块、每条 ≤600、
// 旧 3 点按差集删除、bump 一次；第二次运行零写入（幂等）。
func TestRunResplit_RewritesMetaOnlyBlocksAndIsIdempotent(t *testing.T) {
	const itemID = 7
	mq := &memQdrant{pts: map[string]memPt{
		"old-body":   {itemID: itemID, text: rep('甲', 590)},
		"old-meta1":  {itemID: itemID, text: "来源：某文档"},
		"old-meta2":  {itemID: itemID, text: "适用：生产环境"},
		"other-item": {itemID: 99, text: "无关 item 的既有条目，必须不动"},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	src := writeSRC(t, []resplitItem{{
		ID: itemID, Dir: "k8s-clean", TaskID: 42,
		Text: rep('甲', 590) + "\n\n来源：某文档\n\n适用：生产环境",
	}})

	bumps := 0
	deps := mq.deps()
	deps.Bump = func(context.Context) error { bumps++; return nil }

	if err := runResplit(context.Background(), src, "sre", srv.URL, "k", deps, false); err != nil {
		t.Fatalf("runResplit: %v", err)
	}
	if bumps != 1 {
		t.Errorf("bump 次数 = %d, want 1", bumps)
	}
	if mq.scrollCalls == 0 {
		t.Error("未读取存量点（scroll 未被调用）")
	}
	// 旧 3 点全部是差集（新 id 是确定性 UUID，与旧自由 id 不重合）
	wantDeleted := []string{"old-body", "old-meta1", "old-meta2"}
	if got := sortedCopy(mq.deleted); !reflect.DeepEqual(got, wantDeleted) {
		t.Errorf("删除集合 = %v, want %v", got, wantDeleted)
	}
	// 顺序契约：先 upsert 后 delete
	if len(mq.seq) != 2 || mq.seq[0] != "upsert" || mq.seq[1] != "delete" {
		t.Errorf("调用顺序 = %v, want [upsert delete]", mq.seq)
	}
	// 点数收敛 4→3（item 7: 3→2；无关 item 保留）
	if len(mq.pts) != 3 {
		t.Errorf("点数 = %d, want 3 (2 新 + 1 无关)", len(mq.pts))
	}
	if _, ok := mq.pts["other-item"]; !ok {
		t.Error("无关 item 的点被误删/误改（RESPLIT 不得扫全库）")
	}
	// 重排结果：无纯元数据块、每条 ≤600、正文非空
	itemPts := 0
	for id, p := range mq.pts {
		if p.itemID != itemID {
			continue
		}
		itemPts++
		if metaOnlyEntry(p.text) {
			t.Errorf("重排后仍有纯元数据块 %s: %q", id, p.text)
		}
		if n := len([]rune(p.text)); n > cleaner.MaxEntryLen {
			t.Errorf("条目 %s 长度 %d 超上限 %d", id, n, cleaner.MaxEntryLen)
		}
	}
	if itemPts != 2 {
		t.Errorf("item %d 重排后点数 = %d, want 2", itemID, itemPts)
	}

	// —— 第二次运行：幂等零写入 ——
	beforeUpsert, beforeDelete, beforeBumps := mq.upsertCalls, mq.deleteCalls, bumps
	beforeSparse, beforeEnsure := mq.sparseWrites, mq.ensureCalls
	if err := runResplit(context.Background(), src, "sre", srv.URL, "k", deps, false); err != nil {
		t.Fatalf("runResplit 二次: %v", err)
	}
	if mq.upsertCalls != beforeUpsert {
		t.Errorf("二次运行 upsert 次数 = %d, want %d（应零写入）", mq.upsertCalls, beforeUpsert)
	}
	if mq.deleteCalls != beforeDelete {
		t.Errorf("二次运行 delete 次数 = %d, want %d（差集应为空）", mq.deleteCalls, beforeDelete)
	}
	if mq.sparseWrites != beforeSparse {
		t.Errorf("二次运行稀疏写 = %d, want %d（幂等短路不得再写稀疏）", mq.sparseWrites, beforeSparse)
	}
	if mq.ensureCalls != beforeEnsure {
		t.Errorf("二次运行 EnsureSparse 次数 = %d, want %d（无变更不应触达）", mq.ensureCalls, beforeEnsure)
	}
	if bumps != beforeBumps {
		t.Errorf("二次运行 bump 次数 = %d, want %d（无变更不 bump）", bumps, beforeBumps)
	}
}

// TestRunResplit_WritesBM25SparseVectors 重排后每个新点都携带 bm25 稀疏向量：
//   - EnsureSparse 在 upsert 前调用（顺序契约，防 sparse 配置缺失 4xx）；
//   - 每个 upsert 点的 wire 形态为命名向量 map：默认稠密向量 "" + bm25 稀疏；
//   - bm25 indices/values 与对该点最终文本独立重算 bm25.SparseVector 完全一致
//     （证明稀疏编码的是重排后的新文本，而非旧文本）。
func TestRunResplit_WritesBM25SparseVectors(t *testing.T) {
	const itemID = 7
	mq := &memQdrant{pts: map[string]memPt{
		"old-body":  {itemID: itemID, text: rep('甲', 590)},
		"old-meta1": {itemID: itemID, text: "来源：某文档"},
		"old-meta2": {itemID: itemID, text: "适用：生产环境"},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	src := writeSRC(t, []resplitItem{{
		ID: itemID, Dir: "k8s-clean", TaskID: 42,
		Text: rep('甲', 590) + "\n\n来源：某文档\n\n适用：生产环境",
	}})
	deps := mq.deps()
	deps.Bump = func(context.Context) error { return nil }

	if err := runResplit(context.Background(), src, "sre", srv.URL, "k", deps, false); err != nil {
		t.Fatalf("runResplit: %v", err)
	}
	if mq.ensureCalls != 1 {
		t.Errorf("EnsureSparse 次数 = %d, want 1", mq.ensureCalls)
	}
	if mq.upsertBeforeEnsure {
		t.Error("Upsert 发生在 EnsureSparse 之前（顺序契约破坏）")
	}
	if len(mq.upsertWire) != 2 {
		t.Fatalf("upsert wire 点数 = %d, want 2", len(mq.upsertWire))
	}
	if mq.sparseWrites != 2 {
		t.Errorf("携带 bm25 稀疏的点写入 = %d, want 2", mq.sparseWrites)
	}
	for _, raw := range mq.upsertWire {
		var wp struct {
			Vector  map[string]json.RawMessage `json:"vector"`
			Payload struct {
				Text string `json:"text"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(raw), &wp); err != nil {
			t.Fatalf("解析 upsert wire 失败: %v\n%s", err, raw)
		}
		if _, ok := wp.Vector[""]; !ok {
			t.Errorf("点缺少默认稠密向量 \"\": %s", raw)
		}
		rawSparse, ok := wp.Vector[qdrant.SparseVectorName]
		if !ok {
			t.Fatalf("点缺少 bm25 命名稀疏向量: %s", raw)
		}
		var got qdrant.SparseVector
		if err := json.Unmarshal(rawSparse, &got); err != nil {
			t.Fatalf("解析 bm25 稀疏向量失败: %v", err)
		}
		wantIdx, wantVals := bm25.SparseVector(wp.Payload.Text)
		if len(wantIdx) == 0 {
			t.Fatalf("测试文本无词元，用例无意义: %q", wp.Payload.Text)
		}
		if !reflect.DeepEqual(got.Indices, wantIdx) || !reflect.DeepEqual(got.Values, wantVals) {
			t.Errorf("bm25 稀疏与新文本编码不一致:\n got idx=%v vals=%v\nwant idx=%v vals=%v",
				got.Indices, got.Values, wantIdx, wantVals)
		}
	}
}

// TestRunResplit_SetDifferenceKeepsNewPoints 差集删除不得误删确定性新 id
// （2026-09-07 两次误删事故根因）：旧点中已存在的新 id 必须保留，只删多余旧点。
func TestRunResplit_SetDifferenceKeepsNewPoints(t *testing.T) {
	const itemID = 7
	text := rep('甲', 590) + "\n来源：X" // SplitEntries → 单条（595 字）
	newID0 := cleaner.EntryPointID(itemID, "clean", 0)
	mq := &memQdrant{pts: map[string]memPt{
		newID0:  {itemID: itemID, text: text},
		"extra": {itemID: itemID, text: "旧残留点"},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	src := writeSRC(t, []resplitItem{{ID: itemID, Dir: "k8s-clean", TaskID: 42, Text: text}})
	deps := mq.deps()
	deps.Bump = func(context.Context) error { return nil }

	if err := runResplit(context.Background(), src, "sre", srv.URL, "k", deps, false); err != nil {
		t.Fatalf("runResplit: %v", err)
	}
	if got := sortedCopy(mq.deleted); !reflect.DeepEqual(got, []string{"extra"}) {
		t.Errorf("删除集合 = %v, want [extra]（新 id 不得被删）", got)
	}
	if p, ok := mq.pts[newID0]; !ok || p.text != text {
		t.Errorf("确定性新点被误删/改写: ok=%v pts=%+v", ok, mq.pts)
	}
}

// TestRunResplit_DryRunNoWrites DRY_RUN：只读存量点、不 embed/不 upsert/不 delete/
// 不 bump，库状态与版本不变。
func TestRunResplit_DryRunNoWrites(t *testing.T) {
	const itemID = 7
	mq := &memQdrant{pts: map[string]memPt{
		"old-body": {itemID: itemID, text: rep('甲', 590)},
		"old-meta": {itemID: itemID, text: "来源：某文档\n适用：生产环境"},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	src := writeSRC(t, []resplitItem{{
		ID: itemID, Dir: "k8s-clean", TaskID: 42,
		Text: rep('甲', 590) + "\n\n来源：某文档\n\n适用：生产环境",
	}})
	deps := mq.deps() // Bump 为 nil：dry 模式不应依赖 MySQL

	if err := runResplit(context.Background(), src, "sre", srv.URL, "k", deps, true); err != nil {
		t.Fatalf("runResplit dry: %v", err)
	}
	if mq.scrollCalls == 0 {
		t.Error("dry 模式应读取存量点以报告变更")
	}
	if mq.upsertCalls != 0 || mq.deleteCalls != 0 {
		t.Errorf("dry 模式写入: upsert=%d delete=%d, want 0/0", mq.upsertCalls, mq.deleteCalls)
	}
	if mq.ensureCalls != 0 {
		t.Errorf("dry 模式 EnsureSparse 次数 = %d, want 0（不写库、不触达 collection 配置）", mq.ensureCalls)
	}
	if mq.sparseWrites != 0 {
		t.Errorf("dry 模式稀疏写 = %d, want 0", mq.sparseWrites)
	}
	if len(mq.pts) != 2 {
		t.Errorf("dry 模式改变了库状态: %d 点", len(mq.pts))
	}
}

// TestRunResplit_FailedItemKeepsOldPoints 单 item 失败（embed 出错）时旧点原样保留、
// 且终态返回错误（非零退出告警）。
func TestRunResplit_FailedItemKeepsOldPoints(t *testing.T) {
	const itemID = 7
	mq := &memQdrant{pts: map[string]memPt{
		"old-body": {itemID: itemID, text: rep('甲', 590)},
		"old-meta": {itemID: itemID, text: "来源：某文档"},
	}}
	srv := httptest.NewServer(mq.handler())
	defer srv.Close()

	src := writeSRC(t, []resplitItem{{
		ID: itemID, Dir: "k8s-clean", TaskID: 42,
		Text: rep('甲', 590) + "\n\n来源：某文档",
	}})
	deps := mq.deps()
	deps.Embed = func(context.Context, []string) ([][]float32, error) {
		return nil, context.DeadlineExceeded
	}
	deps.Bump = func(context.Context) error { return nil }

	err := runResplit(context.Background(), src, "sre", srv.URL, "k", deps, false)
	if err == nil {
		t.Fatal("存在失败 item 应返回非 nil error")
	}
	if mq.upsertCalls != 0 || mq.deleteCalls != 0 {
		t.Errorf("失败 item 不应写库: upsert=%d delete=%d", mq.upsertCalls, mq.deleteCalls)
	}
	if len(mq.pts) != 2 {
		t.Errorf("失败 item 旧点应原样保留, 现 %d 点", len(mq.pts))
	}
}

// —— 纯函数表驱动（set-difference / 幂等比对）——

func TestStaleIDs(t *testing.T) {
	cases := []struct {
		name string
		old  []cleanPoint
		new  []string
		want []string
	}{
		{"全新无交集", []cleanPoint{{ID: "a"}, {ID: "b"}}, []string{"c"}, []string{"a", "b"}},
		{"新 id 保留", []cleanPoint{{ID: "a"}, {ID: "b"}}, []string{"a"}, []string{"b"}},
		{"无旧点", nil, []string{"a"}, nil},
		{"无新点", []cleanPoint{{ID: "a"}}, nil, []string{"a"}},
		{"完全一致零差集", []cleanPoint{{ID: "a"}}, []string{"a"}, nil},
	}
	for _, c := range cases {
		if got := staleIDs(c.old, c.new); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: staleIDs = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestStaleIDsFromIDs(t *testing.T) {
	cases := []struct {
		name string
		old  []string
		new  []string
		want []string
	}{
		{"全新无交集", []string{"a", "b"}, []string{"c"}, []string{"a", "b"}},
		{"新 id 保留", []string{"a", "b"}, []string{"a"}, []string{"b"}},
		{"完全一致零差集", []string{"a"}, []string{"a"}, nil},
	}
	for _, c := range cases {
		if got := staleIDsFromIDs(c.old, c.new); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: staleIDsFromIDs = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSameEntries(t *testing.T) {
	ids := []string{"a", "b"}
	cases := []struct {
		name    string
		old     []cleanPoint
		newIDs  []string
		entries []string
		want    bool
	}{
		{"完全一致", []cleanPoint{{ID: "a", Text: "x"}, {ID: "b", Text: "y"}}, ids, []string{"x", "y"}, true},
		{"文本不同", []cleanPoint{{ID: "a", Text: "x"}, {ID: "b", Text: "y"}}, ids, []string{"x", "z"}, false},
		{"id 不同", []cleanPoint{{ID: "a", Text: "x"}, {ID: "c", Text: "y"}}, ids, []string{"x", "y"}, false},
		{"条数不同", []cleanPoint{{ID: "a", Text: "x"}}, ids, []string{"x", "y"}, false},
	}
	for _, c := range cases {
		if got := sameEntries(c.old, c.newIDs, c.entries); got != c.want {
			t.Errorf("%s: sameEntries = %v, want %v", c.name, got, c.want)
		}
	}
}
