package store

// 写操作审计的「同事务」语义单测：用 modernc.org/sqlite（纯 Go，内存/临时文件）
// 真跑事务，验证 withAuditedTx 的三条路径，并验证真实写方法（DeleteItem）在审计
// 上下文下把「业务写 + 审计写」放进同一事务。
//
// 该库只用 BeginTx/Exec/Commit 等方言无关能力，故 sqlite 可完整覆盖事务原语；
// 生产 MySQL 上各写方法调用同一 withAuditedTx。

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func newAuditTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, ddl := range []string{
		`CREATE TABLE biz (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE task_items (id INTEGER PRIMARY KEY, kb_id INTEGER)`,
		`CREATE TABLE audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			actor_sub TEXT, actor_role TEXT, action TEXT, kb TEXT,
			object_id TEXT, result TEXT, source_ip TEXT, detail TEXT)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	return &Store{DB: db}
}

func auditRowCount(t *testing.T, s *Store, where string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM audit_log "+where, args...).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

func bizRowCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM biz").Scan(&n); err != nil {
		t.Fatalf("count biz: %v", err)
	}
	return n
}

var testEntry = AuditEntry{
	ActorSub: "machine", ActorRole: "machine", Action: "unit_test",
	KB: "sre", ObjectID: "42", SourceIP: "192.0.2.1",
}

// 成功：业务写 + result=ok 审计行同事务提交。
func TestWithAuditedTx_SuccessCommitsBoth(t *testing.T) {
	s := newAuditTestStore(t)
	_, err := withAuditedTx(context.Background(), s, testEntry, func(tx *sql.Tx) (struct{}, string, error) {
		if _, err := tx.Exec(`INSERT INTO biz (id, v) VALUES (1, 'x')`); err != nil {
			return struct{}{}, "", err
		}
		return struct{}{}, "", nil
	})
	if err != nil {
		t.Fatalf("withAuditedTx: %v", err)
	}
	if bizRowCount(t, s) != 1 {
		t.Fatal("业务行必须提交")
	}
	if n := auditRowCount(t, s, "WHERE result='ok' AND action='unit_test'"); n != 1 {
		t.Fatalf("want 1 ok audit row, got %d", n)
	}
}

// 业务失败：事务回滚，业务行不落；尽力补记 result=fail 审计行（detail 携带原因）。
func TestWithAuditedTx_BusinessFailureRollsBackAndAuditsFail(t *testing.T) {
	s := newAuditTestStore(t)
	berr := errors.New("boom")
	_, err := withAuditedTx(context.Background(), s, testEntry, func(tx *sql.Tx) (struct{}, string, error) {
		if _, err := tx.Exec(`INSERT INTO biz (id, v) VALUES (2, 'y')`); err != nil {
			return struct{}{}, "", err
		}
		return struct{}{}, "not_found", berr
	})
	if !errors.Is(err, berr) {
		t.Fatalf("want business error, got %v", err)
	}
	if bizRowCount(t, s) != 0 {
		t.Fatal("业务失败必须回滚业务行")
	}
	if n := auditRowCount(t, s, "WHERE result='fail' AND detail='not_found'"); n != 1 {
		t.Fatalf("want 1 fail audit row, got %d", n)
	}
}

// 审计写失败：整事务回滚（写失败即业务失败）。
func TestWithAuditedTx_AuditFailureRollsBackBusiness(t *testing.T) {
	s := newAuditTestStore(t)
	if _, err := s.DB.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatalf("drop audit_log: %v", err)
	}
	_, err := withAuditedTx(context.Background(), s, testEntry, func(tx *sql.Tx) (struct{}, string, error) {
		if _, err := tx.Exec(`INSERT INTO biz (id, v) VALUES (3, 'z')`); err != nil {
			return struct{}{}, "", err
		}
		return struct{}{}, "", nil
	})
	if err == nil {
		t.Fatal("审计写失败时必须报错（业务不得提交）")
	}
	if bizRowCount(t, s) != 0 {
		t.Fatal("审计失败必须回滚业务行")
	}
}

// 真实写方法 DeleteItem：挂审计上下文后，删行与审计同事务；删成功 → 行删 + ok 审计。
func TestDeleteItem_WithAuditSameTx(t *testing.T) {
	s := newAuditTestStore(t)
	if _, err := s.DB.Exec(`INSERT INTO task_items (id, kb_id) VALUES (42, 1)`); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	ctx := WithAudit(context.Background(), AuditEntry{
		ActorSub: "machine", ActorRole: "machine", Action: "delete_item",
		KB: "sre", ObjectID: "42", SourceIP: "192.0.2.1",
	})
	if err := s.DeleteItem(ctx, 1, 42); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM task_items WHERE id=42`).Scan(&n); err != nil {
		t.Fatalf("count item: %v", err)
	}
	if n != 0 {
		t.Fatal("条目必须被删除")
	}
	if got := auditRowCount(t, s, "WHERE result='ok' AND action='delete_item' AND object_id='42'"); got != 1 {
		t.Fatalf("want 1 ok audit row, got %d", got)
	}
}

// DeleteItem 目标不存在：业务失败（ErrNotFound），事务回滚 + fail 审计。
func TestDeleteItem_WithAuditNotFoundRecordsFail(t *testing.T) {
	s := newAuditTestStore(t)
	ctx := WithAudit(context.Background(), AuditEntry{Action: "delete_item", ObjectID: "99"})
	if err := s.DeleteItem(ctx, 1, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if got := auditRowCount(t, s, "WHERE result='fail' AND action='delete_item' AND object_id='99'"); got != 1 {
		t.Fatalf("want 1 fail audit row, got %d", got)
	}
}
