package store

// store3.go：知识生命周期（UC-011）元数据侧——条目删除、失败重试（failed→pending）、
// reader 增删的存储原语。qdrant 点位删除不在本层（由 api 编排：先删点位再删行，
// 避免「行已删但点位仍在」被 ask 继续命中）。

import (
	"context"
	"database/sql"
	"fmt"
)

// GetItem 按 id 取条目（kb 归属校验，防跨库误删）；不存在返回 ErrNotFound。
func (s *Store) GetItem(ctx context.Context, kbID, itemID int64) (*TaskItem, error) {
	var it TaskItem
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, task_id, kb_id, dir_id, status, original_text, clean_text, error, point_id, clean_point_id
		 FROM task_items WHERE id=? AND kb_id=?`, itemID, kbID).
		Scan(&it.ID, &it.TaskID, &it.KBID, &it.DirID, &it.Status, &it.OriginalText, &it.CleanText,
			&it.Error, &it.PointID, &it.CleanPointID)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get item %d: %w", itemID, err)
	}
	return &it, nil
}

// SourceNameForItem 与批量 sourceNamesForItems 已移至 knowledge.go（S1 前缀数据源
// 与 SourceNameForItem 共用同一套「task 内序号 ↔ documents 行」不变式实现）。

// DeleteItem 删除条目行（UC-011 A1 MySQL 侧）。不存在返回 ErrNotFound——
// API 层映射为 404（幂等语义：二次删除不静默 200）。
// 只删 task_items：documents 按 task 粒度登记（一条 doc 对应任务内多条 item），
// 删它会误伤同任务其余条目。
func (s *Store) DeleteItem(ctx context.Context, kbID, itemID int64) error {
	if e, ok := auditEntryFrom(ctx); ok {
		_, err := withAuditedTx(ctx, s, e, func(tx *sql.Tx) (struct{}, string, error) {
			res, err := tx.ExecContext(ctx, "DELETE FROM task_items WHERE id=? AND kb_id=?", itemID, kbID)
			if err != nil {
				return struct{}{}, "", fmt.Errorf("store: delete item %d: %w", itemID, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return struct{}{}, "not_found", ErrNotFound
			}
			return struct{}{}, "", nil
		})
		return err
	}
	res, err := s.DB.ExecContext(ctx, "DELETE FROM task_items WHERE id=? AND kb_id=?", itemID, kbID)
	if err != nil {
		return fmt.Errorf("store: delete item %d: %w", itemID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RequeueItem failed → pending（清空 error，worker 轮询会自动重跑）。
// 语义（UC-011 主流程 4：终败后手动 requeue）：
//   - 条目不存在 → ("", ErrNotFound)
//   - 存在但状态非 failed → (当前状态, ErrInvalidState)（API 映射 409，附当前状态）
//   - 成功 → (pending, nil)
//
// 行锁 + 条件更新保证与 worker 并发时不会把 cleaning/cleaned 打回 pending。
func (s *Store) RequeueItem(ctx context.Context, kbID, itemID int64) (TaskStatus, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("store: requeue item %d: %w", itemID, err)
	}
	defer tx.Rollback()
	e, audited := auditEntryFrom(ctx)
	var cur TaskStatus
	err = tx.QueryRowContext(ctx,
		"SELECT status FROM task_items WHERE id=? AND kb_id=? FOR UPDATE", itemID, kbID).Scan(&cur)
	if err == sql.ErrNoRows {
		if audited {
			_ = tx.Rollback()
			s.auditFailBestEffort(ctx, e, "not_found")
		}
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: requeue item %d: %w", itemID, err)
	}
	if cur != StatusFailed {
		if audited {
			_ = tx.Rollback()
			s.auditFailBestEffort(ctx, e, "invalid_state:"+string(cur))
		}
		return cur, ErrInvalidState
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE task_items SET status=?, error=NULL WHERE id=? AND kb_id=?", StatusPending, itemID, kbID); err != nil {
		return "", fmt.Errorf("store: requeue item %d: %w", itemID, err)
	}
	// 审计：与业务写同事务（插入失败 → 回滚，状态不改）。
	if audited {
		if err := insertAuditTx(ctx, tx, e, "ok", ""); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: requeue item %d: %w", itemID, err)
	}
	return StatusPending, nil
}
