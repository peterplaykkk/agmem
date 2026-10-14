package store

// store2 测试：分页规范化（纯函数）+ 需 MySQL 的集成测试（DSN 守卫，缺库 skip）。
// 集成测试运行方式：AGMEM_TEST_MYSQL_DSN='user:pass@tcp(host:3306)/agmem_test?...' go test ./internal/store/

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestNormalizePage(t *testing.T) {
	cases := []struct {
		page, size, wp, ws int
	}{
		{0, 0, 1, 20},
		{-5, -1, 1, 20},
		{1, 10, 1, 10},
		{3, 20, 3, 20},
		{2, 501, 2, 500},
		{100, 500, 100, 500},
	}
	for _, c := range cases {
		p, s := NormalizePage(c.page, c.size)
		if p != c.wp || s != c.ws {
			t.Errorf("NormalizePage(%d,%d)=(%d,%d) want (%d,%d)", c.page, c.size, p, s, c.wp, c.ws)
		}
	}
}

func TestNullStringJSON(t *testing.T) {
	cases := []struct {
		in   NullString
		want string
	}{
		{NullString{}, "null"},
		{NewNullString(""), "null"},
		{NewNullString("abc"), `"abc"`},
	}
	for _, c := range cases {
		b, err := c.in.MarshalJSON()
		if err != nil || string(b) != c.want {
			t.Errorf("MarshalJSON(%+v)=%s err=%v want %s", c.in, b, err, c.want)
		}
	}
	// 反序列化
	var n NullString
	if err := n.UnmarshalJSON([]byte(`null`)); err != nil || n.Valid {
		t.Fatalf("null unmarshal: %+v %v", n, err)
	}
	if err := n.UnmarshalJSON([]byte(`"x"`)); err != nil || !n.Valid || n.String != "x" {
		t.Fatalf("string unmarshal: %+v %v", n, err)
	}
}

func TestTaskStatusTransitionsUnchanged(t *testing.T) {
	// 契约批重构防回归：终态不可逆
	if StatusCleaned.Valid(StatusFailed) || StatusFailed.Valid(StatusCleaning) {
		t.Fatal("terminal states must be irreversible")
	}
	if !StatusPending.Valid(StatusFailed) {
		t.Fatal("pending -> failed must be allowed")
	}
}

// finding 1：任务级状态推进决策（纯函数）
func TestDecideTaskStatus(t *testing.T) {
	cases := []struct {
		name                       string
		total, pending, done, fail int64
		want                       TaskStatus
	}{
		{"empty", 0, 0, 0, 0, StatusPending},
		{"all pending", 3, 3, 0, 0, StatusPending},
		{"in flight", 3, 2, 1, 0, StatusCleaning},
		{"in flight with failure", 3, 1, 1, 1, StatusCleaning},
		{"all done", 3, 0, 3, 0, TaskStatusCompleted},
		{"all failed", 2, 0, 0, 2, TaskStatusCompletedWithErrors},
		{"mixed terminal", 3, 0, 2, 1, TaskStatusCompletedWithErrors},
		{"count drift safety", 2, 0, 5, 0, TaskStatusCompleted},
	}
	for _, c := range cases {
		if got := DecideTaskStatus(c.total, c.pending, c.done, c.fail); got != c.want {
			t.Errorf("%s: DecideTaskStatus(%d,%d,%d,%d)=%s want %s",
				c.name, c.total, c.pending, c.done, c.fail, got, c.want)
		}
	}
}

