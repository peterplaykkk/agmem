package cache

import "errors"

// 缓存层哨兵错误。
//
// 注：onnxruntime 引擎相关的 ErrORTUnavailable / ErrModelUnavailable 已随
// 本地引擎迁至 internal/localembed（2026-09-11 拆分），本包保持纯 Go、无 cgo，
// 以便 agmemserv 在 CGO_ENABLED=0 下静态构建时安全引用缓存存储。
var (
	ErrCacheNotFound = errors.New("cache: not found")
)
