package cleaner

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"sre/agmem/internal/bm25"
	"sre/agmem/internal/embedding"
	"sre/agmem/internal/qdrant"
	"sre/agmem/internal/store"
	"sre/agmem/internal/wikisync"
)

// —— TaskStatus 状态机迁移表（store.TaskStatus.Valid）——

func TestTaskStatusTransitions(t *testing.T) {
	cases := []struct {
		from, to store.TaskStatus
		want     bool
	}{
		{store.StatusPending, store.StatusEmbedded, true},
		{store.StatusPending, store.StatusCleaning, false},
		{store.StatusPending, store.StatusCleaned, false},
		{store.StatusPending, store.StatusFailed, true},
		{store.StatusEmbedded, store.StatusCleaning, true},
		{store.StatusEmbedded, store.StatusPending, false},
		{store.StatusEmbedded, store.StatusFailed, true},
		{store.StatusCleaning, store.StatusCleaned, true},
		{store.StatusCleaning, store.StatusFailed, true},
		{store.StatusCleaning, store.StatusEmbedded, false},
		{store.StatusCleaned, store.StatusFailed, false}, // 终态不可逆
		{store.StatusCleaned, store.StatusCleaning, false},
		{store.StatusFailed, store.StatusFailed, false}, // 终态不可逆
		{store.StatusFailed, store.StatusPending, false},
	}
	for _, c := range cases {
		if got := c.from.Valid(c.to); got != c.want {
			t.Errorf("%s -> %s: got %v want %v", c.from, c.to, got, c.want)
		}
	}
}

// —— fake 组件 ——

type fakeLLM struct {
	out       string
	err       error
	strictOut string
	strictErr error
	anchors   []string
	source    string
}

func (f *fakeLLM) Clean(ctx context.Context, text string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.out, nil
}

func (f *fakeLLM) CleanWithSource(ctx context.Context, text, sourceName string) (string, error) {
	f.source = sourceName
	return f.Clean(ctx, text)
}

func (f *fakeLLM) CleanWithAnchors(ctx context.Context, text, sourceName string, anchors []string) (string, error) {
	f.source = sourceName
	f.anchors = anchors
	if f.strictErr != nil {
		return "", f.strictErr
	}
	if f.strictOut != "" {
		return f.strictOut, nil
	}
	return f.Clean(ctx, text)
}

// fakeSourceResolver 内存版 SourceResolver。
type fakeSourceResolver struct {
	name string
	err  error
}

func (f fakeSourceResolver) SourceNameForItem(context.Context, int64) (string, error) {
	return f.name, f.err
}

// processPending / processEmbedded 依赖 Store（MySQL），单测聚焦状态机判定与
// LLM 空结果去重语义；依赖注入的桩实现见 stubs（worker.go 的 LLM 接口可测性）。

func TestFakeLLM_ErrorPropagates(t *testing.T) {
	want := errors.New("boom")
	f := &fakeLLM{err: want}
	_, err := f.Clean(context.Background(), "x")
	if !errors.Is(err, want) {
		t.Fatalf("want boom, got %v", err)
	}
}

func TestCleanDirNameSuffix(t *testing.T) {
	w := &Worker{DirResolver: func(dirID int64) string { return "sre" }}
	if got := w.cleanDirName(1); got != "sre-clean" {
		t.Fatalf("want sre-clean, got %s", got)
	}
	fallback := &Worker{} // 无 resolver：骨架期回退 dir-<id>
	if got := fallback.dirName(7); got != "dir-7" {
		t.Fatalf("fallback resolver: want dir-7, got %s", got)
	}
	if got := fallback.cleanDirName(7); got != "dir-7-clean" {
		t.Fatalf("fallback clean dir: want dir-7-clean, got %s", got)
	}
}

func TestPointIDDeterministic(t *testing.T) {
	// 确定性：同 (kind, itemID) 幂等
	if newPointID(42, "original") != newPointID(42, "original") {
		t.Fatalf("point id not deterministic")
	}
	// 合法性：必须是 UUID（Qdrant 只接受 u64/UUID 字符串；"item-42" 会被 400）
	if _, err := uuid.Parse(newPointID(42, "original")); err != nil {
		t.Fatalf("point id must be a UUID: %v", err)
	}
	// original 与 clean 必须是两个不同点（搜索按 point_type 过滤，二者共存）
	if newPointID(42, "original") == newPointID(42, "clean") {
		t.Fatal("original and clean point ids must differ")
	}
	if newPointID(1, "original") == newPointID(2, "original") {
		t.Fatal("point ids must differ across items")
	}
}

// —— 占位条目判定（2026-09-07 线上实证：GLM 清洗输出占位条目被照常入库）——