func TestIntegration_RefreshTaskStatus(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	kbID, err := s.CreateKB(ctx, "it-task-status", "owner-x")
	if err != nil {
		t.Fatalf("create kb: %v", err)
	}
	dirID, err := s.CreateDir(ctx, kbID, "d1")
	if err != nil && err != ErrExists {
		t.Fatalf("create dir: %v", err)
	}
	if dirID == 0 {
		dirID = mustDirID(t, s, kbID, "d1")
	}
	// 入库去重后同库重复跑测试会命中历史文本 → 文本带纳秒后缀保证唯一
	uniq := strconv.FormatInt(time.Now().UnixNano(), 10)
	taskID, inserted, err := s.CreateTaskWithItems(ctx, kbID, dirID, []string{"a-" + uniq, "b-" + uniq}, nil)
	if err != nil || inserted != 2 {
		t.Fatalf("create task: err=%v inserted=%d", err, inserted)
	}
	// 用 GetTask 取本任务条目（kb 级 pending 列表在重复运行下会混入历史 run 的条目）
	tk, err := s.GetTask(ctx, kbID, taskID)
	if err != nil || len(tk.Items) != 2 {
		t.Fatalf("get task: err=%v items=%d", err, len(tk.Items))
	}
	// 状态机前置（store 既有语义）：CompleteCleaned 仅接受 cleaning 态
	// （Valid: cleaning→cleaned）；Claim 系列为全局竞争领取无法定向，
	// 集成测试直接推进该条目状态（2026-09-08 集成验证首次暴露的既有测试缺陷，
	// 基线 1bb31080 在干净 MySQL 上同样失败，见交付说明）。
	if _, err := s.DB.ExecContext(ctx,
		"UPDATE task_items SET status=? WHERE id=?", StatusCleaning, tk.Items[0].ID); err != nil {
		t.Fatalf("advance to cleaning: %v", err)
	}
	if err := s.CompleteCleaned(ctx, tk.Items[0].ID, "clean-a", "p-a"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := s.FailItem(ctx, tk.Items[1].ID, tk.Items[1].Status, "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	st, err := s.RefreshTaskStatus(ctx, taskID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if st != TaskStatusCompletedWithErrors {
		t.Fatalf("want completed_with_errors, got %s", st)
	}
	tk, err = s.GetTask(ctx, kbID, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if tk.Status != TaskStatusCompletedWithErrors {
		t.Fatalf("task.status=%s want completed_with_errors", tk.Status)
	}
	// 幂等：重复刷新结果不变
	st2, _ := s.RefreshTaskStatus(ctx, taskID)
	if st2 != TaskStatusCompletedWithErrors {
		t.Fatalf("idempotent refresh: %s", st2)
	}
	// 全 cleaned → completed（条目先推进到 cleaning，见上方状态机前置说明）
	task2, _, err := s.CreateTaskWithItems(ctx, kbID, dirID, []string{"x-" + uniq, "y-" + uniq}, nil)
	if err != nil {
		t.Fatalf("create task2: %v", err)
	}
	tk2, err := s.GetTask(ctx, kbID, task2)
	if err != nil || len(tk2.Items) != 2 {
		t.Fatalf("get task2: err=%v items=%d", err, len(tk2.Items))
	}
	for _, it := range tk2.Items {
		if _, err := s.DB.ExecContext(ctx,
			"UPDATE task_items SET status=? WHERE id=?", StatusCleaning, it.ID); err != nil {
			t.Fatalf("advance2 to cleaning: %v", err)
		}
		if err := s.CompleteCleaned(ctx, it.ID, "c", "p"); err != nil {
			t.Fatalf("complete2: %v", err)
		}
	}
	if st, _ := s.RefreshTaskStatus(ctx, task2); st != TaskStatusCompleted {
		t.Fatalf("want completed, got %s", st)
	}
}

// —— 以下为需 MySQL 的集成测试（本地缺库自动 skip）——

func testDBStore(t *testing.T) *Store {
	t.Helper()
	dsn := envOrSkip(t, "AGMEM_TEST_MYSQL_DSN")
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Skipf("MySQL 不可用（AGMEM_TEST_MYSQL_DSN），跳过集成测试: %v", err)
	}
	t.Cleanup(func() { s.DB.Close() })
	return s
}

func TestIntegration_PaginationBoundaries(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	kbID, err := s.CreateKB(ctx, "it-pg", "owner-x")
	if err != nil {
		t.Fatalf("create kb: %v", err)
	}
	dirID, err := s.CreateDir(ctx, kbID, "d1")
	if err != nil && err != ErrExists {
		t.Fatalf("create dir: %v", err)
	}
	if dirID == 0 {
		dirID = mustDirID(t, s, kbID, "d1")
	}
	uniq := strconv.FormatInt(time.Now().UnixNano(), 10)
	taskID, _, err := s.CreateTaskWithItems(ctx, kbID, dirID,
		[]string{"t1-" + uniq, "t2-" + uniq, "t3-" + uniq, "t4-" + uniq, "t5-" + uniq}, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	p1, err := s.ListItemsByStatusPage(ctx, kbID, StatusPending, 1, 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(p1.Items) != 2 || p1.Total < 5 {
		t.Fatalf("page1: got %d items total=%d", len(p1.Items), p1.Total)
	}
	p2, _ := s.ListItemsByStatusPage(ctx, kbID, StatusPending, 2, 2)
	if len(p2.Items) != 2 {
		t.Fatalf("page2: %d items", len(p2.Items))
	}
	if p1.Items[0].ID == p2.Items[0].ID {
		t.Fatal("page1 and page2 must not overlap")
	}
	// 边界：页超界 → 空列表不报错
	p99, _ := s.ListItemsByStatusPage(ctx, kbID, StatusPending, 99, 2)
	if len(p99.Items) != 0 {
		t.Fatalf("page99 should be empty, got %d", len(p99.Items))
	}
	// 参数规范化
	pn, _ := s.ListItemsByStatusPage(ctx, kbID, StatusPending, -1, 0)
	if pn.Page != 1 || pn.PageSize != 20 {
		t.Fatalf("normalize: page=%d size=%d", pn.Page, pn.PageSize)
	}
	_ = taskID
}

func TestIntegration_PermissionOwnerVsReader(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	kbID, err := s.CreateKB(ctx, "it-perm", "owner-alice")
	if err != nil {
		t.Fatalf("create kb: %v", err)
	}
	ok, err := s.HasReadAccess(ctx, kbID, "owner-alice", false)
	if err != nil || !ok {
		t.Fatalf("owner must have access: ok=%v err=%v", ok, err)
	}
	ok, _ = s.HasReadAccess(ctx, kbID, "stranger", false)
	if ok {
		t.Fatal("stranger must NOT have access")
	}
	if _, err := s.AddReader(ctx, kbID, "bob"); err != nil {
		t.Fatalf("add reader: %v", err)
	}
	ok, _ = s.HasReadAccess(ctx, kbID, "bob", false)
	if !ok {
		t.Fatal("reader bob must have access")
	}
	// machine token 全量
	ok, _ = s.HasReadAccess(ctx, kbID, "", true)
	if !ok {
		t.Fatal("machine must have access")
	}
	// 列表：owner+reader 可见，stranger 不可见
	l1, _ := s.ListKBs(ctx, "owner-alice")
	if !containsKB(l1, "it-perm") {
		t.Fatal("owner must see kb in list")
	}
	l2, _ := s.ListKBs(ctx, "bob")
	if !containsKB(l2, "it-perm") {
		t.Fatal("reader must see kb in list")
	}
	l3, _ := s.ListKBs(ctx, "stranger")
	if containsKB(l3, "it-perm") {
		t.Fatal("stranger must not see kb")
	}
}

func containsKB(kbs []KB, name string) bool {
	for _, k := range kbs {
		if k.Name == name {
			return true
		}
	}
	return false
}

func mustDirID(t *testing.T, s *Store, kbID int64, name string) int64 {
	t.Helper()
	d, err := s.GetKBDirByName(context.Background(), kbID, name)
	if err != nil {
		t.Fatalf("dir lookup: %v", err)
	}
	return d.ID
}

// 入库去重（2026-09-08）：original_text 已存在（任意状态）→ 跳过不建条目；
// 同批内重复同样跳过；全部重复 → ErrAllDuplicates 且不落空任务。
func TestIntegration_CreateTaskDedup(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	kbID, err := s.CreateKB(ctx, "it-dedup", "owner-x")
	if err != nil {
		t.Fatalf("create kb: %v", err)
	}
	dirID, err := s.CreateDir(ctx, kbID, "d1")
	if err != nil && err != ErrExists {
		t.Fatalf("create dir: %v", err)
	}
	if dirID == 0 {
		dirID = mustDirID(t, s, kbID, "d1")
	}
	// 文本带纳秒后缀：同库重复跑测试不触发与历史 run 的去重
	uniq := strconv.FormatInt(time.Now().UnixNano(), 10)
	t1, t2 := "dup-"+uniq+"-1", "dup-"+uniq+"-2"

	// 首次入库：2 条新文本 + 1 条同批重复 → 只入库 2 条
	taskID, inserted, err := s.CreateTaskWithItems(ctx, kbID, dirID, []string{t1, t2, t1}, nil)
	if err != nil || inserted != 2 {
		t.Fatalf("first push: err=%v inserted=%d (want 2, in-batch dup skipped)", err, inserted)
	}
	items, err := s.ListItemsByStatusPage(ctx, kbID, StatusPending, 1, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	n := 0
	for _, it := range items.Items {
		if it.TaskID == taskID {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("task %d must have exactly 2 items, got %d", taskID, n)
	}

	// 再次提交全部相同内容 → ErrAllDuplicates，不落空任务
	_, _, err = s.CreateTaskWithItems(ctx, kbID, dirID, []string{t1, t2}, nil)
	if err != ErrAllDuplicates {
		t.Fatalf("all-duplicate push: want ErrAllDuplicates, got %v", err)
	}

	// 部分重复：1 旧 + 1 新 → 只入库新的
	fresh := "fresh-" + uniq
	taskID2, inserted2, err := s.CreateTaskWithItems(ctx, kbID, dirID, []string{t1, fresh}, nil)
	if err != nil || inserted2 != 1 {
		t.Fatalf("partial dup push: err=%v inserted=%d (want 1)", err, inserted2)
	}
	items2, _ := s.ListItemsByStatusPage(ctx, kbID, StatusPending, 1, 10)
	for _, it := range items2.Items {
		if it.TaskID == taskID2 && it.OriginalText == t1 {
			t.Fatalf("duplicate text %q must not create a second item", t1)
		}
	}

	// 去重与状态无关：t1 所在任务条目置 failed 后，同文本依然被跳过（任意状态算已存在）
	items3, _ := s.ListItemsByStatusPage(ctx, kbID, StatusPending, 1, 10)
	for _, it := range items3.Items {
		if it.TaskID == taskID2 && it.OriginalText == fresh {
			if err := s.FailItem(ctx, it.ID, StatusPending, "boom"); err != nil {
				t.Fatalf("fail: %v", err)
			}
		}
	}
	_, inserted3, err := s.CreateTaskWithItems(ctx, kbID, dirID, []string{fresh}, nil)
	if err != ErrAllDuplicates {
		t.Fatalf("failed-status text must still count as existing: err=%v inserted=%d", err, inserted3)
	}
}

// TestIntegration_CreateTaskNoDedup：wiki 同步专用的不去重入队变体。
//
// 核心断言：库内**已有**同文本时，NoDedup 仍建新条目（同步方自己负责删旧版本）。
// 若这里退了（比如误用去重变体），变更文档里未改动的段会被跳过、随后旧条目被删，
// 内容凭空丢失——这是本变体存在的唯一理由。
func TestIntegration_CreateTaskNoDedup(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	kbID, err := s.CreateKB(ctx, "it-nodedup", "owner-x")
	if err != nil {
		t.Fatalf("create kb: %v", err)
	}
	dirID, err := s.CreateDir(ctx, kbID, "d1")
	if err != nil && err != ErrExists {
		t.Fatalf("create dir: %v", err)
	}
	if dirID == 0 {
		dirID = mustDirID(t, s, kbID, "d1")
	}
	uniq := strconv.FormatInt(time.Now().UnixNano(), 10)
	text := "nodedup-" + uniq

	// 先常规入队一次（建条目）
	if _, inserted, err := s.CreateTaskWithItems(ctx, kbID, dirID, []string{text}, []string{"doc"}); err != nil || inserted != 1 {
		t.Fatalf("first push: err=%v inserted=%d", err, inserted)
	}
	// 常规变体：同文本 → ErrAllDuplicates（既有语义不变）
	if _, _, err := s.CreateTaskWithItems(ctx, kbID, dirID, []string{text}, []string{"doc"}); err != ErrAllDuplicates {
		t.Fatalf("dedup variant must still return ErrAllDuplicates, got %v", err)
	}
	// NoDedup 变体：同文本 → 仍建新条目
	taskID2, inserted2, err := s.CreateTaskWithItemsNoDedup(ctx, kbID, dirID, []string{text}, []string{"doc"})
	if err != nil || inserted2 != 1 {
		t.Fatalf("no-dedup variant: err=%v inserted=%d (want 1)", err, inserted2)
	}
	items, err := s.ListItemsByStatusPage(ctx, kbID, StatusPending, 1, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	n := 0
	for _, it := range items.Items {
		if it.OriginalText == text {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("no-dedup 后同文本应有 2 条（新旧各一），got %d", n)
	}
	// 同批内重复仍然跳过（那是同一份内容传两遍，不是新旧版本）
	dupInBatch := "batchdup-" + uniq
	_, inserted3, err := s.CreateTaskWithItemsNoDedup(ctx, kbID, dirID,
		[]string{dupInBatch, dupInBatch}, []string{"a", "b"})
	if err != nil || inserted3 != 1 {
		t.Fatalf("no-dedup must still drop in-batch duplicates: err=%v inserted=%d (want 1)", err, inserted3)
	}
	_ = taskID2
}
