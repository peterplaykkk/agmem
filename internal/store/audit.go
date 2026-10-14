package store

// audit.go：写操作审计（anti-tamper）。设计要点：
//
//   - 审计行持久化到 audit_log（migration 004），不再只依赖进程日志。
//   - 成功路径的审计写入与业务写在**同一 MySQL 事务**内提交：审计写失败 → 整事务
//     回滚 → 业务失败（不是「尽力而为」的旁路）。
//   - 失败路径（业务写本身失败）没有可提交的事务可挂靠，退化为「尽力而为」单独
//     写一条 result=fail 审计行；写失败仅记日志。这是同事务语义的必然例外：事务
//     已回滚，无法把失败审计行留在一个不存在的事务里。
//
// 接线方式：API 层用 WithAudit(ctx, entry) 把审计主体（actor/action/kb/对象/IP）挂到
// context；store 写方法检测到审计上下文后，把「业务写 + 审计写」包进单事务。未挂
// 审计上下文时（worker/cleaner/migrate 等内部写、测试 fake）走既有路径不变。

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// AuditEntry 一条审计记录的业务主体（result 由 store 按实际结果填，不在本结构）。
type AuditEntry struct {
	ActorSub  string // 操作主体（OIDC sub / "machine" / "client-readonly"）
	ActorRole string // machine | owner | ... （来源 IP 见 SourceIP）
	Action    string // create_kb | create_dir | upload_docs | delete_item | retry_item | add_reader | remove_reader
	KB        string // 目标知识库名（无则空）
	ObjectID  string // 目标对象 id（kb 名 / 目录名 / item id / reader sub）
	SourceIP  string // 请求来源 IP
}

type auditCtxKey struct{}

// WithAudit 把审计主体挂到 context；store 写方法据此决定是否走「业务+审计」单事务路径。
func WithAudit(ctx context.Context, e AuditEntry) context.Context {
	return context.WithValue(ctx, auditCtxKey{}, e)
}

// auditEntryFrom 取审计主体；未挂载返回 (zero, false)。
func auditEntryFrom(ctx context.Context) (AuditEntry, bool) {
	e, ok := ctx.Value(auditCtxKey{}).(AuditEntry)
	return e, ok
}

// insertAuditTx 在给定事务内写一条审计行（result 由调用方判定）。
func insertAuditTx(ctx context.Context, tx *sql.Tx, e AuditEntry, result, detail string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (actor_sub, actor_role, action, kb, object_id, result, source_ip, detail)
		 VALUES (?,?,?,?,?,?,?,?)`,
		e.ActorSub, e.ActorRole, e.Action, e.KB, e.ObjectID, result, e.SourceIP, detail)
	if err != nil {
		return fmt.Errorf("store: insert audit: %w", err)
	}
	return nil
}

// auditFailBestEffort 业务写失败后尽力补记一条 result=fail 审计行（事务已回滚，
// 无法同事务）；写失败仅记日志，不改变业务错误语义。
func (s *Store) auditFailBestEffort(ctx context.Context, e AuditEntry, detail string) {
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO audit_log (actor_sub, actor_role, action, kb, object_id, result, source_ip, detail)
		 VALUES (?,?,?,?,?,?,?,?)`,
		e.ActorSub, e.ActorRole, e.Action, e.KB, e.ObjectID, "fail", e.SourceIP, detail); err != nil {
		log.Printf("store: audit(fail) write error action=%s object=%s: %v", e.Action, e.ObjectID, err)
	}
}

// withAuditedTx 在单个事务内执行 fn（业务写）并写入 result=ok 审计行，返回值语义：
//
//   - fn 业务失败（berr != nil）：回滚事务，尽力补记 result=fail 审计行，返回 berr；
//   - 审计写失败：回滚整事务（业务未提交）并返回审计错误——「写失败即业务失败」；
//   - 提交成功：返回 fn 的结果。
//
// detail 是 fn 附带的业务细节（如幂等重放标记）；出错时它作为 fail 审计的 detail。
// 该函数对 SQL 方言无假设（仅用 BeginTx/Exec/Commit），故可在 sqlite 上直接单测。
func withAuditedTx[T any](ctx context.Context, s *Store, e AuditEntry, fn func(tx *sql.Tx) (T, string, error)) (T, error) {
	var zero T
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return zero, fmt.Errorf("store: audit tx begin: %w", err)
	}
	defer tx.Rollback() // 提交后为 no-op；任何 early return 都会回滚
	res, detail, berr := fn(tx)
	if berr != nil {
		_ = tx.Rollback()
		s.auditFailBestEffort(ctx, e, detail)
		return zero, berr
	}
	if err := insertAuditTx(ctx, tx, e, "ok", detail); err != nil {
		return zero, err // 审计失败 → 业务回滚，业务失败
	}
	if err := tx.Commit(); err != nil {
		return zero, fmt.Errorf("store: audit tx commit: %w", err)
	}
	return res, nil
}
