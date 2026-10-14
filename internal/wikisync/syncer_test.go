package wikisync

// 同步器行为（httptest 假飞书 + 内存 fake KB + sqlite state）：
// 新增/变更/未变/文档被删、替换顺序（先入队后删旧）、单文档失败继续、
// 入队失败保留旧版本、非 docx 跳过、树遍历失败不动数据、调度 Tick 窗口。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// —— 内存 fake KB ——

type fakeKB struct {
	mu          sync.Mutex
	events      []string
	lastParts   []DocPart
	failEnqueue map[string]error
	dupTitles   map[string]bool
	delErr      map[int64]error
	nextTask    int64
	nextItem    int64
}

func newFakeKB() *fakeKB {
	return &fakeKB{failEnqueue: map[string]error{}, dupTitles: map[string]bool{}, delErr: map[int64]error{}}
}

// EnqueueDoc 记录收到的一次入队调用的段结构（段数/段名/总字符），并按段数返回等量条目 id。
// 事件格式：enqueue:<段名逗号分隔>|dir=<目录>|n=<段数>|len=<正文总字节数>。
func (f *fakeKB) EnqueueDoc(_ context.Context, dir string, parts []DocPart) (int64, []int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, len(parts))
	total := 0
	for i, p := range parts {
		names[i] = p.Name
		total += len(p.Text)
	}
	f.events = append(f.events, "enqueue:"+strings.Join(names, ",")+"|dir="+dir+
		"|n="+strconv.Itoa(len(parts))+"|len="+strconv.Itoa(total))
	f.lastParts = append([]DocPart(nil), parts...)
	if len(parts) == 0 {
		return 0, nil, false, errors.New("fakeKB: 空 parts")
	}
	// 以首段名作为失败/重复的注入键（整篇一段时即标题）。
	if err := f.failEnqueue[parts[0].Name]; err != nil {
		return 0, nil, false, err
	}
	if f.dupTitles[parts[0].Name] {
		return 0, nil, true, nil
	}
	f.nextTask++
	ids := make([]int64, 0, len(parts))
	for range parts {
		f.nextItem++
		ids = append(ids, f.nextItem)
	}
	return f.nextTask, ids, false, nil
}

func (f *fakeKB) DeleteItem(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "delete:"+strconv.FormatInt(id, 10))
	return f.delErr[id]
}

func (f *fakeKB) snapshotEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeKB) snapshotParts() []DocPart {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]DocPart(nil), f.lastParts...)
}

// —— 测试装配 ——

func docx(nodeToken, objToken, title string, editTime int64, hasChild bool) map[string]any {
	return map[string]any{
		"node_token": nodeToken, "obj_token": objToken, "obj_type": "docx",
		"title": title, "has_child": hasChild, "obj_edit_time": strconv.FormatInt(editTime, 10),
	}
}

func newTestSyncer(t *testing.T, f *fakeFeishu, kb KBUpdater, clock *time.Time) *Syncer {
	t.Helper()
	ts := f.server()
	t.Cleanup(ts.Close)
	return &Syncer{
		Client:    f.client(ts.URL),
		SpaceID:   "space1",
		State:     newStateStore(t),
		KB:        kb,
		Hours:     mustHours(t, "0-9,12-14,18-23"),
		Loc:       cst,
		HoursSpec: "0-9,12-14,18-23",
		Now:       func() time.Time { return *clock },
	}
}

