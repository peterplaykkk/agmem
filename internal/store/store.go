// Package store：MySQL 元数据存储（知识库/目录/任务/任务项/版本号）。
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store 封装 MySQL 访问。
type Store struct {
	DB *sql.DB
}

// Open 建立 MySQL 连接并执行内嵌迁移（幂等）。
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	s := &Store{DB: db}
	if err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Migrate 逐个执行 migrations/*.sql（按文件名排序，IF NOT EXISTS 幂等）。
func (s *Store) Migrate(ctx context.Context) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("store: read migrations: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return fmt.Errorf("store: read %s: %w", e.Name(), err)
		}
		for _, stmt := range splitStatements(string(b)) {
			if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("store: migrate %s: %w", e.Name(), err)
			}
		}
	}
	return nil
}

// splitStatements 把迁移 SQL 文本拆成可执行语句：先剥离整行注释（-- 开头
// 的行），再按 ";" 分句、丢弃空句。不能直接对整块做 HasPrefix("--") 跳过：
// 那会把「注释行在前 + 其后紧跟的 DDL」整块静默丢掉（2026-09-06 事故：
// knowledge_bases 建表语句前有文件头注释被整块跳过，后续 FK 建表报
// errno 1824）。本仓库迁移文件无存储过程/字符串内分号，按 ";" 分句安全。
func splitStatements(sqlText string) []string {
	var kept []string
	for _, line := range strings.Split(sqlText, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	var stmts []string
	for _, stmt := range strings.Split(strings.Join(kept, "\n"), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt != "" {
			stmts = append(stmts, stmt)
		}
	}
	return stmts
}

// —— 知识库 ——

// KB 一级目录模型下的知识库。
type KB struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	OwnerSub string `json:"owner_sub"`
	// 汇总字段（ListKBs 填充，单条查询为零值）
	DirCount   int64  `json:"dir_count"`
	ItemCount  int64  `json:"item_count"`
	Version    uint64 `json:"version"`
	LastTaskID int64  `json:"last_task_id"`
	LastTaskSt string `json:"last_task_status,omitempty"`
	Role       string `json:"role,omitempty"` // owner|reader（列表接口填充）
}

// Dir 1 级目录（clean 镜像目录由 payload point_type 表达，不落表）。
type Dir struct {
	ID        int64  `json:"id"`
	KBID      int64  `json:"kb_id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// CreateKB 创建知识库（name 唯一）。重复创建（幂等重跑）返回已存在的 id。
func (s *Store) CreateKB(ctx context.Context, name, ownerSub string) (int64, error) {
	if e, ok := auditEntryFrom(ctx); ok {
		return withAuditedTx(ctx, s, e, func(tx *sql.Tx) (int64, string, error) {
			res, err := tx.ExecContext(ctx,
				"INSERT INTO knowledge_bases (name, owner_sub) VALUES (?, ?) ON DUPLICATE KEY UPDATE id=id", name, ownerSub)
			if err != nil {
				if strings.Contains(err.Error(), "Duplicate") {
					return 0, "exists", ErrExists
				}
				return 0, "", fmt.Errorf("store: create kb: %w", err)
			}
			if _, idErr := res.LastInsertId(); idErr != nil {
				return 0, "", fmt.Errorf("store: create kb: %w", idErr)
			}
			// name 唯一，无条件按 name 回查（ON DUPLICATE 触发时 LAST_INSERT_ID 不更新，
			// 见下方既有路径注释）。
			var v int64
			if err := tx.QueryRowContext(ctx, "SELECT id FROM knowledge_bases WHERE name=?", name).Scan(&v); err == nil {
				return v, "", nil
			}
			id, _ := res.LastInsertId()
			return id, "", nil
		})
	}
	res, err := s.DB.ExecContext(ctx,
		"INSERT INTO knowledge_bases (name, owner_sub) VALUES (?, ?) ON DUPLICATE KEY UPDATE id=id", name, ownerSub)
	if err != nil {
		if strings.Contains(err.Error(), "Duplicate") {
			return 0, ErrExists
		}
		return 0, fmt.Errorf("store: create kb: %w", err)
	}
	// ON DUPLICATE 触发（重复运行）时 MySQL 不更新 LAST_INSERT_ID（实测返回 0，
	// 2026-09-08 集成验证首次暴露：原实现 id>0 才回查 → 重复运行拿到 kbID=0）。
	// name 唯一，无条件按 name 回查，读不到再回落 LastInsertId。
	if _, idErr := res.LastInsertId(); idErr != nil {
		return 0, fmt.Errorf("store: create kb: %w", idErr)
	}
	var v int64
	if err := s.DB.QueryRowContext(ctx, "SELECT id FROM knowledge_bases WHERE name=?", name).Scan(&v); err == nil {
		return v, nil
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// GetKB 按 name 查知识库。
func (s *Store) GetKB(ctx context.Context, name string) (*KB, error) {
	var kb KB
	err := s.DB.QueryRowContext(ctx, "SELECT id, name, owner_sub FROM knowledge_bases WHERE name=?", name).
		Scan(&kb.ID, &kb.Name, &kb.OwnerSub)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get kb: %w", err)
	}
	return &kb, nil
}

// —— 目录 ——

// CreateDir 在 kb 下建 1 级目录；同名返回 ErrExists。
func (s *Store) CreateDir(ctx context.Context, kbID int64, name string) (int64, error) {
	if e, ok := auditEntryFrom(ctx); ok {
		// 审计上下文：业务写 + 审计写同事务（审计失败即业务失败）。
		return withAuditedTx(ctx, s, e, func(tx *sql.Tx) (int64, string, error) {
			res, err := tx.ExecContext(ctx, "INSERT IGNORE INTO directories (kb_id, name) VALUES (?, ?)", kbID, name)
			if err != nil {
				return 0, "", fmt.Errorf("store: create dir: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return 0, "exists", ErrExists
			}
			id, err := res.LastInsertId()
			return id, "", err
		})
	}
	res, err := s.DB.ExecContext(ctx, "INSERT IGNORE INTO directories (kb_id, name) VALUES (?, ?)", kbID, name)
	if err != nil {
		return 0, fmt.Errorf("store: create dir: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, ErrExists
	}
	return res.LastInsertId()
}

// —— 任务 ——

// TaskStatus 任务/任务项状态机取值。
type TaskStatus string

const (
	StatusPending  TaskStatus = "pending"
	StatusEmbedded TaskStatus = "embedded"
	StatusCleaning TaskStatus = "cleaning"
	StatusCleaned  TaskStatus = "cleaned"
	StatusFailed   TaskStatus = "failed"
)

// Valid 前置状态 → 目标状态迁移校验。
// 状态机：pending → embedded → cleaning → cleaned；任一非终态 → failed。
func (t TaskStatus) Valid(to TaskStatus) bool {
	switch to {
	case StatusFailed:
		return t != StatusCleaned && t != StatusFailed
	case StatusEmbedded:
		return t == StatusPending
	case StatusCleaning:
		return t == StatusEmbedded
	case StatusCleaned:
		return t == StatusCleaning
	case StatusPending:
		return false
	}
	return false
}

// NextPendingTaskItem 取一条可处理的任务项并推进到 to 状态（乐观锁：仅当状态匹配时更新）。
// 返回 ErrNotFound 表示当前无可处理项。
type TaskItem struct {
	ID           int64      `json:"id"`
	TaskID       int64      `json:"task_id"`
	KBID         int64      `json:"kb_id"`
	DirID        int64      `json:"dir_id"`
	Status       TaskStatus `json:"status"`
	OriginalText string     `json:"original_text"`
	CleanText    NullString `json:"clean_text"`
	Error        NullString `json:"error"`
	PointID      NullString `json:"point_id"`
	CleanPointID NullString `json:"clean_point_id"`
}

// claimItem 领取一条任务项并推进状态（乐观锁：仅当状态匹配时更新）。
//
// 空结果探测优化（CI MySQL 审计 F5，2026-09-30）：该查询 99%+ 的执行落在
// 「无待处理项」的空轮询上，此前每次都为空结果付出 BEGIN+SELECT+ROLLBACK 三条
// 协议语句（29h 实测 105k 组空事务）。现在先用一条无事务的探测 SELECT 判空：
// 空结果直接返回 ErrNotFound（零事务开销），命中才开事务走原 claim 流程。
//
// 取舍：探测与事务之间存在竞态窗口（行被并发 worker 领走）——此时事务内 SELECT
// 返回空 → ErrNotFound，与既有「被并发 worker 抢走」语义一致；代价是有活时多一条
// 探测 SELECT（低频、走 idx_item_status 索引，examined≈1 行）。claim 本身
// （SELECT+UPDATE 同事务 + status 条件更新）语义不变。
func (s *Store) claimItem(ctx context.Context, from, to TaskStatus) (*TaskItem, error) {
	var probed int
	err := s.DB.QueryRowContext(ctx,
		"SELECT 1 FROM task_items WHERE status=? ORDER BY id LIMIT 1", from).Scan(&probed)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound // 空轮询热路径：不开事务、无 ROLLBACK
	}
	if err != nil {
		return nil, fmt.Errorf("store: probe item: %w", err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var it TaskItem
	err = tx.QueryRowContext(ctx,
		`SELECT id, task_id, kb_id, dir_id, status, original_text, clean_text, point_id
		 FROM task_items WHERE status=? ORDER BY id LIMIT 1`, from).
		Scan(&it.ID, &it.TaskID, &it.KBID, &it.DirID, &it.Status, &it.OriginalText, &it.CleanText, &it.PointID)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx,
		"UPDATE task_items SET status=? WHERE id=? AND status=?", to, it.ID, from)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound // 被并发 worker 抢走
	}
	it.Status = to
	return &it, tx.Commit()
}

// ClaimPending 取 pending 项并置为 embedded（原始向量已入库后调用方再推进 cleaning）。
func (s *Store) ClaimPending(ctx context.Context) (*TaskItem, error) {
	return s.claimItem(ctx, StatusPending, StatusEmbedded)
}

// ClaimEmbedded 取 embedded 项并置为 cleaning（进入 GLM 清洗）。
func (s *Store) ClaimEmbedded(ctx context.Context) (*TaskItem, error) {
	return s.claimItem(ctx, StatusEmbedded, StatusCleaning)
}

// CompleteCleaned 写清洗结果并置 cleaned；cleanPointID 为清洗条目的 qdrant point id。
func (s *Store) CompleteCleaned(ctx context.Context, itemID int64, cleanText, cleanPointID string) error {
	_, err := s.DB.ExecContext(ctx,
		"UPDATE task_items SET status=?, clean_text=?, clean_point_id=?, error=NULL WHERE id=? AND status=?",
		StatusCleaned, cleanText, cleanPointID, itemID, StatusCleaning)
	if err != nil {
		return fmt.Errorf("store: complete cleaned: %w", err)
	}
	return nil
}

// FailItem 置失败并记录原因。
func (s *Store) FailItem(ctx context.Context, itemID int64, status TaskStatus, msg string) error {
	_, err := s.DB.ExecContext(ctx,
		"UPDATE task_items SET status=?, error=? WHERE id=?", StatusFailed, msg, itemID)
	if err != nil {
		return fmt.Errorf("store: fail item: %w", err)
	}
	return nil
}

// Task 任务（含条目，供 GET /tasks/{id}）。
type Task struct {
	ID     int64      `json:"id"`
	KBID   int64      `json:"kb_id"`
	DirID  int64      `json:"dir_id"`
	Status TaskStatus `json:"status"`
	Items  []TaskItem `json:"items"`
}

func (s *Store) GetTask(ctx context.Context, kbID, taskID int64) (*Task, error) {
	t := &Task{}
	err := s.DB.QueryRowContext(ctx,
		"SELECT id, kb_id, dir_id, status FROM tasks WHERE id=? AND kb_id=?", taskID, kbID).
		Scan(&t.ID, &t.KBID, &t.DirID, &t.Status)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, status, original_text, clean_text, error, point_id, clean_point_id
		 FROM task_items WHERE task_id=? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it TaskItem
		if err := rows.Scan(&it.ID, &it.Status, &it.OriginalText, &it.CleanText, &it.Error, &it.PointID, &it.CleanPointID); err != nil {
			return nil, err
		}
		it.TaskID = taskID
		t.Items = append(t.Items, it)
	}
	return t, rows.Err()
}

// ListItemsByStatus 列出 kb 内指定状态条目（供 /points?status=cleaning）。
func (s *Store) ListItemsByStatus(ctx context.Context, kbID int64, status TaskStatus, limit int) ([]TaskItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, task_id, status, original_text, clean_text, error
		 FROM task_items WHERE kb_id=? AND status=? ORDER BY id DESC LIMIT ?`, kbID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskItem
	for rows.Next() {
		var it TaskItem
		if err := rows.Scan(&it.ID, &it.TaskID, &it.Status, &it.OriginalText, &it.CleanText, &it.Error); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// —— 版本号 ——

// BumpVersion 清洗完成后递增 kb 数据版本，返回新版本。
func (s *Store) BumpVersion(ctx context.Context, kbID int64) (uint64, error) {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO kb_versions (kb_id, version) VALUES (?, 1)
		 ON DUPLICATE KEY UPDATE version = version + 1`, kbID)
	if err != nil {
		return 0, fmt.Errorf("store: bump version: %w", err)
	}
	var v uint64
	if err := s.DB.QueryRowContext(ctx, "SELECT version FROM kb_versions WHERE kb_id=?", kbID).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// GetVersion 返回 kb 当前数据版本（无记录返回 0）。
func (s *Store) GetVersion(ctx context.Context, kbID int64) (uint64, error) {
	var v uint64
	err := s.DB.QueryRowContext(ctx, "SELECT version FROM kb_versions WHERE kb_id=?", kbID).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}
