// agmemd：本地守护进程（缓存 + 本地 embedding + 版本校验 + 置信度命中）。
//
// 用法: agmemd [--version]      （服务地址经 AGMEMD_ADDR 配置）
//
// 环境变量见 README。首次启动自动下载 bge-base-zh ONNX 模型到缓存目录。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"sre/agmem/internal/cache"
	"sre/agmem/internal/cachectl"
	"sre/agmem/internal/config"
	"sre/agmem/internal/localembed"
)

func main() {
	// 版本查询是最早的短路：先于 AGMEM_URL 必填校验与缓存目录创建——
	// 查版本不应要求配置齐全，也不该有副作用（见 version.go）。
	if printVersionIfRequested(os.Args[1:], os.Stdout) {
		return
	}

	home, _ := os.UserHomeDir()
	cfg := cachectl.Config{
		Addr:       getenv("AGMEMD_ADDR", "127.0.0.1:8377"),
		CacheDir:   getenv("AGMEMD_DIR", filepath.Join(home, ".cache", "agmemd")),
		MaxBytes:   500 << 20,
		AgmemURL:   os.Getenv("AGMEM_URL"),
		AgmemToken: os.Getenv("AGMEM_TOKEN"),
		// 单库版（2026-09-08）：全局唯一 kb 的版本门禁名（与 agmemserv 的 AGMEM_KB 一致）
		KB: getenv("AGMEM_KB", config.DefaultKB),
	}
	if v := os.Getenv("AGMEMD_MAX_BYTES"); v != "" {
		if mb, err := strconv.ParseInt(v, 10, 64); err == nil && mb > 0 {
			cfg.MaxBytes = mb
		}
	}
	if cfg.AgmemURL == "" {
		log.Fatalf("agmemd: AGMEM_URL 必填")
	}
	if v := os.Getenv("AGMEMD_SIM_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
			cache.SetSimThreshold(f)
		}
	}

	store, err := cache.OpenCache(cfg.CacheDir)
	if err != nil {
		log.Fatalf("agmemd: cache: %v", err)
	}
	engine := localembed.NewEngine(localembed.DefaultEmbedConfig(cfg.CacheDir))

	srv := &cachectl.Server{Cfg: cfg, Store: store, Engine: engine}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("agmemd listening on http://%s (cache=%s)", cfg.Addr, cfg.CacheDir)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("agmemd: listen: %v", err)
		}
	}()

	// 模型/引擎加载放后台：healthz 先可用，agmem 拉起后可轮询 engine ready
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := engine.Load(ctx); err != nil {
			log.Printf("agmemd: engine load failed（/api/ask 将返回 503）: %v", err)
		} else {
			log.Printf("agmemd: local embedding engine ready (EP=%s, dim=%d)",
				getenv("AGMEMD_EP", "cpu"), localembed.EmbedDim)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
