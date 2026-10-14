// worker 空轮询退避测试（CI MySQL 审计 F5，2026-09-30）：
//   - backoffDelay 纯函数表驱动（序列、下限抬升、封顶、溢出安全）；
//   - step 返回「本轮是否领到活」（退避复位依据）；
//   - Run 空闲期指数退避 + Poke 入队唤醒恢复满速（计时集成，留宽裕边界防抖）。
package cleaner

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"sre/agmem/internal/store"
)

// countingStore 计数 claim 调用（退避效果断言用），其余方法透传 fakeStore。
type countingStore struct {
	*fakeStore
	pendingClaims atomic.Int64
	totalClaims   atomic.Int64
}

func (c *countingStore) ClaimPending(ctx context.Context) (*store.TaskItem, error) {
	c.pendingClaims.Add(1)
	c.totalClaims.Add(1)
	return c.fakeStore.ClaimPending(ctx)
}

func (c *countingStore) ClaimEmbedded(ctx context.Context) (*store.TaskItem, error) {
	c.totalClaims.Add(1)
	if c.fakeStore.item == nil {
		// 真实契约：无活时返回 ErrNotFound（fakeStore 直接返回 nil item，
		// step 会把它当领取成功，这里显式对齐）。
		return nil, store.ErrNotFound
	}
	return c.fakeStore.ClaimEmbedded(ctx)
}

// newCountingWorker 退避测试用 worker 装配（Store 走计数包装，保持接口注入）。
func newCountingWorker(st ItemStore) *Worker {
	return &Worker{
		Store:              st,
		Embedder:           &fakeEmbedder{},
		Qdrant:             &fakeQdrant{},
		LLM:                &fakeLLM{},
		CollectionResolver: func(int64) string { return "kb-sre" },
		DirResolver:        func(int64) string { return "sre" },
	}
}

// TestBackoffDelay 退避序列：streak<=0 满速；否则 max(fast, base<<n) 封顶 limit。
// 默认参数（fast=Interval=2s, base=1s, limit=30s）的有效序列：
// 2s,2s,4s,8s,16s,30s,30s,…（前两级被满速下限抬高，与「1s→2s→4s→…→cap 30s」同范式）。
func TestBackoffDelay(t *testing.T) {
	const fast, base, limit = 2 * time.Second, time.Second, 30 * time.Second
	cases := []struct {
		streak int
		want   time.Duration
	}{
		{0, fast},            // 领到活 / Poke 复位 → 满速
		{-1, fast},           // 防御：负 streak 视为满速
		{1, 2 * time.Second}, // max(2s, 1s)
		{2, 2 * time.Second}, // max(2s, 2s)
		{3, 4 * time.Second}, // max(2s, 4s)
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second}, // 32s 封顶 30s
		{7, 30 * time.Second},
		{1000, 30 * time.Second}, // 极大 streak 不溢出（循环翻倍 + 提前返回）
	}
	for _, c := range cases {
		if got := backoffDelay(c.streak, fast, base, limit); got != c.want {
			t.Errorf("backoffDelay(streak=%d) = %v, want %v", c.streak, got, c.want)
		}
	}
	// 退避基数 ≥ 上限：直接取上限。
	if got := backoffDelay(1, fast, 40*time.Second, limit); got != limit {
		t.Errorf("base>limit: got %v, want %v", got, limit)
	}
	// 满速 ≥ 上限的防御配置：不超上限。
	if got := backoffDelay(1, 60*time.Second, base, limit); got != limit {
		t.Errorf("fast>limit: got %v, want %v", got, limit)
	}
}

// TestWorkerPollDelayDefaults 未配置退避参数时的默认值（1s 基数 / 30s 上限）。
func TestWorkerPollDelayDefaults(t *testing.T) {
	w := &Worker{Interval: 2 * time.Second}
	if got := w.pollDelay(0); got != 2*time.Second {
		t.Errorf("streak=0: got %v, want 2s（满速）", got)
	}
	if got := w.pollDelay(1); got != 2*time.Second {
		t.Errorf("streak=1: got %v, want 2s", got)
	}
	if got := w.pollDelay(6); got != 30*time.Second {
		t.Errorf("streak=6: got %v, want 30s（封顶）", got)
	}
	// Interval 未配置 → 回落 2s（config 默认），不再 panic。
	if got := (&Worker{}).pollDelay(0); got != 2*time.Second {
		t.Errorf("zero interval: got %v, want 2s", got)
	}
}