func TestIsPlaceholderEntry(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// 占位：命中判定规则
		{"空串", "", true},
		{"字面双引号", `""`, true},
		{"全角括号空", `（空）`, true},
		{"半角括号空", `(空)`, true},
		{"引号包裹空白", `"  "`, true},
		{"引号括号混用垃圾串", `'"'`, true},
		{"不配对引号串", `"'`, true},
		{"单个引号", `"`, true},
		{"引号包裹空", `"空"`, true},
		{"嵌套包裹", `"（空）"`, true},
		{"单字无", `无`, true},
		{"N/A 大写", `N/A`, true},
		{"n/a 小写", `n/a`, true},
		{"N/A 混合大小写", `N/a`, true},
		// 非占位：宁宽勿误杀
		{"正常短条目", `镜像使用方法`, false},
		{"正常长条目", `镜像使用方法：docker pull ccr.example.com/pub/agmem:v0.3 后执行 kubectl rollout restart deploy/agmemd`, false},
		{"markdown 开头条目", `- 部署脚本位置 ops/runbook.md`, false},
		{"编号开头条目", `1. 无重复内容已删除`, false},
		{"「无」为前缀的正文不误杀", `无状态服务部署在 prod-server`, false},
		{"括号包裹正文不误杀", `（重要）周五发布窗口`, false},
		{"空白包裹正文不误杀", ` 空 行 说 明 `, false},
		{"引号包裹正文不误杀", `"N/A" 表示未知来源的条目`, false},
	}
	for _, c := range cases {
		if got := isPlaceholderEntry(c.in); got != c.want {
			t.Errorf("%s: isPlaceholderEntry(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestFilterCleanEntries 的全部用例随分条批升级为「行级中和 → 分条 → 条目级过滤」
// 管线语义回归，迁移至 entrysplit_test.go TestCleanPipeline_PlaceholderFiltering。

// —— 分条多点入库（2026-09-07 分条批：1 条目 1 个 clean 点）——

type completeCall struct {
	itemID   int64
	cleanTxt string
	pointID  string
}

type failCall struct {
	itemID int64
	status store.TaskStatus
	msg    string
}

type requeueCall struct {
	kbID   int64
	itemID int64
}

// fakeStore 内存版 ItemStore（*store.Store 的最小假实现）。
type fakeStore struct {
	item      *store.TaskItem
	pending   *store.TaskItem // ClaimPending 返回值（nil = 无 pending，维持 ErrNotFound 语义）
	completed []completeCall
	failed    []failCall
	requeued  []requeueCall
	bumps     []int64
	refreshed []int64
}

func (f *fakeStore) ClaimPending(context.Context) (*store.TaskItem, error) {
	if f.pending == nil {
		return nil, store.ErrNotFound
	}
	return f.pending, nil
}

func (f *fakeStore) ClaimEmbedded(context.Context) (*store.TaskItem, error) { return f.item, nil }

func (f *fakeStore) CompleteCleaned(_ context.Context, itemID int64, cleanText, cleanPointID string) error {
	f.completed = append(f.completed, completeCall{itemID, cleanText, cleanPointID})
	return nil
}

func (f *fakeStore) FailItem(_ context.Context, itemID int64, status store.TaskStatus, msg string) error {
	f.failed = append(f.failed, failCall{itemID, status, msg})
	return nil
}

func (f *fakeStore) RequeueItem(_ context.Context, kbID, itemID int64) (store.TaskStatus, error) {
	f.requeued = append(f.requeued, requeueCall{kbID, itemID})
	return store.StatusPending, nil
}

func (f *fakeStore) BumpVersion(_ context.Context, kbID int64) (uint64, error) {
	f.bumps = append(f.bumps, kbID)
	return 1, nil
}

func (f *fakeStore) RefreshTaskStatus(_ context.Context, taskID int64) (store.TaskStatus, error) {
	f.refreshed = append(f.refreshed, taskID)
	return "", nil
}

// fakeEmbedder 记录每次批量输入，返回可区分向量 [batchLen, i]。
type fakeEmbedder struct {
	batches [][]string
	fail    error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.batches = append(f.batches, texts)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{float32(len(out)), float32(i)}
	}
	return out, nil
}

// shortEmbedder 契约破坏桩：返回向量数与输入不一致（worker 必须拒绝而非 panic）。
type shortEmbedder struct{}

func (shortEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1}}, nil
}

// fakeQdrant 假 qdrant：记录 upsert 的 collection 与全部点。
type fakeQdrant struct {
	calls       int
	collections []string
	points      []qdrant.Point
	fail        error
}

func (f *fakeQdrant) Upsert(_ context.Context, collection string, points []qdrant.Point) error {
	if f.fail != nil {
		return f.fail
	}
	f.calls++
	f.collections = append(f.collections, collection)
	f.points = append(f.points, points...)
	return nil
}

func newTestWorker(item *store.TaskItem, llm LLM, emb Embedder, qd PointSink, st *fakeStore) *Worker {
	return &Worker{
		Store:              st,
		Embedder:           emb,
		Qdrant:             qd,
		LLM:                llm,
		CollectionResolver: func(int64) string { return "kb-sre" },
		DirResolver:        func(int64) string { return "sre" },
	}
}

func newItem() *store.TaskItem {
	return &store.TaskItem{ID: 42, TaskID: 7, KBID: 1, DirID: 3, Status: store.StatusCleaning, OriginalText: "原始文本"}
}