func seedState(t *testing.T, s *Syncer, st State) {
	t.Helper()
	if err := s.State.Save(context.Background(), st); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

func mustState(t *testing.T, s *Syncer, token string) State {
	t.Helper()
	all, err := s.State.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	return all[token]
}

func fixedClock() *time.Time {
	tm := time.Date(2026, 9, 10, 12, 0, 0, 0, cst)
	return &tm
}

// —— 用例 ——

func TestSyncOnceNewDoc(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{docx("n1", "d1", "Doc A", 100, false)}
	f.content["d1"] = "正文内容"
	kb := newFakeKB()
	s := newTestSyncer(t, f, kb, fixedClock())

	s.SyncOnce(context.Background())

	ev := kb.snapshotEvents()
	if len(ev) != 1 || !strings.HasPrefix(ev[0], "enqueue:Doc A|dir=SRE 知识库|n=1|len=") {
		t.Fatalf("新增应入队一次且 dir=空间名: %v", ev)
	}
	st := mustState(t, s, "d1")
	if st.ObjEditTime != 100 || st.TaskID != 1 || len(st.ItemIDs) != 1 || st.ItemIDs[0] != 1 {
		t.Fatalf("state 记录错误: %+v", st)
	}
	snap := s.Snapshot()
	if snap.LastAdded != 1 || snap.LastChanged != 0 || snap.LastFailed != 0 {
		t.Fatalf("计数错误: %+v", snap)
	}
}

// —— 切段端到端（本次修复核心）——

// 大文档（>4000 rune）必须切成多段、一次入队，且段名带「（第N/M段）」。
// 这是「整篇当一个 text 导致 embed 失败」的直接回归断言。
func TestSyncOnceChunksLargeDoc(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{docx("n1", "d1", "华南理工", 100, false)}
	// 每行 9 个汉字 + 换行，重复 500 次 → 约 5000 rune，必须被切成 2 段以上。
	body := strings.Repeat("行内容比较长的内容\n", 500)
	f.content["d1"] = body
	kb := newFakeKB()
	s := newTestSyncer(t, f, kb, fixedClock())

	s.SyncOnce(context.Background())

	parts := kb.snapshotParts()
	if len(parts) < 2 {
		t.Fatalf(">%d rune 文档应切成多段，得到 %d 段", DocChunkRunes, len(parts))
	}
	// 段名：多段必带（第i+1/M段）；正文按序无损（去空白后与原文一致）。
	var joined strings.Builder
	for i, p := range parts {
		want := fmt.Sprintf("华南理工（第%d/%d段）", i+1, len(parts))
		if p.Name != want {
			t.Fatalf("第 %d 段名=%q want %q", i, p.Name, want)
		}
		if len([]rune(p.Text)) > DocChunkRunes {
			t.Fatalf("第 %d 段超过 %d rune: %d", i, DocChunkRunes, len([]rune(p.Text)))
		}
		joined.WriteString(p.Text)
	}
	if stripSpace(joined.String()) != stripSpace(body) {
		t.Fatalf("切段后内容丢失/乱序（去空白后不一致）")
	}
	// 一次入队（不是一段一个请求）。
	if ev := kb.snapshotEvents(); len(ev) != 1 || !strings.HasPrefix(ev[0], "enqueue:") {
		t.Fatalf("应一次入队全部段，得到 %v", ev)
	}
	// state 应记录与段数等量的条目 id（供后续变更时删旧）。
	st := mustState(t, s, "d1")
	if len(st.ItemIDs) != len(parts) {
		t.Fatalf("state item_ids=%d want %d（=段数）", len(st.ItemIDs), len(parts))
	}
	if snap := s.Snapshot(); snap.LastAdded != 1 || snap.LastFailed != 0 {
		t.Fatalf("计数错误: %+v", snap)
	}
}

func TestSyncOnceChangedReplacesOld(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{docx("n1", "d1", "Doc A", 200, false)}
	f.content["d1"] = "v2 正文"
	kb := newFakeKB()
	s := newTestSyncer(t, f, kb, fixedClock())
	seedState(t, s, State{DocToken: "d1", Title: "Doc A", Dir: "old", ObjEditTime: 100, TaskID: 5, ItemIDs: []int64{100, 101}})

	s.SyncOnce(context.Background())

	ev := kb.snapshotEvents()
	// 顺序保证：先入队新版本，成功后删旧。len 为字节数（"v2 正文" = 9）。
	want := []string{"enqueue:Doc A|dir=SRE 知识库|n=1|len=9", "delete:100", "delete:101"}
	if len(ev) != len(want) {
		t.Fatalf("事件序列=%v want %v", ev, want)
	}
	for i := range want {
		if ev[i] != want[i] {
			t.Fatalf("事件顺序错误 [%d]=%q want %q（完整 %v）", i, ev[i], want[i], ev)
		}
	}
	st := mustState(t, s, "d1")
	if st.ObjEditTime != 200 || len(st.ItemIDs) != 1 || st.ItemIDs[0] != 1 {
		t.Fatalf("变更后 state 应为新版本: %+v", st)
	}
	snap := s.Snapshot()
	if snap.LastChanged != 1 || snap.LastAdded != 0 || snap.LastFailed != 0 {
		t.Fatalf("计数错误: %+v", snap)
	}
}

func TestSyncOnceUnchanged(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{docx("n1", "d1", "Doc A", 200, false)}
	f.content["d1"] = "v2"
	kb := newFakeKB()
	s := newTestSyncer(t, f, kb, fixedClock())
	seedState(t, s, State{DocToken: "d1", Title: "Doc A", Dir: "ops", ObjEditTime: 200, TaskID: 5, ItemIDs: []int64{9}})

	s.SyncOnce(context.Background())

	if ev := kb.snapshotEvents(); len(ev) != 0 {
		t.Fatalf("未变文档不应有任何调用: %v", ev)
	}
	snap := s.Snapshot()
	if snap.LastAdded != 0 || snap.LastChanged != 0 || snap.LastFailed != 0 {
		t.Fatalf("未变计数应为 0: %+v", snap)
	}
	if st := mustState(t, s, "d1"); len(st.ItemIDs) != 1 || st.ItemIDs[0] != 9 {
		t.Fatalf("未变不应改 state: %+v", st)
	}
}

func TestSyncOnceRemovedDoc(t *testing.T) {
	f := newFakeFeishu() // 树为空
	kb := newFakeKB()
	s := newTestSyncer(t, f, kb, fixedClock())
	seedState(t, s, State{DocToken: "d1", Title: "Gone", Dir: "ops", ObjEditTime: 100, ItemIDs: []int64{7, 8}})

	s.SyncOnce(context.Background())

	ev := kb.snapshotEvents()
	if len(ev) != 2 || ev[0] != "delete:7" || ev[1] != "delete:8" {
		t.Fatalf("移除应删全部旧条目: %v", ev)
	}
	if all, _ := s.State.LoadAll(context.Background()); len(all) != 0 {
		t.Fatalf("移除后 state 应清空: %v", all)
	}
	if snap := s.Snapshot(); snap.LastRemoved != 1 {
		t.Fatalf("removed 计数错误: %+v", snap)
	}
}

func TestSyncOnceSingleDocFailureContinues(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{
		docx("n1", "d1", "Doc A", 100, false),
		docx("n2", "d2", "Doc B", 100, false),
	}
	f.badObj["d1"] = true // d1 正文读取失败
	f.content["d2"] = "ok"
	kb := newFakeKB()
	s := newTestSyncer(t, f, kb, fixedClock())

	s.SyncOnce(context.Background())

	ev := kb.snapshotEvents()
	if len(ev) != 1 || !strings.HasPrefix(ev[0], "enqueue:Doc B") {
		t.Fatalf("失败文档不应阻断后续文档: %v", ev)
	}
	snap := s.Snapshot()
	if snap.LastAdded != 1 || snap.LastFailed != 1 {
		t.Fatalf("计数错误: %+v", snap)
	}
	if st := mustState(t, s, "d2"); st.ObjEditTime != 100 {
		t.Fatalf("Doc B 应已入库: %+v", st)
	}
	if st := mustState(t, s, "d1"); st.DocToken != "" {
		t.Fatalf("失败文档不应写 state: %+v", st)
	}
}

func TestSyncOnceEnqueueFailureKeepsOld(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{docx("n1", "d1", "Doc A", 200, false)}
	f.content["d1"] = "v2"
	kb := newFakeKB()
	kb.failEnqueue["Doc A"] = errors.New("mysql down")
	s := newTestSyncer(t, f, kb, fixedClock())
	seedState(t, s, State{DocToken: "d1", Title: "Doc A", Dir: "ops", ObjEditTime: 100, TaskID: 5, ItemIDs: []int64{100}})

	s.SyncOnce(context.Background())

	for _, e := range kb.snapshotEvents() {
		if strings.HasPrefix(e, "delete:") {
			t.Fatalf("入队失败时绝不能删旧版本: %v", kb.snapshotEvents())
		}
	}
	st := mustState(t, s, "d1")
	if st.ObjEditTime != 100 || len(st.ItemIDs) != 1 || st.ItemIDs[0] != 100 {
		t.Fatalf("入队失败应保留旧 state: %+v", st)
	}
	if snap := s.Snapshot(); snap.LastFailed != 1 {
		t.Fatalf("failed 计数错误: %+v", snap)
	}
}

// 删旧失败：新版本仍入库、state 记为新版本、failed 计数 +1（取舍见 syncer.go ingest 注释）。
func TestSyncOnceDeleteOldFailureStillIngestsNew(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{docx("n1", "d1", "Doc A", 200, false)}
	f.content["d1"] = "v2"
	kb := newFakeKB()
	kb.delErr[100] = errors.New("qdrant down")
	s := newTestSyncer(t, f, kb, fixedClock())
	seedState(t, s, State{DocToken: "d1", Title: "Doc A", Dir: "ops", ObjEditTime: 100, TaskID: 5, ItemIDs: []int64{100}})

	s.SyncOnce(context.Background())

	st := mustState(t, s, "d1")
	if st.ObjEditTime != 200 || len(st.ItemIDs) != 1 || st.ItemIDs[0] != 1 {
		t.Fatalf("删旧失败也应记录新版本: %+v", st)
	}
	if snap := s.Snapshot(); snap.LastFailed != 1 || snap.LastChanged != 1 {
		t.Fatalf("计数错误: %+v", snap)
	}
}

func TestSyncOnceDupKeepsOld(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{docx("n1", "d1", "Doc A", 200, false)}
	f.content["d1"] = "same"
	kb := newFakeKB()
	kb.dupTitles["Doc A"] = true
	s := newTestSyncer(t, f, kb, fixedClock())
	seedState(t, s, State{DocToken: "d1", Title: "Doc A", Dir: "ops", ObjEditTime: 100, TaskID: 5, ItemIDs: []int64{100}})

	s.SyncOnce(context.Background())

	for _, e := range kb.snapshotEvents() {
		if strings.HasPrefix(e, "delete:") {
			t.Fatalf("重复正文不应删旧版本: %v", kb.snapshotEvents())
		}
	}
	st := mustState(t, s, "d1")
	if st.ObjEditTime != 200 || len(st.ItemIDs) != 1 || st.ItemIDs[0] != 100 {
		t.Fatalf("重复正文应保留旧 item_ids 并推进 edit_time: %+v", st)
	}
	if snap := s.Snapshot(); snap.LastSkipped != 1 {
		t.Fatalf("skipped 计数错误: %+v", snap)
	}
}

func TestSyncOnceTreeFailureLeavesDataUntouched(t *testing.T) {
	f := newFakeFeishu()
	f.failNodes = true
	kb := newFakeKB()
	s := newTestSyncer(t, f, kb, fixedClock())
	seedState(t, s, State{DocToken: "d1", Title: "Doc A", Dir: "ops", ObjEditTime: 100, ItemIDs: []int64{100}})

	s.SyncOnce(context.Background())

	if ev := kb.snapshotEvents(); len(ev) != 0 {
		t.Fatalf("树遍历失败不得改动数据: %v", ev)
	}
	if st := mustState(t, s, "d1"); len(st.ItemIDs) != 1 {
		t.Fatalf("树遍历失败不得改 state: %+v", st)
	}
	if snap := s.Snapshot(); snap.LastError == "" {
		t.Fatalf("应记录最近错误: %+v", snap)
	}
}

func TestWalkRecursesAndSkipsNonDocx(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{
		docx("root", "d0", "Root Doc", 1, true), // 顶层 docx 且带子节点
		{"node_token": "s1", "obj_token": "x1", "obj_type": "sheet", "title": "表", "has_child": false, "obj_edit_time": "1"},
	}
	f.tree["root"] = []map[string]any{docx("c1", "d1", "Child", 2, false)}
	f.content["d0"] = "a"
	f.content["d1"] = "b"
	kb := newFakeKB()
	s := newTestSyncer(t, f, kb, fixedClock())

	s.SyncOnce(context.Background())

	ev := kb.snapshotEvents()
	if len(ev) != 2 {
		t.Fatalf("应入队 2 个 docx（顶层+子），得到 %v", ev)
	}
	if !strings.Contains(ev[1], "enqueue:Child|dir=Root Doc|") {
		t.Fatalf("子节点 dir 应为父节点标题: %v", ev)
	}
	if snap := s.Snapshot(); snap.LastScanned != 3 {
		t.Fatalf("scanned 应含被跳过节点: %+v", snap)
	}
}

// walk 记录树结构：node_token / parent_node_token / depth（根=0、子=1、孙=2）/ node_url。
func TestWalkRecordsTreeStructure(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{docx("root", "d0", "Root", 1, true)}
	f.tree["root"] = []map[string]any{docx("c1", "d1", "Child", 2, true)}
	f.tree["c1"] = []map[string]any{docx("g1", "d2", "Grand", 3, false)}
	kb := newFakeKB()
	s := newTestSyncer(t, f, kb, fixedClock())

	tree, err := s.walk(context.Background())
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	want := []struct {
		obj, node, parent string
		depth             int
	}{
		{"d0", "root", "", 0},
		{"d1", "c1", "root", 1},
		{"d2", "g1", "c1", 2},
	}
	if len(tree.Docs) != len(want) {
		t.Fatalf("Docs=%d want %d: %+v", len(tree.Docs), len(want), tree.Docs)
	}
	for i, w := range want {
		d := tree.Docs[i]
		if d.ObjToken != w.obj || d.NodeToken != w.node || d.ParentNodeToken != w.parent || d.Depth != w.depth {
			t.Fatalf("Docs[%d]=%+v want obj=%s node=%s parent=%q depth=%d", i, d, w.obj, w.node, w.parent, w.depth)
		}
		if wantURL := "https://example.feishu.cn/wiki/" + w.node; d.NodeURL != wantURL {
			t.Fatalf("Docs[%d].NodeURL=%q want %q", i, d.NodeURL, wantURL)
		}
	}
}

// SyncOnce 把树结构字段落进 state，并在状态快照透出 tree_nodes / max_depth。
func TestSyncOncePersistsTreeFields(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{docx("root", "d0", "Root", 1, true)}
	f.tree["root"] = []map[string]any{docx("c1", "d1", "Child", 2, false)}
	f.content["d0"] = "a"
	f.content["d1"] = "b"
	kb := newFakeKB()
	s := newTestSyncer(t, f, kb, fixedClock())

	s.SyncOnce(context.Background())

	root := mustState(t, s, "d0")
	if root.NodeToken != "root" || root.ParentNodeToken != "" || root.Depth != 0 ||
		root.NodeURL != "https://example.feishu.cn/wiki/root" {
		t.Fatalf("根文档树字段错误: %+v", root)
	}
	child := mustState(t, s, "d1")
	if child.NodeToken != "c1" || child.ParentNodeToken != "root" || child.Depth != 1 ||
		child.NodeURL != "https://example.feishu.cn/wiki/c1" {
		t.Fatalf("子文档树字段错误: %+v", child)
	}
	snap := s.Snapshot()
	if snap.TreeNodes != 2 || snap.MaxDepth != 1 {
		t.Fatalf("状态快照 tree_nodes/max_depth 错误: tree_nodes=%d max_depth=%d", snap.TreeNodes, snap.MaxDepth)
	}
}

// 迁移期回填：未变文档（obj_edit_time 与树一致）也补写树结构列，但不入队/不删旧、
// 不动变更判定字段、不计入 added/changed/failed。
func TestSyncOnceBackfillsTreeFieldsForUnchanged(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{docx("n1", "d1", "Doc A", 200, false)}
	f.content["d1"] = "v2"
	kb := newFakeKB()
	s := newTestSyncer(t, f, kb, fixedClock())
	// 旧行：obj_edit_time 与树一致（未变），树字段为空（扩列前写入）。
	seedState(t, s, State{DocToken: "d1", Title: "Doc A", Dir: "ops", ObjEditTime: 200, TaskID: 5, ItemIDs: []int64{9}})

	s.SyncOnce(context.Background())

	if ev := kb.snapshotEvents(); len(ev) != 0 {
		t.Fatalf("未变文档不应入队/删旧，只回填树字段: %v", ev)
	}
	st := mustState(t, s, "d1")
	if st.NodeToken != "n1" || st.ParentNodeToken != "" || st.Depth != 0 ||
		st.NodeURL != "https://example.feishu.cn/wiki/n1" {
		t.Fatalf("未变文档树字段应被回填: %+v", st)
	}
	if st.ObjEditTime != 200 || st.TaskID != 5 || len(st.ItemIDs) != 1 || st.ItemIDs[0] != 9 {
		t.Fatalf("回填不得改动变更判定字段: %+v", st)
	}
	snap := s.Snapshot()
	if snap.LastAdded != 0 || snap.LastChanged != 0 || snap.LastFailed != 0 {
		t.Fatalf("回填不应计入 added/changed/failed: %+v", snap)
	}
}

// —— 调度窗口（Tick）——

func TestTickWindow(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{docx("n1", "d1", "Doc A", 100, false)}
	f.content["d1"] = "x"
	kb := newFakeKB()
	clock := fixedClock()
	s := newTestSyncer(t, f, kb, clock)
	ctx := context.Background()

	// 高峰窗口（10:00，落在 9-12 峰内）不跑
	*clock = time.Date(2026, 9, 10, 10, 0, 0, 0, cst)
	if s.Tick(ctx) {
		t.Fatal("高峰窗口不得执行")
	}
	if len(kb.snapshotEvents()) != 0 {
		t.Fatal("高峰窗口不得有任何调用")
	}

	// 非高峰（12:00，峰后第一槽）跑一次
	*clock = time.Date(2026, 9, 10, 12, 0, 0, 0, cst)
	if !s.Tick(ctx) {
		t.Fatal("非高峰窗口应执行")
	}
	// 同一 30 分钟槽内再 tick 不跑
	*clock = time.Date(2026, 9, 10, 12, 29, 0, 0, cst)
	if s.Tick(ctx) {
		t.Fatal("同一 30 分钟槽不得重复执行")
	}
	// 下一个槽（12:30）应再跑——这是「每 30 分钟一次」的核心断言
	*clock = time.Date(2026, 9, 10, 12, 30, 0, 0, cst)
	if !s.Tick(ctx) {
		t.Fatal("下一个 30 分钟槽应再次执行")
	}

	// 跨天：次日 00:05 应再跑（即便上一次是同日 12:30）
	*clock = time.Date(2026, 9, 11, 0, 5, 0, 0, cst)
	if !s.Tick(ctx) {
		t.Fatal("跨天应再次执行")
	}
}

// —— 配置一致性 ——

// NextRun 在 Snapshot 中体现（下次计划时间非空且落在允许小时）。
func TestSnapshotNextRunAt(t *testing.T) {
	f := newFakeFeishu()
	kb := newFakeKB()
	clock := fixedClock() // 12:00
	s := newTestSyncer(t, f, kb, clock)
	snap := s.Snapshot()
	if snap.NextRunAt == "" {
		t.Fatalf("NextRunAt 应为下一允许整点: %+v", snap)
	}
	next, err := time.Parse(time.RFC3339, snap.NextRunAt)
	if err != nil {
		t.Fatalf("NextRunAt 解析失败: %v", err)
	}
	if !s.Hours[next.In(cst).Hour()] {
		t.Fatalf("NextRunAt 落在非允许小时: %s", snap.NextRunAt)
	}
	if !next.After(*clock) {
		t.Fatalf("NextRunAt 应晚于当前: %s", snap.NextRunAt)
	}
}

// 「没有变更就不提取」的可观测证据：未变文档必须计入 LastUnchanged，
// 且扫描数应能被 未变+新增+变更+跳过+失败 解释（否则状态里看不出发生过什么）。
func TestSyncOnceUnchangedIsCounted(t *testing.T) {
	f := newFakeFeishu()
	f.tree[""] = []map[string]any{docx("n1", "d1", "Doc A", 100, false)}
	f.content["d1"] = "正文"
	kb := newFakeKB()
	clock := fixedClock()
	s := newTestSyncer(t, f, kb, clock)

	// 第一轮：新增
	if !s.Tick(context.Background()) {
		t.Fatal("首轮应执行")
	}
	if st := s.Snapshot(); st.LastAdded != 1 || st.LastUnchanged != 0 {
		t.Fatalf("首轮应 added=1 unchanged=0, got added=%d unchanged=%d", st.LastAdded, st.LastUnchanged)
	}

	// 第二轮：edit_time 未变 → 必须跳过且计入 unchanged
	*clock = clock.Add(30 * time.Minute)
	if !s.Tick(context.Background()) {
		t.Fatal("第二轮应执行")
	}
	st := s.Snapshot()
	if st.LastUnchanged != 1 {
		t.Fatalf("未变文档应计入 unchanged=1, got %d", st.LastUnchanged)
	}
	if st.LastAdded != 0 || st.LastChanged != 0 {
		t.Fatalf("未变不应产生 added/changed: added=%d changed=%d", st.LastAdded, st.LastChanged)
	}
	// 零提取：跳过的一轮不得对知识库产生任何写入事件（假 KB 记录全部事件）。
	// 首轮 1 次 enqueue；第二轮未变 → 事件数不变。
	if ev := kb.snapshotEvents(); len(ev) != 1 {
		t.Fatalf("未变轮不应产生新的入库事件, 事件序列=%v", ev)
	}
	if st.TotalUnchanged != 1 {
		t.Fatalf("累计 unchanged 应为 1, got %d", st.TotalUnchanged)
	}
}
