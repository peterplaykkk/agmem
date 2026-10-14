package store

// store3 集成测试（UC-011 生命周期存储原语）：需 MySQL，AGMEM_TEST_MYSQL_DSN 守卫，
// 缺库 skip（与 store2_test.go 同约定）。
//
// 运行：AGMEM_TEST_MYSQL_DSN='user:pass@tcp(host:3306)/agmem_test?...' go test ./internal/store/

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// seedItem 建 kb+dir+任务并返回一个条目 id（status=pending）。
func seedItem(t *testing.T, s *Store, kbName, dirName string) (kbID, itemID int64) {
	t.Helper()
	ctx := context.Background()
	kbID, err := s.CreateKB(ctx, kbName, "owner-lifecycle")
	if err != nil {
		t.Fatalf("create kb: %v", err)
	}
	if _, err := s.CreateDir(ctx, kbID, dirName); err != nil && err != ErrExists {
		t.Fatalf("create dir: %v", err)
	}
	dirID := mustDirID(t, s, kbID, dirName)
	uniq := strconv.FormatInt(time.Now().UnixNano(), 10)
	_, _, err = s.CreateTaskWithItems(ctx, kbID, dirID, []string{"lifecycle-" + uniq}, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	items, err := s.ListItemsByStatusPage(ctx, kbID, StatusPending, 1, 1)
	if err != nil || len(items.Items) == 0 {
		t.Fatalf("list items: %v n=%d", err, len(items.Items))
	}
	return kbID, items.Items[0].ID
}

func TestIntegration_DeleteItem(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	kbName := "it-del-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	kbID, itemID := seedItem(t, s, kbName, "d1")

	if _, err := s.GetItem(ctx, kbID, itemID); err != nil {
		t.Fatalf("get item: %v", err)
	}
	// 跨 kb 归属校验：别的 kbID 查不到
	if _, err := s.GetItem(ctx, kbID+999999, itemID); err != ErrNotFound {
		t.Fatalf("cross-kb get must be ErrNotFound, got %v", err)
	}
	if err := s.DeleteItem(ctx, kbID, itemID); err != nil {
		t.Fatalf("delete item: %v", err)
	}
	if _, err := s.GetItem(ctx, kbID, itemID); err != ErrNotFound {
		t.Fatalf("deleted item must be gone, got %v", err)
	}
	// 二次删除 → ErrNotFound（API 层映射 404 幂等语义）
	if err := s.DeleteItem(ctx, kbID, itemID); err != ErrNotFound {
		t.Fatalf("second delete: want ErrNotFound, got %v", err)
	}
}

func TestIntegration_RequeueItem(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	kbName := "it-retry-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	kbID, itemID := seedItem(t, s, kbName, "d1")

	// 非 failed → ErrInvalidState + 当前状态
	st, err := s.RequeueItem(ctx, kbID, itemID)
	if err != ErrInvalidState || st != StatusPending {
		t.Fatalf("pending requeue: want (pending, ErrInvalidState), got (%s, %v)", st, err)
	}
	// 置 failed 后可重排
	if err := s.FailItem(ctx, itemID, StatusPending, "boom"); err != nil {
		t.Fatalf("fail item: %v", err)
	}
	st, err = s.RequeueItem(ctx, kbID, itemID)
	if err != nil || st != StatusPending {
		t.Fatalf("failed requeue: want (pending, nil), got (%s, %v)", st, err)
	}
	it, err := s.GetItem(ctx, kbID, itemID)
	if err != nil {
		t.Fatalf("get after requeue: %v", err)
	}
	if it.Status != StatusPending || it.Error.Valid {
		t.Fatalf("requeue must clear error, got status=%s error=%+v", it.Status, it.Error)
	}
	// 不存在
	if _, err := s.RequeueItem(ctx, kbID, itemID+999999); err != ErrNotFound {
		t.Fatalf("missing item requeue: want ErrNotFound, got %v", err)
	}
}

func TestIntegration_ReaderCRUD(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	kbName := "it-readers-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	kbID, _ := seedItem(t, s, kbName, "d1")

	added, err := s.AddReader(ctx, kbID, "bob")
	if err != nil || !added {
		t.Fatalf("first add: added=%v err=%v", added, err)
	}
	added, err = s.AddReader(ctx, kbID, "bob")
	if err != nil || added {
		t.Fatalf("re-add must be no-op: added=%v err=%v", added, err)
	}
	readers, err := s.ListReaders(ctx, kbID)
	if err != nil || len(readers) != 1 || readers[0] != "bob" {
		t.Fatalf("list readers: %v %v", readers, err)
	}
	removed, err := s.RemoveReader(ctx, kbID, "bob")
	if err != nil || !removed {
		t.Fatalf("remove: removed=%v err=%v", removed, err)
	}
	removed, err = s.RemoveReader(ctx, kbID, "bob")
	if err != nil || removed {
		t.Fatalf("re-remove must be no-op: removed=%v err=%v", removed, err)
	}
	readers, _ = s.ListReaders(ctx, kbID)
	if len(readers) != 0 {
		t.Fatalf("readers must be empty, got %v", readers)
	}
}

// SourceNameForItem（G01 来源行数据源）：task 内序号映射（含重复跳过）、
// 未知条目与缺失 documents 行一律空串不报错。
func TestIntegration_SourceNameForItem(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	uniq := strconv.FormatInt(time.Now().UnixNano(), 10)
	kbName := "it-src-" + uniq
	kbID, err := s.CreateKB(ctx, kbName, "owner-src")
	if err != nil {
		t.Fatalf("create kb: %v", err)
	}
	if _, err := s.CreateDir(ctx, kbID, "d1"); err != nil && err != ErrExists {
		t.Fatalf("create dir: %v", err)
	}
	dirID := mustDirID(t, s, kbID, "d1")

	texts := []string{"src-a-" + uniq, "src-b-" + uniq, "src-c-" + uniq}
	names := []string{"doc-a.md", "doc-b.md", "doc-c.md"}
	taskID, n, err := s.CreateTaskWithItems(ctx, kbID, dirID, texts, names)
	if err != nil || n != 3 {
		t.Fatalf("create task: n=%d err=%v", n, err)
	}
	task, err := s.GetTask(ctx, kbID, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if len(task.Items) != 3 {
		t.Fatalf("want 3 items, got %d", len(task.Items))
	}
	// GetTask 按 id 升序返回；序号 N ↔ documents 第 N 行
	for i, it := range task.Items {
		got, err := s.SourceNameForItem(ctx, it.ID)
		if err != nil {
			t.Fatalf("item %d: %v", it.ID, err)
		}
		if got != names[i] {
			t.Fatalf("item %d (ordinal %d): want %q, got %q", it.ID, i+1, names[i], got)
		}
	}

	// 未知条目 → 空串且不报错
	if got, err := s.SourceNameForItem(ctx, task.Items[len(task.Items)-1].ID+999999); err != nil || got != "" {
		t.Fatalf("missing item: want (\"\", nil), got (%q, %v)", got, err)
	}

	// 重复跳过：首条文本已存在（跳过）→ 只插第 2 条，序号映射仍须指向 doc-d.md
	taskID2, n2, err := s.CreateTaskWithItems(ctx, kbID, dirID,
		[]string{texts[0], "src-d-" + uniq}, []string{"doc-a.md", "doc-d.md"})
	if err != nil || n2 != 1 {
		t.Fatalf("dup-skip task: n=%d err=%v", n2, err)
	}
	task2, err := s.GetTask(ctx, kbID, taskID2)
	if err != nil || len(task2.Items) != 1 {
		t.Fatalf("task2 items: %d err=%v", len(task2.Items), err)
	}
	if got, err := s.SourceNameForItem(ctx, task2.Items[0].ID); err != nil || got != "doc-d.md" {
		t.Fatalf("dup-skip mapping: want doc-d.md, got (%q, %v)", got, err)
	}

	// documents 行缺失（历史/异常数据）→ 空串降级不报错
	if _, err := s.DB.ExecContext(ctx, "DELETE FROM documents WHERE task_id=?", taskID2); err != nil {
		t.Fatalf("delete documents: %v", err)
	}
	if got, err := s.SourceNameForItem(ctx, task2.Items[0].ID); err != nil || got != "" {
		t.Fatalf("no documents row: want (\"\", nil), got (%q, %v)", got, err)
	}

	// sourceNames 为空（seedItem 形态）→ source_name 空串
	_, seedItemID := seedItem(t, s, kbName+"-nil", "d1")
	if got, err := s.SourceNameForItem(ctx, seedItemID); err != nil || got != "" {
		t.Fatalf("nil sourceNames: want (\"\", nil), got (%q, %v)", got, err)
	}
}

// TestIntegration_ClaimItem 领取原语（claimItem 探测优化后回归，F5 2026-09-30）：
// 空表领取 → ErrNotFound 且无副作用；领取 pending → 状态推进 embedded；
// 并发竞争（状态已被改）→ ErrNotFound。
func TestIntegration_ClaimItem(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()

	// 空领取：先隔离同库先前测试遗留的 pending/embedded 行（测试串行执行，
	// 前序用例已断言完毕），确保探针真的落在空表上。
	if _, err := s.DB.ExecContext(ctx,
		"UPDATE task_items SET status=? WHERE status IN (?,?)", StatusFailed, StatusPending, StatusEmbedded); err != nil {
		t.Fatalf("quarantine leftovers: %v", err)
	}
	if _, err := s.claimItem(ctx, StatusPending, StatusEmbedded); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty claim: want ErrNotFound, got %v", err)
	}

	kbName := "it-claim-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	kbID, itemID := seedItem(t, s, kbName, "d1")

	it, err := s.claimItem(ctx, StatusPending, StatusEmbedded)
	if err != nil {
		t.Fatalf("claim pending: %v", err)
	}
	if it.ID != itemID || it.Status != StatusEmbedded {
		t.Fatalf("claimed item = (%d,%s), want (%d,%s)", it.ID, it.Status, itemID, StatusEmbedded)
	}
	// 状态已推进：再次领取同 kb 无活（seedItem 的条目已 embedded）。
	if _, err := s.claimItem(ctx, StatusPending, StatusEmbedded); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second claim: want ErrNotFound, got %v", err)
	}
	// DB 侧确认状态。
	var got string
	if err := s.DB.QueryRowContext(ctx,
		"SELECT status FROM task_items WHERE id=?", itemID).Scan(&got); err != nil || got != string(StatusEmbedded) {
		t.Fatalf("db status = (%q, %v), want %q", got, err, StatusEmbedded)
	}

	// 并发竞争：embedded 条目被别处推进 cleaning 后，按 embedded 领取 → ErrNotFound。
	if _, err := s.claimItem(ctx, StatusEmbedded, StatusCleaning); err != nil {
		t.Fatalf("claim embedded: %v", err)
	}
	if _, err := s.claimItem(ctx, StatusEmbedded, StatusCleaning); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stolen claim: want ErrNotFound, got %v", err)
	}
	_ = kbID
}