func TestProcessEmbeddedMultiEntry(t *testing.T) {
	glmOut := "## 条目一\n" + rep('甲', 380) + "\n\n## 条目二\n" + rep('乙', 380) +
		"\n\n## 条目三\n" + rep('丙', 380)
	st := &fakeStore{item: newItem()}
	qd := &fakeQdrant{}
	emb := &fakeEmbedder{}
	w := newTestWorker(st.item, &fakeLLM{out: glmOut}, emb, qd, st)

	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	// 条目形态 = 清洗管线 + 来源行注入（G01：item.OriginalText 无标题 → 未命名文档）
	wantEntries := cleanEntriesWithSource(glmOut, unnamedSource)
	if len(wantEntries) != 3 {
		t.Fatalf("前置：3 个 ~387 字条目应各自成条，got %d", len(wantEntries))
	}
	if qd.calls != 1 || len(qd.points) != 3 {
		t.Fatalf("1 item × 3 条目必须 1 次 upsert 3 个点，got calls=%d points=%d", qd.calls, len(qd.points))
	}
	if qd.collections[0] != "kb-sre" {
		t.Errorf("collection = %q, want kb-sre", qd.collections[0])
	}
	seen := map[string]bool{}
	for i, p := range qd.points {
		if seen[p.ID] {
			t.Errorf("point id 重复: %s", p.ID)
		}
		seen[p.ID] = true
		if _, err := uuid.Parse(p.ID); err != nil {
			t.Errorf("point %d id 非法 UUID: %v", i, err)
		}
		if p.ID != newEntryPointID(42, "clean", i) {
			t.Errorf("point %d id = %s, want entry 派生 id %s", i, p.ID, newEntryPointID(42, "clean", i))
		}
		pl := p.Payload
		if pl.PointType != "clean" {
			t.Errorf("point %d point_type = %q", i, pl.PointType)
		}
		if pl.Dir != "sre-clean" {
			t.Errorf("point %d dir = %q, want sre-clean", i, pl.Dir)
		}
		if pl.ItemID != 42 || pl.TaskID != 7 || pl.KB != "kb-sre" {
			t.Errorf("point %d item/task/kb = %d/%d/%q", i, pl.ItemID, pl.TaskID, pl.KB)
		}
		if pl.EntryIndex != i || pl.EntryCount != 3 {
			t.Errorf("point %d entry_index/count = %d/%d, want %d/3", i, pl.EntryIndex, pl.EntryCount, i)
		}
		if pl.Text != wantEntries[i] {
			t.Errorf("point %d text 与条目不一致:\n got=%.40s\nwant=%.40s", i, pl.Text, wantEntries[i])
		}
		if pl.SourceDoc != unnamedSource {
			t.Errorf("point %d source_doc = %q, want %q", i, pl.SourceDoc, unnamedSource)
		}
		if !strings.Contains(pl.Text, "来源："+unnamedSource) {
			t.Errorf("point %d 缺来源行: %q", i, pl.Text)
		}
		if want := []float32{3, float32(i)}; !reflect.DeepEqual(p.Vector, want) {
			t.Errorf("point %d vector = %v, want %v（条目与向量一一对应）", i, p.Vector, want)
		}
	}
	// embedding 恰好一次批量、输入即 3 条目原文
	if len(emb.batches) != 1 || !reflect.DeepEqual(emb.batches[0], wantEntries) {
		t.Errorf("embed 输入 = %#v, want 一次批量 3 条目", emb.batches)
	}
	// store 副作用：complete 一次（全文拼接 + 首条点 id）、bump、refresh，无失败
	if len(st.completed) != 1 {
		t.Fatalf("CompleteCleaned 调用 %d 次, want 1", len(st.completed))
	}
	c := st.completed[0]
	if c.itemID != 42 || c.cleanTxt != strings.Join(wantEntries, "\n") || c.pointID != qd.points[0].ID {
		t.Errorf("complete = %+v, want item=42 全文拼接 + 首条点 id", c)
	}
	if !reflect.DeepEqual(st.bumps, []int64{1}) || !reflect.DeepEqual(st.refreshed, []int64{7}) {
		t.Errorf("bumps/refreshed = %v/%v", st.bumps, st.refreshed)
	}
	if len(st.failed) != 0 {
		t.Errorf("不应失败: %+v", st.failed)
	}
}

func TestProcessEmbeddedPlaceholderOnly(t *testing.T) {
	st := &fakeStore{item: newItem()}
	qd := &fakeQdrant{}
	w := newTestWorker(st.item, &fakeLLM{out: "\"\"\n（空）"}, &fakeEmbedder{}, qd, st)
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if qd.calls != 0 || len(qd.points) != 0 {
		t.Errorf("纯占位响应不得落任何 clean 点，got calls=%d points=%d", qd.calls, len(qd.points))
	}
	if len(st.completed) != 1 || st.completed[0] != (completeCall{itemID: 42}) {
		t.Errorf("complete = %+v, want {42 \"\" \"\"}（沿用全文重复语义）", st.completed)
	}
	if len(st.bumps) != 0 {
		t.Errorf("dedup 路径不递增版本，bumps=%v", st.bumps)
	}
	if !reflect.DeepEqual(st.refreshed, []int64{7}) {
		t.Errorf("refreshed = %v", st.refreshed)
	}
}

func TestProcessEmbeddedSingleEntry(t *testing.T) {
	st := &fakeStore{item: newItem()}
	qd := &fakeQdrant{}
	// fixture 长度 ≥MinEntryLen（<15 字条目按 2026-09-08 规格在 embed 前剔除，
	// 微型-only 语义另有 TestProcessEmbeddedMicroOnly 专测）
	w := newTestWorker(st.item, &fakeLLM{out: "一条记录：部署完成，验证通过后归档"}, &fakeEmbedder{}, qd, st)
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if len(qd.points) != 1 {
		t.Fatalf("单条目必须单点，got %d", len(qd.points))
	}
	p := qd.points[0]
	if p.ID != newEntryPointID(42, "clean", 0) || p.Payload.EntryIndex != 0 || p.Payload.EntryCount != 1 {
		t.Errorf("单点 payload = id=%s idx=%d count=%d", p.ID, p.Payload.EntryIndex, p.Payload.EntryCount)
	}
}

func TestProcessEmbeddedEmbedMismatchFails(t *testing.T) {
	st := &fakeStore{item: newItem()}
	qd := &fakeQdrant{}
	// 三个 ~387 字条目（装不进一条）→ 3 输入 1 向量 → 必须报错而非索引越界
	glmOut := "## 甲\n" + rep('甲', 380) + "\n\n## 乙\n" + rep('乙', 380) + "\n\n## 丙\n" + rep('丙', 380)
	w := newTestWorker(st.item, &fakeLLM{out: glmOut}, shortEmbedder{}, qd, st)
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if len(qd.points) != 0 || len(st.completed) != 0 {
		t.Errorf("向量数不符必须中止，points=%d completed=%d", len(qd.points), len(st.completed))
	}
	if len(st.failed) != 1 || !strings.Contains(st.failed[0].msg, "embed clean") {
		t.Errorf("fail = %+v, want embed clean 阶段失败", st.failed)
	}
}

