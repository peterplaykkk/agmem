package store

// store2.go：列表/权限/任务创建/分页（后端契约批新增）。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// GetKBDirByName kb 内按名字取目录（不存在返回 ErrNotFound）。
func (s *Store) GetKBDirByName(ctx context.Context, kbID int64, name string) (*Dir, error) {
	var d Dir
	err := s.DB.QueryRowContext(ctx,
		"SELECT id, kb_id, name, created_at FROM directories WHERE kb_id=? AND name=?", kbID, name).
		Scan(&d.ID, &d.KBID, &d.Name, &d.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListKBs 返回 sub 有读权限（owner 或 reader）的知识库列表（含汇总）。
// sub 为空返回全部（机器 token/管理语义）。
func (s *Store) ListKBs(ctx context.Context, sub string) ([]KB, error) {
	var rows *sql.Rows
	var err error
	if sub == "" {
		rows, err = s.DB.QueryContext(ctx, `
			SELECT k.id, k.name, k.owner_sub,
			       (SELECT COUNT(*) FROM directories d WHERE d.kb_id=k.id) AS dir_count,
			       (SELECT COUNT(*) FROM task_items i WHERE i.kb_id=k.id) AS item_count,
			       COALESCE((SELECT version FROM kb_versions v WHERE v.kb_id=k.id), 0) AS ver,
			       COALESCE((SELECT MAX(id) FROM tasks t WHERE t.kb_id=k.id), 0) AS last_task,
			       COALESCE((SELECT status FROM tasks t WHERE t.id =
			          (SELECT MAX(id) FROM tasks t2 WHERE t2.kb_id=k.id)), '') AS last_status
			FROM knowledge_bases k ORDER BY k.id`)
	} else {
		rows, err = s.DB.QueryContext(ctx, `
			SELECT k.id, k.name, k.owner_sub,
			       (SELECT COUNT(*) FROM directories d WHERE d.kb_id=k.id),
			       (SELECT COUNT(*) FROM task_items i WHERE i.kb_id=k.id),
			       COALESCE((SELECT version FROM kb_versions v WHERE v.kb_id=k.id), 0),
			       COALESCE((SELECT MAX(id) FROM tasks t WHERE t.kb_id=k.id), 0),
			       COALESCE((SELECT status FROM tasks t WHERE t.id =
			          (SELECT MAX(id) FROM tasks t2 WHERE t2.kb_id=k.id)), '')
			FROM knowledge_bases k
			WHERE k.owner_sub=? OR k.id IN (SELECT kb_id FROM kb_readers WHERE sub=?)
			ORDER BY k.id`, sub, sub)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list kbs: %w", err)
	}
	defer rows.Close()
	var out []KB
	for rows.Next() {
		var k KB
		if err := rows.Scan(&k.ID, &k.Name, &k.OwnerSub, &k.DirCount, &k.ItemCount,
			&k.Version, &k.LastTaskID, &k.LastTaskSt); err != nil {
			return nil, err
		}
		if k.OwnerSub != "" && k.OwnerSub == sub {
			k.Role = "owner"
		} else {
			k.Role = "reader"
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// HasReadAccess 判定 sub（或机器 token）对 kb 是否有读权限。
// machine=true（agmem）放行全部；否则 owner/reader。
func (s *Store) HasReadAccess(ctx context.Context, kbID int64, sub string, machine bool) (bool, error) {
	if machine {
		return true, nil
	}
	if sub == "" {
		return false, nil
	}
	var n int
	err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM knowledge_bases k
		 WHERE k.id=? AND (k.owner_sub=? OR k.id IN (SELECT kb_id FROM kb_readers WHERE sub=?))`,
		kbID, sub, sub).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// AddReader 授予 sub 读权限；返回是否新插入（幂等：已存在返回 false, nil）。
// 返回 inserted 供管理端点判定「确有变更才 bump 版本」——幂等重放不得抖动缓存。
func (s *Store) AddReader(ctx context.Context, kbID int64, sub string) (bool, error) {
	if sub == "" {
		return false, fmt.Errorf("store: add reader: sub 为空")
	}
	if e, ok := auditEntryFrom(ctx); ok {
		return withAuditedTx(ctx, s, e, func(tx *sql.Tx) (bool, string, error) {
			res, err := tx.ExecContext(ctx, "INSERT IGNORE INTO kb_readers (kb_id, sub) VALUES (?, ?)", kbID, sub)
			if err != nil {
				return false, "", fmt.Errorf("store: add reader: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return false, "replay", nil // 幂等重放非业务失败
			}
			return true, "", nil
		})
	}
	res, err := s.DB.ExecContext(ctx, "INSERT IGNORE INTO kb_readers (kb_id, sub) VALUES (?, ?)", kbID, sub)
	if err != nil {
		return false, fmt.Errorf("store: add reader: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RemoveReader 撤销 sub 读权限；返回是否删除了行（幂等：不存在返回 false, nil）。
func (s *Store) RemoveReader(ctx context.Context, kbID int64, sub string) (bool, error) {
	if sub == "" {
		return false, fmt.Errorf("store: remove reader: sub 为空")
	}
	if e, ok := auditEntryFrom(ctx); ok {
		return withAuditedTx(ctx, s, e, func(tx *sql.Tx) (bool, string, error) {
			res, err := tx.ExecContext(ctx, "DELETE FROM kb_readers WHERE kb_id=? AND sub=?", kbID, sub)
			if err != nil {
				return false, "", fmt.Errorf("store: remove reader: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return false, "replay", nil
			}
			return true, "", nil
		})
	}
	res, err := s.DB.ExecContext(ctx, "DELETE FROM kb_readers WHERE kb_id=? AND sub=?", kbID, sub)
	if err != nil {
		return false, fmt.Errorf("store: remove reader: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListReaders 读者列表。
func (s *Store) ListReaders(ctx context.Context, kbID int64) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT sub FROM kb_readers WHERE kb_id=? ORDER BY sub", kbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sub string
		if err := rows.Scan(&sub); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// ListDirs kb 下 1 级目录列表。
func (s *Store) ListDirs(ctx context.Context, kbID int64) ([]Dir, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT id, kb_id, name, created_at FROM directories WHERE kb_id=? ORDER BY id", kbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dir
	for rows.Next() {
		var d Dir
		if err := rows.Scan(&d.ID, &d.KBID, &d.Name, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDirName 目录名（worker DirResolver 用）；不存在返回 ErrNotFound。
func (s *Store) GetDirName(ctx context.Context, dirID int64) (string, error) {
	var name string
	err := s.DB.QueryRowContext(ctx, "SELECT name FROM directories WHERE id=?", dirID).Scan(&name)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return name, err
}

// KBNameByID kb id → name（worker 定位 qdrant collection 用）。
func (s *Store) KBNameByID(ctx context.Context, kbID int64) (string, error) {
	var name string
	err := s.DB.QueryRowContext(ctx, "SELECT name FROM knowledge_bases WHERE id=?", kbID).Scan(&name)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return name, err
}

// CreateTaskWithItems 建任务并拆条目（POST /docs 用），返回 task id 与实际入库条数。
//
// 入库去重（2026-09-08 拍板）：original_text 已存在于同 kb（任意状态，含历史
// 任务与 failed）的文本跳过不建条目——同一文档重复 push 会产生完全重复的条目
// 并污染检索（线上 4 份重复 gitlab访问凭证 的根因）。同批内重复文本同样去重。
// schema 无唯一键（MEDIUMTEXT 无法直接建唯一索引），按拍板用「插入前 SELECT
// 查重」的简单实现；数据量 ≤100 万条内可接受，量级上涨后应加 original_sha256
// 生成列 + 索引（遗留项，见交付说明）。
//
// 全部文本均为重复时回滚并返回 ErrAllDuplicates（不落空任务）。
func (s *Store) CreateTaskWithItems(ctx context.Context, kbID, dirID int64, texts []string, sourceNames []string) (int64, int, error) {
	return s.createTaskWithItems(ctx, kbID, dirID, texts, sourceNames, true)
}

// CreateTaskWithItemsNoDedup 同 CreateTaskWithItems，但**不做入库去重**：同批重复
// 文本仍去重（那是同一份内容传两遍），但不与库内既有 original_text 比对。
//
// 为什么需要它（wiki 同步专用，2026-09-10）：同步方在文档变更时会**先入队新版本、
// 再删除上一版的全部条目**，自己就是该文档条目的唯一 owner。若这里仍按库内文本
// 去重，变更文档中「未改动的段」会被判重复而跳过不建条目，紧接着旧版本条目被删
// → 那些段的内容凭空丢失，且因 obj_edit_time 已推进不会在后续轮次补回。
// 走 CreateTaskWithItems 的常规 push 路径语义不变。
func (s *Store) CreateTaskWithItemsNoDedup(ctx context.Context, kbID, dirID int64, texts []string, sourceNames []string) (int64, int, error) {
	return s.createTaskWithItems(ctx, kbID, dirID, texts, sourceNames, false)
}

func (s *Store) createTaskWithItems(ctx context.Context, kbID, dirID int64, texts []string, sourceNames []string, dedup bool) (int64, int, error) {
	if len(texts) == 0 {
		return 0, 0, fmt.Errorf("store: create task: 空 texts")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "INSERT INTO tasks (kb_id, dir_id, status) VALUES (?,?,?)",
		kbID, dirID, StatusPending)
	if err != nil {
		return 0, 0, err
	}
	taskID, err := res.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	inserted := 0
	seen := make(map[string]bool, len(texts))
	for i, text := range texts {
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if seen[text] {
			continue // 同批内重复
		}
		seen[text] = true
		// 入库去重：同 kb 已存在同文本（任意状态）→ 跳过。无索引全表扫描，
		// 量小可接受（见函数注释遗留项）。dedup=false 时跳过该比对（见
		// CreateTaskWithItemsNoDedup 注释）。
		if dedup {
			var n int
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM task_items WHERE kb_id=? AND original_text=?", kbID, text).
				Scan(&n); err != nil {
				return 0, 0, err
			}
			if n > 0 {
				continue
			}
		}
		name := ""
		if i < len(sourceNames) {
			name = sourceNames[i]
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO task_items (task_id, kb_id, dir_id, status, original_text) VALUES (?,?,?,?,?)`,
			taskID, kbID, dirID, StatusPending, text); err != nil {
			return 0, 0, err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO documents (kb_id, dir_id, task_id, source_name) VALUES (?,?,?,?)",
			kbID, dirID, taskID, name); err != nil {
			return 0, 0, err
		}
		inserted++
	}
	if inserted == 0 {
		if e, ok := auditEntryFrom(ctx); ok {
			_ = tx.Rollback() // 先回滚再补记（失败审计为尽力而为，事务已作废）
			s.auditFailBestEffort(ctx, e, "all_duplicates")
		}
		return 0, 0, ErrAllDuplicates
	}
	// 审计：与业务写同事务（插入失败 → 回滚，任务不入库）。
	if e, ok := auditEntryFrom(ctx); ok {
		if err := insertAuditTx(ctx, tx, e, "ok", fmt.Sprintf("items=%d", inserted)); err != nil {
			return 0, 0, err
		}
	}
	return taskID, inserted, tx.Commit()
}

// 任务级状态（finding 1）：单一 tasks.status 字段推进。
const (
	TaskStatusCompleted           TaskStatus = "completed"
	TaskStatusCompletedWithErrors TaskStatus = "completed_with_errors"
)

// DecideTaskStatus 任务级状态决策（纯函数，便于单测）。
// 入参：total 条目总数、pending 仍 pending 数、done 已 cleaned 数、failed 失败数。
// 语义（README「任务状态机」）：
//   - total==0 或全部条目仍 pending → pending（任务未开始）
//   - 存在非终态条目（进行中） → cleaning
//   - 全部终态：有 failed → completed_with_errors；否则 completed
func DecideTaskStatus(total, pending, done, failed int64) TaskStatus {
	if total <= 0 || pending == total {
		return StatusPending
	}
	if done+failed >= total {
		if failed > 0 {
			return TaskStatusCompletedWithErrors
		}
		return TaskStatusCompleted
	}
	return StatusCleaning
}

// RefreshTaskStatus 按 task_items 统计推进 tasks.status（幂等，worker 在每条条目
// 终态迁移后调用）。任务不存在返回 ErrNotFound。
func (s *Store) RefreshTaskStatus(ctx context.Context, taskID int64) (TaskStatus, error) {
	var total, pending, done, failed int64
	err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(status='pending'), 0),
		       COALESCE(SUM(status='cleaned'), 0),
		       COALESCE(SUM(status='failed'), 0)
		FROM task_items WHERE task_id=?`, taskID).
		Scan(&total, &pending, &done, &failed)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: task stats %d: %w", taskID, err)
	}
	st := DecideTaskStatus(total, pending, done, failed)
	res, err := s.DB.ExecContext(ctx, "UPDATE tasks SET status=? WHERE id=?", st, taskID)
	if err != nil {
		return "", fmt.Errorf("store: refresh task %d: %w", taskID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// MySQL 对「值未变化的 UPDATE」affected_rows=0：可能是幂等重刷（状态已相同）
		// 或任务不存在——回查判别，幂等重刷正常返回（2026-09-08 集成验证暴露：
		// 原实现对重复刷新误判 ErrNotFound）。
		var exists int64
		if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE id=?", taskID).Scan(&exists); err != nil {
			return "", fmt.Errorf("store: refresh task %d: %w", taskID, err)
		}
		if exists == 0 {
			return "", ErrNotFound
		}
	}
	return st, nil
}

// NormalizePage 分页参数规范化（纯函数便于单测）：page<1→1；pageSize<1→20；>500→500。
func NormalizePage(page, pageSize int) (int, int) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 500 {
		pageSize = 500
	}
	return page, pageSize
}

// PageResult 分页结果。
type PageResult struct {
	Items    []TaskItem `json:"items"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
	Total    int64      `json:"total"`
}

// ListItemsByStatusPage 按状态分页列出 kb 条目（page 从 1 起，pageSize 1..500）。
func (s *Store) ListItemsByStatusPage(ctx context.Context, kbID int64, status TaskStatus, page, pageSize int) (*PageResult, error) {
	page, pageSize = NormalizePage(page, pageSize)
	var total int64
	if err := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM task_items WHERE kb_id=? AND status=?", kbID, status).Scan(&total); err != nil {
		return nil, err
	}
	offset := (page - 1) * pageSize
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, task_id, kb_id, dir_id, status, original_text, clean_text, error, point_id, clean_point_id
		 FROM task_items WHERE kb_id=? AND status=? ORDER BY id DESC LIMIT ? OFFSET ?`,
		kbID, status, pageSize, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]TaskItem, 0, pageSize)
	for rows.Next() {
		var it TaskItem
		if err := rows.Scan(&it.ID, &it.TaskID, &it.KBID, &it.DirID, &it.Status,
			&it.OriginalText, &it.CleanText, &it.Error, &it.PointID, &it.CleanPointID); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &PageResult{Items: items, Page: page, PageSize: pageSize, Total: total}, nil
}
