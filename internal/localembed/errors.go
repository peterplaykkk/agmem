package localembed

import "errors"

// 本地 embedding 引擎哨兵错误（原属 internal/cache；2026-09-11 拆分到本包，
// 使纯 Go 的 internal/cache 不再经 onnxruntime cgo 依赖——agmemserv 以
// CGO_ENABLED=0 静态构建，不能链接 onnxruntime）。
var (
	// ErrORTUnavailable onnxruntime 共享库不可用（未安装/路径错误）。
	ErrORTUnavailable = errors.New("cache: onnxruntime library unavailable")
	// ErrModelUnavailable 本地模型未就绪（未下载/下载失败）。
	ErrModelUnavailable = errors.New("cache: local embedding model unavailable")
)
