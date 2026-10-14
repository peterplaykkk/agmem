package store

import "errors"

// ErrNotFound / ErrExists / ErrAllDuplicates：存储层通用哨兵错误。
var (
	ErrNotFound = errors.New("store: not found")
	ErrExists   = errors.New("store: already exists")
	// ErrAllDuplicates 入库去重（2026-09-08）：CreateTaskWithItems 收到的全部
	// 文本都已存在于该 kb（任意状态），未创建任何条目 → 不建空任务，返回本哨兵。
	ErrAllDuplicates = errors.New("store: all items duplicate")
	// ErrInvalidState 状态机拒绝迁移（UC-011 重试：仅 failed → pending）。
	// 调用方（api）映射为 409；配合返回的当前状态给出可判定错误体。
	ErrInvalidState = errors.New("store: invalid state")
)
