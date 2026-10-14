package store

// knowledge_test.go：KnowledgeEntries（S1 前缀数据源）集成测试。需 MySQL，
// AGMEM_TEST_MYSQL_DSN 守卫，缺库 skip（与 store2_test.go / store3_test.go 同约定）。
//
// 运行：AGMEM_TEST_MYSQL_DSN='user:pass@tcp(host:3306)/agmem_test?...' go test ./internal/store/

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestIntegration_KnowledgeEntries(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	uniq := strconv.FormatInt(time.Now().UnixNano(), 10)

	kbID, err := s.CreateKB(ctx, "it-know-"+uniq, "owner-know")
	if err != nil {
		t.Fatalf("create kb: %v", err)
	}
	if _, err := s.CreateDir(ctx, kbID, "k8s"); err != nil && err != ErrExists {
		t.Fatalf("create dir: %v", err)
	}
	dirID := mustDirID(t, s, kbID, "k8s")

	texts := []string{"know-a-" + uniq, "know-b-" + uniq, "know-c-" + uniq}
	names := []string{"doc-a.md", "doc-b.md", "doc-c.md"}
	taskID, n, err := s.CreateTaskWithItems(ctx, kbID, dirID, texts, names)
	if err != nil || n != 3 {
		t.Fatalf("create task: n=%d err=%v", n, err)
	}
	task, err := s.GetTask(ctx, kbID, taskID)
	if err != nil || len(task.Items) != 3 {
		t.Fatalf("get task: %v n=%d", err, len(task.Items))
	}
	ids := []int64{task.Items[0].ID, task.Items[1].ID, task.Items[2].ID}

	// 第 1、3 条置 cleaned 且 clean_text 非空；第 2 条置 cleaned 但 clean_text 为空
	// （必须先出现，验证被排除）；另留一条 pending（不置 cleaned，也应被排除）。
	if _, err := s.DB.ExecContext(ctx,
		"UPDATE task_items SET status='cleaned', clean_text=? WHERE id=?", "条目一-"+uniq, ids[0]); err != nil {
		t.Fatalf("update 1: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx,
		"UPDATE task_items SET status='cleaned', clean_text='' WHERE id=?", ids[1]); err != nil {
		t.Fatalf("update 2: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx,
		"UPDATE task_items SET status='cleaned', clean_text=? WHERE id=?", "条目三-"+uniq, ids[2]); err != nil {
		t.Fatalf("update 3: %v", err)
	}

	// 另一个 kb 的 cleaned 条目不得混入。
	otherKB, err := s.CreateKB(ctx, "it-know-other-"+uniq, "owner-know")
	if err != nil {
		t.Fatalf("create other kb: %v", err)
	}
	if _, err := s.CreateDir(ctx, otherKB, "k8s"); err != nil && err != ErrExists {
		t.Fatalf("dir other: %v", err)
	}
	otherDir := mustDirID(t, s, otherKB, "k8s")
	otherTask, _, err := s.CreateTaskWithItems(ctx, otherKB, otherDir, []string{"other-" + uniq}, []string{"other.md"})
	if err != nil {
		t.Fatalf("other task: %v", err)
	}
	otherItems, err := s.GetTask(ctx, otherKB, otherTask)
	if err != nil || len(otherItems.Items) != 1 {
		t.Fatalf("other get task: %v n=%d", err, len(otherItems.Items))
	}
	if _, err := s.DB.ExecContext(ctx,
		"UPDATE task_items SET status='cleaned', clean_text=? WHERE id=?",
		"别库条目-"+uniq, otherItems.Items[0].ID); err != nil {
		t.Fatalf("update other: %v", err)
	}

	rows, err := s.KnowledgeEntries(ctx, kbID)
	if err != nil {
		t.Fatalf("knowledge entries: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2（只取 cleaned 且 clean_text 非空）: %+v", len(rows), rows)
	}
	// 按 id 升序（= 产出时间序），doc_name 走 task 内序号不变式。
	want := []KnowledgeEntryRow{
		{ItemID: ids[0], Dir: "k8s", DocName: "doc-a.md", Text: "条目一-" + uniq},
		{ItemID: ids[2], Dir: "k8s", DocName: "doc-c.md", Text: "条目三-" + uniq},
	}
	for i, w := range want {
		if rows[i] != w {
			t.Fatalf("row %d = %+v, want %+v", i, rows[i], w)
		}
	}

	// 无条目的 kb → 0 行且不报错。
	emptyKB, err := s.CreateKB(ctx, "it-know-empty-"+uniq, "owner-know")
	if err != nil {
		t.Fatalf("empty kb: %v", err)
	}
	if er, err := s.KnowledgeEntries(ctx, emptyKB); err != nil || len(er) != 0 {
		t.Fatalf("empty kb rows=%d err=%v", len(er), err)
	}
}