func TestProcessEmbeddedLLMErrorFails(t *testing.T) {
	st := &fakeStore{item: newItem()}
	qd := &fakeQdrant{}
	w := newTestWorker(st.item, &fakeLLM{err: errors.New("boom")}, &fakeEmbedder{}, qd, st)
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if len(st.failed) != 1 || !strings.Contains(st.failed[0].msg, "glm clean") {
		t.Errorf("fail = %+v, want glm clean 阶段失败", st.failed)
	}
	if !reflect.DeepEqual(st.refreshed, []int64{7}) {
		t.Errorf("失败路径也要推进任务状态，refreshed=%v", st.refreshed)
	}
	if len(qd.points) != 0 || len(st.completed) != 0 {
		t.Errorf("失败路径不得写点/complete")
	}
}

func TestNewEntryPointID(t *testing.T) {
	// 确定性 + UUID 合法性
	if newEntryPointID(42, "clean", 0) != newEntryPointID(42, "clean", 0) {
		t.Fatal("entry point id not deterministic")
	}
	if _, err := uuid.Parse(newEntryPointID(42, "clean", 0)); err != nil {
		t.Fatalf("must be UUID: %v", err)
	}
	// idx / item / kind 任一不同 → id 不同
	if newEntryPointID(42, "clean", 0) == newEntryPointID(42, "clean", 1) {
		t.Fatal("ids must differ across entry indexes")
	}
	if newEntryPointID(1, "clean", 0) == newEntryPointID(2, "clean", 0) {
		t.Fatal("ids must differ across items")
	}
	if newEntryPointID(42, "original", 0) == newEntryPointID(42, "clean", 0) {
		t.Fatal("ids must differ across kinds")
	}
	// 与 item 粒度 newPointID 命名空间隔离
	if newEntryPointID(42, "clean", 0) == newPointID(42, "clean") {
		t.Fatal("entry id must not collide with item-level id")
	}
}

// —— 微型条目（<MinEntryLen）dedup 语义（2026-09-08 混合检索批）——

// TestProcessEmbeddedMicroOnly：GLM 输出全部微型条目 → 与占位同语义：
// cleaned、不落 clean 点、不 bump 版本。
func TestProcessEmbeddedMicroOnly(t *testing.T) {
	st := &fakeStore{item: newItem()}
	qd := &fakeQdrant{}
	w := newTestWorker(st.item, &fakeLLM{out: "第一步\n第二步\n第三步"}, &fakeEmbedder{}, qd, st)
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if qd.calls != 0 || len(qd.points) != 0 {
		t.Errorf("微型-only 响应不得落任何 clean 点，got calls=%d points=%+v", qd.calls, qd.points)
	}
	if len(st.completed) != 1 || st.completed[0] != (completeCall{itemID: 42}) {
		t.Errorf("complete = %+v, want {42 \"\" \"\"}（沿用 dedup 语义）", st.completed)
	}
	if len(st.bumps) != 0 {
		t.Errorf("dedup 语义不 bump 版本，got %v", st.bumps)
	}
	if len(st.failed) != 0 {
		t.Errorf("不应失败: %+v", st.failed)
	}
}

// —— 混合检索稀疏写入（SparseEnabled 开关）——

// TestProcessEmbeddedSparseEnabled：开关开启 → clean 点带 bm25 稀疏向量
// （indices/values 由条目文本编码，确定性）；original 点不写稀疏。
func TestProcessEmbeddedSparseEnabled(t *testing.T) {
	glmOut := "## 步骤\n" + rep('甲', 20) // 22 字条目（≥15 保留）
	st := &fakeStore{item: newItem()}
	qd := &fakeQdrant{}
	w := newTestWorker(st.item, &fakeLLM{out: glmOut}, &fakeEmbedder{}, qd, st)
	w.SparseEnabled = true
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if len(qd.points) != 1 {
		t.Fatalf("want 1 clean point, got %d", len(qd.points))
	}
	p := qd.points[0]
	sv, ok := p.Sparse[qdrant.SparseVectorName]
	if !ok {
		t.Fatalf("clean point must carry bm25 sparse vector, got %+v", p.Sparse)
	}
	wantIdx, wantVals := bm25.SparseVector(p.Payload.Text)
	if !reflect.DeepEqual(sv.Indices, wantIdx) || !reflect.DeepEqual(sv.Values, wantVals) {
		t.Errorf("sparse = %v/%v, want 对条目文本的编码 %v/%v",
			sv.Indices, sv.Values, wantIdx, wantVals)
	}
	if len(sv.Indices) == 0 || len(sv.Indices) != len(sv.Values) {
		t.Errorf("sparse indices/values 非法: %d/%d", len(sv.Indices), len(sv.Values))
	}

	// original 点不写稀疏（检索只搜 clean）
	qd2 := &fakeQdrant{}
	pendingItem := *st.item
	pendingItem.Status = store.StatusPending
	st2 := &fakeStore{pending: &pendingItem}
	w2 := newTestWorker(&pendingItem, &fakeLLM{}, &fakeEmbedder{}, qd2, st2)
	w2.SparseEnabled = true
	if _, err := w2.processPending(context.Background()); err != nil {
		t.Fatalf("processPending: %v", err)
	}
	if len(qd2.points) != 1 {
		t.Fatalf("want 1 original point, got %d", len(qd2.points))
	}
	if len(qd2.points[0].Sparse) != 0 {
		t.Errorf("original point must not carry sparse vectors, got %+v", qd2.points[0].Sparse)
	}
}

