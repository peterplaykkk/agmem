package cachectl

// 单库版版本门禁测试（2026-09-08 单库化）：
//   - 版本锚点恒为 /api/version/{cfg.KB} 的单 kb 版本（旧「/api/version/global
//     跨 kb 指纹」语义已随单库化退役）；
//   - 版本稳定 → 同问命中本地缓存；版本变化 → 失效回上游并落新版本条目；
//   - 版本端点失败/非 200 → 502（上游既已不可用，直通同样会失败；不允许无版本命中）。
//
// 注入方式与 ask_scope_test.go 一致：mock 上游 + EmbedFn 绕过 onnxruntime。

import (
	"net/http"
	"testing"

	"sre/agmem/internal/cache"
)

const gateQuestion = "统一出口代理地址"

// 版本稳定：同问复答命中本地缓存（上游 ask 只调 1 次），条目携带单 kb 版本锚点。
func TestAskCached_VersionGatedHit(t *testing.T) {
	agmem := &scopedAgmem{version: 2}
	local, store := newScopedServer(t, agmem)

	body := map[string]any{"query": gateQuestion}
	_, out1 := postAsk(t, local.URL, body)
	if out1.CacheHit {
		t.Fatalf("first ask must miss: %+v", out1)
	}
	_, out2 := postAsk(t, local.URL, body)
	if !out2.CacheHit {
		t.Fatalf("repeat ask must hit local cache (version unchanged): %+v", out2)
	}
	if n := agmem.calls(); n != 1 {
		t.Fatalf("second ask must be local, want 1 upstream call, got %d", n)
	}
	if out2.KBVersion != 2 {
		t.Fatalf("hit must carry the single-kb version 2, got %d", out2.KBVersion)
	}
	// 落库：id 按 (dir, question) 派生；KB 为溯源字段（cfg.KB）
	e, err := store.Get(cache.QueryID("", gateQuestion))
	if err != nil {
		t.Fatalf("entry not stored: %v", err)
	}
	if e.KB != "sre" || e.Dir != "" {
		t.Fatalf("entry provenance wrong: %+v", e)
	}
	if e.KBVersion != 2 {
		t.Fatalf("stored entry must carry single-kb version, got %d", e.KBVersion)
	}
}

// 版本变化（服务端知识库变版）→ 缓存失效，回上游并落新版本条目。
func TestAskCached_VersionChangeMisses(t *testing.T) {
	agmem := &scopedAgmem{version: 2}
	local, _ := newScopedServer(t, agmem)

	body := map[string]any{"query": gateQuestion}
	postAsk(t, local.URL, body)
	_, out2 := postAsk(t, local.URL, body)
	if !out2.CacheHit {
		t.Fatal("precondition: repeat must hit before version change")
	}

	agmem.setVersion(3) // 模拟 kb 变版
	_, out3 := postAsk(t, local.URL, body)
	if out3.CacheHit {
		t.Fatalf("version bump must invalidate cache: %+v", out3)
	}
	_, out4 := postAsk(t, local.URL, body)
	if !out4.CacheHit {
		t.Fatal("repeat after refresh must hit with new version")
	}
	if out4.KBVersion != 3 {
		t.Fatalf("new entry must carry new version 3, got %d", out4.KBVersion)
	}
	if n := agmem.calls(); n != 2 {
		t.Fatalf("want 2 upstream calls (initial + post-change), got %d", n)
	}
}

// 版本端点 500 → 502（不允许无版本命中；不落缓存）。
func TestAskCached_VersionEndpointError502(t *testing.T) {
	agmem := &scopedAgmem{failVersion: true}
	local, store := newScopedServer(t, agmem)

	code, out := postAsk(t, local.URL, map[string]any{"query": gateQuestion})
	if code != http.StatusBadGateway || out.CacheHit {
		t.Fatalf("version failure: want 502 no-hit, got code=%d out=%+v", code, out)
	}
	if total, _ := store.TotalBytes(); total != 0 {
		t.Fatalf("must not write local cache, bytes=%d", total)
	}
	if agmem.calls() != 0 {
		t.Fatalf("ask must not reach upstream without a valid version anchor, got %d calls", agmem.calls())
	}
}

// 版本端点版本回 0（库无记录的合法形态）→ 正常走通（版本 0 与条目版本一致即可命中）。
func TestAskCached_VersionZeroIsLegal(t *testing.T) {
	agmem := &scopedAgmem{version: 0}
	local, _ := newScopedServer(t, agmem)

	body := map[string]any{"query": gateQuestion}
	_, out1 := postAsk(t, local.URL, body)
	if out1.CacheHit || out1.KBVersion != 0 {
		t.Fatalf("first ask must miss with version 0: %+v", out1)
	}
	_, out2 := postAsk(t, local.URL, body)
	if !out2.CacheHit {
		t.Fatalf("same version 0 repeat must hit: %+v", out2)
	}
}

// dir 查询与全库查询的版本锚点相同（同一 /api/version/{KB}）——dir 只影响缓存
// scope，不影响版本门禁。
func TestAskCached_DirSharesVersionAnchor(t *testing.T) {
	agmem := &scopedAgmem{version: 5}
	local, _ := newScopedServer(t, agmem)

	q := map[string]any{"dir": "k8s", "query": gateQuestion}
	if _, out := postAsk(t, local.URL, q); out.CacheHit {
		t.Fatal("precondition: first dir ask must miss")
	}
	if _, out := postAsk(t, local.URL, q); !out.CacheHit {
		t.Fatalf("dir repeat must hit: %+v", out)
	}
	if n := agmem.calls(); n != 1 {
		t.Fatalf("want 1 upstream ask call, got %d", n)
	}
}
