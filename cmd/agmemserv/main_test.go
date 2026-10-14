package main

// 进程装配层：newParentDirFunc（cleaner L2 父目录钩子）接 wiki_sync_state 父子关系。
// 未启用（nil state）→ nil 钩子；state 为空/查询失败 → 空串；有数据 → 命中 -clean 目录名。

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"sre/agmem/internal/cache"
	"sre/agmem/internal/wikisync"
)

// —— 进程内查询缓存装配（2026-09-11）：目录可写启用、不可写 fail-open ——
// 只读根文件系统（readOnlyRootFilesystem）下缓存目录不可写，openAskCache 必须
// fail-open 返回 nil（禁用缓存）而非让启动失败。

// 可写目录：启用缓存并在 AGMEMD_DIR 下建出 cache.db。
func TestOpenAskCache_EnabledUsesEnvDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGMEMD_DIR", dir)
	cs := openAskCache()
	if cs == nil {
		t.Fatal("writable AGMEMD_DIR must enable cache")
	}
	defer cs.DB.Close()
	if _, err := os.Stat(filepath.Join(dir, "cache.db")); err != nil {
		t.Fatalf("cache.db not created under AGMEMD_DIR: %v", err)
	}
}

// 不可创建目录（父路径是普通文件 → MkdirAll 失败）：fail-open 返回 nil，不 panic/fatal。
func TestOpenAskCache_FailOpenUncreatableDir(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGMEMD_DIR", filepath.Join(parent, "sub"))
	if cs := openAskCache(); cs != nil {
		defer cs.DB.Close()
		t.Fatal("uncreatable AGMEMD_DIR must fail open to nil")
	}
}

// 目录存在但只读（模拟 readOnlyRootFilesystem 挂载点）：无法创建 cache.db → fail-open。
// root 不受 0599 权限约束，故 root 下跳过（非 root 的本地/CI 覆盖真实语义）。
func TestOpenAskCache_FailOpenReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 绕过目录权限，跳过只读目录 fail-open 断言")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	t.Setenv("AGMEMD_DIR", dir)
	if cs := openAskCache(); cs != nil {
		defer cs.DB.Close()
		t.Fatal("read-only AGMEMD_DIR must fail open to nil")
	}
}

// AGMEMD_MAX_BYTES：合法正数覆盖、非法/空回落默认 500MB。
func TestCacheMaxBytesFromEnv(t *testing.T) {
	t.Setenv("AGMEMD_MAX_BYTES", "")
	if got := cacheMaxBytesFromEnv(); got != cache.DefaultMaxBytes {
		t.Fatalf("empty → want default %d, got %d", cache.DefaultMaxBytes, got)
	}
	t.Setenv("AGMEMD_MAX_BYTES", "1048576")
	if got := cacheMaxBytesFromEnv(); got != 1048576 {
		t.Fatalf("want 1048576, got %d", got)
	}
	t.Setenv("AGMEMD_MAX_BYTES", "garbage")
	if got := cacheMaxBytesFromEnv(); got != cache.DefaultMaxBytes {
		t.Fatalf("invalid → want default %d, got %d", cache.DefaultMaxBytes, got)
	}
}

func newTestWSState(t *testing.T) *wikisync.StateStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s := &wikisync.StateStore{DB: db, Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return s
}

func TestNewParentDirFunc(t *testing.T) {
	ctx := context.Background()

	// 未启用 wiki 同步（nil state）→ nil 钩子（cleaner 据此降级为仅同目录）。
	if fn := newParentDirFunc(ctx, nil); fn != nil {
		t.Fatalf("nil state 应返回 nil 钩子，得到非 nil")
	}

	// state 为空：非 nil 钩子，但任何目录都返回空串且不 panic。
	ws := newTestWSState(t)
	fn := newParentDirFunc(ctx, ws)
	if fn == nil {
		t.Fatal("非 nil state 应返回非 nil 钩子")
	}
	if got := fn("任意-clean"); got != "" {
		t.Fatalf("空 state 应返回空串，得到 %q", got)
	}

	// 有父子数据：目录名按 `<dir>-clean` 口径命中父目录。
	seed := func(st wikisync.State) {
		if err := ws.Save(ctx, st); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(wikisync.State{DocToken: "cd", NodeToken: "c1", ParentNodeToken: "p1", Dir: "系统"})
	seed(wikisync.State{DocToken: "dd", NodeToken: "d1", ParentNodeToken: "c1", Dir: "网络"})
	seed(wikisync.State{DocToken: "ed", NodeToken: "e1", ParentNodeToken: "d1", Dir: "文档D"})

	fn = newParentDirFunc(ctx, ws)
	if got := fn("文档D-clean"); got != "网络-clean" {
		t.Fatalf("fn(文档D-clean)=%q want 网络-clean", got)
	}
	if got := fn("网络-clean"); got != "系统-clean" {
		t.Fatalf("fn(网络-clean)=%q want 系统-clean", got)
	}
	if got := fn("不存在-clean"); got != "" {
		t.Fatalf("未知目录应返回空串，得到 %q", got)
	}
}