// TestProcessPendingEmbedPathIgnoresQueryInstruction 写入路径不受查询指令影响
// （2026-09-09 检索质量批）：worker 用 Embedder.Embed（非 EmbedQuery），即使
// 生产 embedder 注入了 QueryInstruction，落库向量也不含指令前缀——非对称检索
// 的文档侧保证，存量 2577 向量无需重索引。用真实 embedding.Client 打到 fake
// /embeddings，断言请求 input 就是原始文本。
func TestProcessPendingEmbedPathIgnoresQueryInstruction(t *testing.T) {
	var got [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = append(got, req.Input)
		vec := make([]float32, embedding.Dim)
		b, _ := json.Marshal(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": vec}},
		})
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	emb := embedding.New(srv.URL, "k", "", "", "m", 16, 0)
	emb.QueryInstruction = "Instruct: X\nQuery: "

	item := *newItem()
	item.Status = store.StatusPending
	st := &fakeStore{pending: &item}
	qd := &fakeQdrant{}
	w := newTestWorker(&item, &fakeLLM{}, emb, qd, st)
	if _, err := w.processPending(context.Background()); err != nil {
		t.Fatalf("processPending: %v", err)
	}
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("want 1 embed call with 1 text, got %v", got)
	}
	if got[0][0] != item.OriginalText {
		t.Fatalf("write path must embed raw text (no query instruction), got %q want %q", got[0][0], item.OriginalText)
	}
	if len(qd.points) != 1 {
		t.Fatalf("want 1 original point, got %d", len(qd.points))
	}
}

// TestProcessEmbeddedSparseDisabledLegacyWire：开关关闭（AGMEM_HYBRID=off /
// 缺省 worker）→ 点位不带稀疏，wire 保持纯稠密数组形态（存量兼容）。
func TestProcessEmbeddedSparseDisabledLegacyWire(t *testing.T) {
	glmOut := "## 步骤\n" + rep('甲', 20)
	st := &fakeStore{item: newItem()}
	qd := &fakeQdrant{}
	w := newTestWorker(st.item, &fakeLLM{out: glmOut}, &fakeEmbedder{}, qd, st)
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if len(qd.points) != 1 || len(qd.points[0].Sparse) != 0 {
		t.Fatalf("SparseEnabled=false 时不得写稀疏向量, got %+v", qd.points)
	}
}

// —— G01：来源行必出 + payload.source_doc ——

// TestProcessEmbeddedSourceLineFromResolver：注入 SourceResolver 时来源行与
// payload.source_doc 均取 documents.source_name。
func TestProcessEmbeddedSourceLineFromResolver(t *testing.T) {
	item := newItem()
	item.OriginalText = "## 某文档\n正文内容"
	st := &fakeStore{item: item}
	qd := &fakeQdrant{}
	llm := &fakeLLM{out: "## 部署步骤\n" + rep('步', 30)}
	w := newTestWorker(item, llm, &fakeEmbedder{}, qd, st)
	w.Sources = fakeSourceResolver{name: "k8s-deploy.md"}
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if llm.source != "k8s-deploy.md" {
		t.Errorf("LLM 收到来源名 = %q, want k8s-deploy.md", llm.source)
	}
	if len(qd.points) != 1 {
		t.Fatalf("want 1 point, got %d", len(qd.points))
	}
	p := qd.points[0]
	if p.Payload.SourceDoc != "k8s-deploy.md" {
		t.Errorf("payload.source_doc = %q, want k8s-deploy.md", p.Payload.SourceDoc)
	}
	if !strings.Contains(p.Payload.Text, "来源：k8s-deploy.md") {
		t.Errorf("条目缺规范来源行: %q", p.Payload.Text)
	}
}

// TestProcessEmbeddedSourceLineFallbackTitle：无 resolver 时回退原文首行标题。
func TestProcessEmbeddedSourceLineFallbackTitle(t *testing.T) {
	item := newItem()
	item.OriginalText = "# 我的文档\n正文内容"
	st := &fakeStore{item: item}
	qd := &fakeQdrant{}
	w := newTestWorker(item, &fakeLLM{out: "## 部署步骤\n" + rep('步', 30)}, &fakeEmbedder{}, qd, st)
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if len(qd.points) != 1 {
		t.Fatalf("want 1 point, got %d", len(qd.points))
	}
	if got := qd.points[0].Payload.SourceDoc; got != "我的文档" {
		t.Errorf("payload.source_doc = %q, want 我的文档", got)
	}
	if !strings.Contains(qd.points[0].Payload.Text, "来源：我的文档") {
		t.Errorf("条目缺来源行: %q", qd.points[0].Payload.Text)
	}
}

// —— G02：清洗后锚点校验 ——

// TestProcessEmbeddedAnchorRecleanSuccess：首轮丢锚点 → 严格重清一次带锚点约束
// 成功 → 正常入库，且锚点清单传入 LLM。
func TestProcessEmbeddedAnchorRecleanSuccess(t *testing.T) {
	item := newItem()
	item.OriginalText = "服务监听 192.0.2.162:1194/UDP"
	st := &fakeStore{item: item}
	qd := &fakeQdrant{}
	llm := &fakeLLM{
		out:       "## 监听配置\n服务监听 92.0.2.162:1194/UDP 的改写",
		strictOut: "## 监听配置\n服务监听 192.0.2.162:1194/UDP 原样保留",
	}
	w := newTestWorker(item, llm, &fakeEmbedder{}, qd, st)
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if len(st.failed) != 0 {
		t.Fatalf("严格重清成功后不得失败: %+v", st.failed)
	}
	if len(qd.points) == 0 {
		t.Fatal("应落 clean 点")
	}
	if !strings.Contains(qd.points[0].Payload.Text, "192.0.2.162") {
		t.Errorf("重清后条目仍缺锚点: %q", qd.points[0].Payload.Text)
	}
	if !reflect.DeepEqual(llm.anchors, []string{"192.0.2.162"}) {
		t.Errorf("严格重清锚点清单 = %v, want [192.0.2.162]", llm.anchors)
	}
}