// TestStepReportsClaimedWork step 的返回值是退避复位依据：空手而归=false，领到=true。
func TestStepReportsClaimedWork(t *testing.T) {
	ctx := context.Background()

	// 空 store：两路 claim 均空 → false。
	empty := &countingStore{fakeStore: &fakeStore{}}
	w := newCountingWorker(empty)
	if w.step(ctx) {
		t.Error("空 store：step 应返回 false")
	}

	// 有 pending：领取成功即 true（后续处理成败不影响）。
	pending := newItem()
	pending.Status = store.StatusPending
	st := &countingStore{fakeStore: &fakeStore{pending: pending}}
	w2 := newCountingWorker(st)
	if !w2.step(ctx) {
		t.Error("有 pending：step 应返回 true")
	}
}

// TestWorkerRunEmptyBackoff 空闲期退避：恒频 ticker（20ms）600ms 内会步进 ≈30 次；
// 退避（20ms→20ms→40ms→80ms→100ms 封顶）后步进数显著下降（理论 ≈8）。
func TestWorkerRunEmptyBackoff(t *testing.T) {
	st := &countingStore{fakeStore: &fakeStore{}}
	w := newCountingWorker(st)
	w.Interval = 20 * time.Millisecond
	w.BaseBackoff = 20 * time.Millisecond
	w.MaxBackoff = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	<-ctx.Done()
	select { // 等 in-flight step 结束再读计数
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run 未在 ctx 取消后退出")
	}

	n := st.pendingClaims.Load()
	if n < 3 {
		t.Errorf("空闲 600ms 步进 %d 次，连退避下界都不到（worker 没跑起来？）", n)
	}
	if n > 12 {
		t.Errorf("空闲 600ms 步进 %d 次（>12）：退避未生效，疑似仍为恒频 ticker", n)
	}
}

// TestWorkerRunPokeResumesFast 入队唤醒：深退避（基数 100ms）等待期收到 Poke 后，
// 下一次步进在满速间隔量级内到达，而不是等当前退避到期。
func TestWorkerRunPokeResumesFast(t *testing.T) {
	st := &countingStore{fakeStore: &fakeStore{}}
	w := newCountingWorker(st)
	w.Interval = 20 * time.Millisecond
	w.BaseBackoff = 100 * time.Millisecond
	w.MaxBackoff = 10 * time.Second // 深退避：不 Poke 就要等很久

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// 让退避爬升：步进 t≈20ms（下次 100ms）、t≈120ms（下次 200ms）。
	time.Sleep(190 * time.Millisecond)
	before := st.pendingClaims.Load()
	if before < 2 {
		t.Fatalf("前置：退避期应已有 ≥2 次步进，got %d", before)
	}
	// 安静窗（t≈190–215ms，理论上下一次步进在 t≈320ms）：无 Poke 不应有步进。
	time.Sleep(25 * time.Millisecond)
	if still := st.pendingClaims.Load(); still != before {
		t.Fatalf("前置：安静窗内出现步进 %d→%d，退避未生效", before, still)
	}

	start := time.Now()
	w.Poke()
	deadline := start.Add(1500 * time.Millisecond)
	for st.pendingClaims.Load() == before {
		if time.Now().After(deadline) {
			t.Fatalf("Poke 后 1.5s 内未恢复步进（退避未复位）")
		}
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)
	// 无 Poke 时下一次步进在 ≥200ms 之外；Poke 后应在满速量级（≈20ms，放宽到
	// 100ms 防调度抖动）。
	if elapsed > 100*time.Millisecond {
		t.Errorf("Poke 后 %v 才步进：应接近满速间隔（≤100ms）", elapsed)
	}
}