// TestProcessEmbeddedAnchorMissingFails：严格重清后仍缺锚点 → failed，错误信息
// 含缺失清单（可重试）。
func TestProcessEmbeddedAnchorMissingFails(t *testing.T) {
	item := newItem()
	item.OriginalText = "服务监听 192.0.2.162:1194/UDP"
	st := &fakeStore{item: item}
	qd := &fakeQdrant{}
	llm := &fakeLLM{
		out:       "## 监听配置\n服务监听 92.0.2.162:1194/UDP 的改写",
		strictOut: "## 监听配置\n仍缺 IP 的改写内容",
	}
	w := newTestWorker(item, llm, &fakeEmbedder{}, qd, st)
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if len(qd.points) != 0 {
		t.Errorf("锚点缺失不得落点, got %+v", qd.points)
	}
	if len(st.failed) != 1 || !strings.Contains(st.failed[0].msg, "anchor verify") {
		t.Fatalf("fail = %+v, want anchor verify", st.failed)
	}
	if !strings.Contains(st.failed[0].msg, "192.0.2.162") {
		t.Errorf("失败信息应含缺失锚点清单: %q", st.failed[0].msg)
	}
	if !reflect.DeepEqual(llm.anchors, []string{"192.0.2.162"}) {
		t.Errorf("严格重清锚点清单 = %v", llm.anchors)
	}
}

// TestProcessEmbeddedEncryptedOriginalSkipsAnchors：加密原文（base64 blob）跳过
// 锚点校验，正常入库。
func TestProcessEmbeddedEncryptedOriginalSkipsAnchors(t *testing.T) {
	item := newItem()
	item.OriginalText = strings.Repeat("A", 300)
	st := &fakeStore{item: item}
	qd := &fakeQdrant{}
	w := newTestWorker(item, &fakeLLM{out: "## 说明\n这是一段加密内容的清洗结果描述文字"}, &fakeEmbedder{}, qd, st)
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if len(st.failed) != 0 {
		t.Fatalf("加密原文不应触发锚点失败: %+v", st.failed)
	}
	if len(qd.points) != 1 {
		t.Fatalf("want 1 point, got %d", len(qd.points))
	}
}

// —— G11：item 级 ≤3 次指数退避自动重排 ——

// TestFailRetryableBackoffRequeue：失败 → 记退避（不到点不重排）→ 到点 RequeueItem
// 重排；第 3 次失败达上限 → retry_exhausted，不再重排。
func TestFailRetryableBackoffRequeue(t *testing.T) {
	st := &fakeStore{}
	var events []DegradeEvent
	w := &Worker{
		Store:       st,
		RetryBase:   10 * time.Second,
		MaxAttempts: 3,
		OnDegrade:   func(ev DegradeEvent) { events = append(events, ev) },
	}
	now := time.Unix(1000, 0)
	w.clock = func() time.Time { return now }
	item := newItem()
	ctx := context.Background()

	// 第一次失败：退避 10s，未到点不重排
	if err := w.failRetryable(ctx, item, "glm clean", errors.New("boom1")); err != nil {
		t.Fatalf("failRetryable: %v", err)
	}
	w.requeueDue(ctx)
	if len(st.requeued) != 0 {
		t.Fatalf("退避未到点不得重排: %+v", st.requeued)
	}
	now = now.Add(11 * time.Second)
	w.requeueDue(ctx)
	if !reflect.DeepEqual(st.requeued, []requeueCall{{kbID: 1, itemID: 42}}) {
		t.Fatalf("到点应重排 item 42: %+v", st.requeued)
	}

	// 第二次失败：退避 20s
	if err := w.failRetryable(ctx, item, "glm clean", errors.New("boom2")); err != nil {
		t.Fatalf("failRetryable: %v", err)
	}
	w.requeueDue(ctx)
	if len(st.requeued) != 1 {
		t.Fatalf("第二次退避未到点不得重排: %+v", st.requeued)
	}
	now = now.Add(21 * time.Second)
	w.requeueDue(ctx)
	if len(st.requeued) != 2 {
		t.Fatalf("第二次到点应重排: %+v", st.requeued)
	}

	// 第三次失败：达上限，终败不再重排
	if err := w.failRetryable(ctx, item, "glm clean", errors.New("boom3")); err != nil {
		t.Fatalf("failRetryable: %v", err)
	}
	now = now.Add(1 * time.Hour)
	w.requeueDue(ctx)
	if len(st.requeued) != 2 {
		t.Fatalf("达上限后不得再重排: %+v", st.requeued)
	}
	if len(st.failed) != 3 {
		t.Fatalf("应有 3 次失败记录: %+v", st.failed)
	}
	var scheduled, exhausted int
	for _, ev := range events {
		switch ev.Kind {
		case "retry_scheduled":
			scheduled++
		case "retry_exhausted":
			exhausted++
		}
	}
	if scheduled != 2 || exhausted != 1 {
		t.Errorf("degrade 事件 scheduled=%d exhausted=%d, want 2/1: %+v", scheduled, exhausted, events)
	}
}

// TestClearRetryOnSuccess：成功路径清除退避状态，不再重排。
func TestClearRetryOnSuccess(t *testing.T) {
	st := &fakeStore{item: newItem()}
	qd := &fakeQdrant{}
	w := newTestWorker(st.item, &fakeLLM{out: "## 步骤\n" + rep('步', 30)}, &fakeEmbedder{}, qd, st)
	w.retries = map[int64]*retryState{42: {kbID: 1, itemID: 42, attempts: 1}}
	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if len(w.retries) != 0 {
		t.Errorf("成功后应清除退避状态: %+v", w.retries)
	}
}

// —— S1 前缀接线：轮次冻结 / 数据源透传 / 回退 ——

// fakePrefixSource 内存版 PrefixSource：记录收到的 kbID，返回固定条目快照。
type fakePrefixSource struct {
	calls []int64
	rows  []KnowledgeEntry
	err   error
}

func (f *fakePrefixSource) KnowledgeEntries(_ context.Context, kbID int64) ([]KnowledgeEntry, error) {
	f.calls = append(f.calls, kbID)
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// fakePrefixLLM 同时实现 LLM 与 PrefixCleaner，记录最后一次前缀/目标与调用次数。
type fakePrefixLLM struct {
	out        string
	err        error
	calls      int
	lastPrefix Prefix
	lastTarget string
}

func (f *fakePrefixLLM) Clean(context.Context, string) (string, error) { return f.finish() }

func (f *fakePrefixLLM) CleanWithSource(context.Context, string, string) (string, error) {
	return f.finish()
}

func (f *fakePrefixLLM) CleanWithPrefix(_ context.Context, prefix Prefix, targetDoc, sourceName, text string, anchors []string) (string, CleanUsage, error) {
	f.calls++
	f.lastPrefix = prefix
	f.lastTarget = targetDoc
	if f.err != nil {
		return "", CleanUsage{}, f.err
	}
	return f.out, CleanUsage{PromptTokens: 1234, PromptCacheHitTokens: 1200, PromptCacheMissTokens: 34, ReasoningTokens: 7}, nil
}

func (f *fakePrefixLLM) finish() (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.out, nil
}

// TestWorker_PrefixRoundFreeze 轮次冻结：轮 = (kb, 目标目录)。同一目录内只取一次条目
// 快照并复用同一 Prefix（轮内逐字节稳定）；目录切换才重建。
func TestWorker_PrefixRoundFreeze(t *testing.T) {
	src := &fakePrefixSource{rows: []KnowledgeEntry{
		{ItemID: 1, DocName: "x.md", Dir: "other-clean", Text: "背景知识"},
	}}
	w := &Worker{
		LLM:           &fakePrefixLLM{},
		PrefixSource:  src,
		PrefixBuilder: &PrefixBuilder{},
		DirResolver:   func(id int64) string { return map[int64]string{10: "d1", 11: "d2"}[id] },
	}
	ctx := context.Background()

	itemA := &store.TaskItem{ID: 1, KBID: 7, DirID: 10}
	r1 := w.newCleanRun(ctx, itemA)
	r2 := w.newCleanRun(ctx, itemA)
	if !r1.usePrefix || !r2.usePrefix {
		t.Fatalf("前缀未启用：%+v %+v", r1, r2)
	}
	if r1.prefix.Text != r2.prefix.Text || r1.prefix.Hash != r2.prefix.Hash {
		t.Fatalf("同轮前缀不稳定")
	}
	if len(src.calls) != 1 || src.calls[0] != 7 {
		t.Fatalf("同一 (kb,目录) 应只取一次快照且 kbID 透传：%v", src.calls)
	}

	// 目录切换 → 重建（再取一次快照）。
	if r := w.newCleanRun(ctx, &store.TaskItem{ID: 2, KBID: 7, DirID: 11}); !r.usePrefix {
		t.Fatalf("切换目录后前缀未启用")
	}
	if len(src.calls) != 2 {
		t.Fatalf("目录切换应重建快照：%v", src.calls)
	}
}

// TestWorker_PrefixSourceErrorFallsBack：取条目失败时回退无前缀清洗（前缀是增强，
// 不阻断清洗），不 panic。
func TestWorker_PrefixSourceErrorFallsBack(t *testing.T) {
	w := &Worker{
		LLM:          &fakePrefixLLM{},
		PrefixSource: &fakePrefixSource{err: errors.New("db down")},
		DirResolver:  func(int64) string { return "d" },
	}
	if r := w.newCleanRun(context.Background(), &store.TaskItem{ID: 1, KBID: 1, DirID: 1}); r.usePrefix {
		t.Fatalf("取条目失败应回退无前缀清洗")
	}
}

// TestWorker_NoPrefixSourceFallsBack：未注入 PrefixSource 或 LLM 不实现 PrefixCleaner
// 时回退无前缀路径，且不 panic（「前缀未启用」只记一次日志标志）。
func TestWorker_NoPrefixSourceFallsBack(t *testing.T) {
	w := &Worker{LLM: &fakeLLM{out: "x"}}
	if r := w.newCleanRun(context.Background(), &store.TaskItem{ID: 1}); r.usePrefix {
		t.Fatalf("无 PrefixSource 不应启用前缀")
	}
	if !w.prefixLogged {
		t.Fatalf("未启用前缀应置日志标志")
	}
}

// TestProcessEmbeddedWithPrefix：带前缀清洗端到端——worker 把轮内冻结前缀传给
// PrefixCleaner（L1 排除目标目录、L2 接在 L1 后），并把 usage 透出到 OnCleanStats。
func TestProcessEmbeddedWithPrefix(t *testing.T) {
	glmOut := "## 目标条目\n" + rep('甲', 380)
	st := &fakeStore{item: newItem()} // DirID=3 → cleanDirName "sre-clean"
	src := &fakePrefixSource{rows: []KnowledgeEntry{
		{ItemID: 9, DocName: "old.md", Dir: "sre-clean", Text: "同目录旧条目"},
		{ItemID: 8, DocName: "other.md", Dir: "k8s-clean", Text: "别目录条目"},
	}}
	llm := &fakePrefixLLM{out: glmOut}
	w := newTestWorker(st.item, llm, &fakeEmbedder{}, &fakeQdrant{}, st)
	w.PrefixSource = src
	w.PrefixBuilder = &PrefixBuilder{}
	w.Sources = fakeSourceResolver{name: "src.md"}
	var stats []CleanStats
	w.OnCleanStats = func(s CleanStats) { stats = append(stats, s) }

	if _, err := w.processEmbedded(context.Background()); err != nil {
		t.Fatalf("processEmbedded: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("PrefixCleaner 调用次数 = %d, want 1", llm.calls)
	}
	if strings.Contains(llm.lastPrefix.L1Text, "同目录旧条目") {
		t.Fatalf("目标目录条目不应出现在 L1：%q", llm.lastPrefix.L1Text)
	}
	if !strings.Contains(llm.lastPrefix.L1Text, "别目录条目") {
		t.Fatalf("别目录条目应出现在 L1：%q", llm.lastPrefix.L1Text)
	}
	if !strings.Contains(llm.lastPrefix.Text, "同目录旧条目") {
		t.Fatalf("目标目录条目应出现在 L2：%q", llm.lastPrefix.Text)
	}
	if llm.lastTarget != "src.md" {
		t.Fatalf("目标文档标识 = %q, want src.md（无 URL，回落文档名）", llm.lastTarget)
	}
	if len(stats) != 1 {
		t.Fatalf("OnCleanStats 调用次数 = %d, want 1", len(stats))
	}
	if stats[0].PrefixHash != llm.lastPrefix.Hash {
		t.Fatalf("OnCleanStats 前缀 hash 不一致：%q vs %q", stats[0].PrefixHash, llm.lastPrefix.Hash)
	}
	if stats[0].PromptTokens != 1234 || stats[0].CacheHitTokens != 1200 || stats[0].ReasoningTokens != 7 {
		t.Fatalf("usage 未透出：%+v", stats[0])
	}
}

// —— 超长 original 分段 embedding（2026-09-10 线上缺陷修复）——

// TestProcessPendingChunksLongOriginal：原文超 DocChunkRunes → 按 SplitDocChunks
// 切段、逐段 embed、合成 1 个 original 点（向量 = 各段均值 L2 归一），payload 仍存
// 整篇原文。
func TestProcessPendingChunksLongOriginal(t *testing.T) {
	long := strings.Repeat("这是一段很长的原文内容。", 400) // 12 rune × 400 = 4800 rune
	item := *newItem()
	item.Status = store.StatusPending
	item.OriginalText = long
	st := &fakeStore{pending: &item}
	qd := &fakeQdrant{}
	emb := &fakeEmbedder{}
	w := newTestWorker(&item, &fakeLLM{}, emb, qd, st)

	if _, err := w.processPending(context.Background()); err != nil {
		t.Fatalf("processPending: %v", err)
	}
	if len(emb.batches) != 1 {
		t.Fatalf("want 1 embed 调用，got %d", len(emb.batches))
	}
	chunks := emb.batches[0]
	if len(chunks) <= 1 {
		t.Fatalf("超长原文应切多段，得到 %d 段", len(chunks))
	}
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > wikisync.DocChunkRunes {
			t.Fatalf("第 %d 段 %d rune 超上限 %d", i, n, wikisync.DocChunkRunes)
		}
	}
	if stripSpace(strings.Join(chunks, "\n")) != stripSpace(long) {
		t.Fatalf("切段有损：去空白后与原文不相等")
	}
	if len(qd.points) != 1 {
		t.Fatalf("want 1 original 点，got %d", len(qd.points))
	}
	p := qd.points[0]
	if p.Payload.Text != long {
		t.Errorf("original payload 必须保留整篇原文")
	}
	if len(p.Vector) != 2 {
		t.Fatalf("向量维度 = %d, want 2（fakeEmbedder 产出）", len(p.Vector))
	}
	// fakeEmbedder 对入参 texts 返回 [len, i]：均值 = [k, (k-1)/2]，再 L2 归一。
	k := float64(len(chunks))
	sx, sy := k, (k-1)/2
	norm := math.Sqrt(sx*sx + sy*sy)
	if math.Abs(float64(p.Vector[0])-sx/norm) > 1e-6 || math.Abs(float64(p.Vector[1])-sy/norm) > 1e-6 {
		t.Errorf("向量 = %v, want [%v %v]（各段均值 L2 归一）", p.Vector, sx/norm, sy/norm)
	}
}

// TestProcessPendingShortOriginalSingleEmbed：正常长度（≤DocChunkRunes）行为不变——
// 整篇一次 embed，不切段。
func TestProcessPendingShortOriginalSingleEmbed(t *testing.T) {
	item := *newItem() // OriginalText = "原始文本"（4 rune）
	item.Status = store.StatusPending
	st := &fakeStore{pending: &item}
	qd := &fakeQdrant{}
	emb := &fakeEmbedder{}
	w := newTestWorker(&item, &fakeLLM{}, emb, qd, st)

	if _, err := w.processPending(context.Background()); err != nil {
		t.Fatalf("processPending: %v", err)
	}
	if len(emb.batches) != 1 || len(emb.batches[0]) != 1 || emb.batches[0][0] != item.OriginalText {
		t.Fatalf("短原文应整篇一次 embed，got %#v", emb.batches)
	}
	if len(qd.points) != 1 {
		t.Fatalf("want 1 original 点，got %d", len(qd.points))
	}
}

// TestProcessPendingEmbedErrorReportsLength：embed 失败时错误串须带实际尝试的文本
// 长度（区分超限与网络），不再是只有「aigateway 主通道失败」。
func TestProcessPendingEmbedErrorReportsLength(t *testing.T) {
	long := strings.Repeat("x", 9000)
	item := *newItem()
	item.Status = store.StatusPending
	item.OriginalText = long
	st := &fakeStore{pending: &item}
	w := newTestWorker(&item, &fakeLLM{}, &fakeEmbedder{fail: errors.New("boom")}, &fakeQdrant{}, st)

	if _, err := w.processPending(context.Background()); err != nil {
		t.Fatalf("processPending: %v", err)
	}
	if len(st.failed) != 1 {
		t.Fatalf("want 1 failed 记录，got %d", len(st.failed))
	}
	if msg := st.failed[0].msg; !strings.Contains(msg, "9000") {
		t.Errorf("失败原因须含实际尝试文本长度 9000，got %q", msg)
	}
}
