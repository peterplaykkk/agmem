// Package cleaner：异步清洗 worker。
//
// 管线：
//
//	pending --[embed original + upsert point_type=original]--> embedded
//	embedded --[claim]--> cleaning --[GLM 清洗 + embed clean + upsert point_type=clean]--> cleaned
//	任一环节失败 --> failed（error 记录原因）
//
// 清洗成功且条目全部完成后递增 kb_versions（agmem 缓存版本校验依据）。
package cleaner

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"sre/agmem/internal/bm25"
	"sre/agmem/internal/qdrant"
	"sre/agmem/internal/store"
	"sre/agmem/internal/wikisync"
)

// LLM 清洗模型接口（GLM 走 aigateway，测试可注入假实现）。
type LLM interface {
	// Clean 把原始文本规范化为记忆条目/执行步骤；失败返回 error。
	Clean(ctx context.Context, text string) (string, error)
}

// SourceCleaner 可选能力：清洗时注入入库文档名（G01：来源行值取 source_name）。
// GLMClient 实现；未实现时回退 LLM.Clean（旧行为）。
type SourceCleaner interface {
	CleanWithSource(ctx context.Context, text, sourceName string) (string, error)
}

// AnchorCleaner 可选能力：带锚点强约束的严格重清（G02：锚点校验失败后重清一次）。
type AnchorCleaner interface {
	CleanWithAnchors(ctx context.Context, text, sourceName string, anchors []string) (string, error)
}

// PrefixCleaner 可选能力：带逐字节不变前缀的清洗（S2）。GLMClient 实现；返回本次
// 请求的 usage 供观测。未实现时 worker 回退 SourceCleaner/LLM 既有路径。
type PrefixCleaner interface {
	CleanWithPrefix(ctx context.Context, prefix Prefix, targetDoc, sourceName, text string, anchors []string) (string, CleanUsage, error)
}

// CleanStats 一次带前缀清洗的观测（S3）：前缀体积/hash/分层（调用方侧）与网关 usage。
type CleanStats struct {
	PrefixHash      string
	PrefixTokens    int
	L1Tokens        int // L1 段（L0 契约 + L1 条目）token 估算
	L2Tokens        int // L2 段（目录上下文）token 估算
	DirDepth        int // L2 实际装入的目录层数
	PromptTokens    int
	CacheHitTokens  int
	CacheMissTokens int
	ReasoningTokens int
}

// SourceResolver 解析条目对应的入库文档名（internal/store 提供
// SourceNameForItem：join task_items→documents.source_name）。cleaner 侧小接口
// 注入，避免直接依赖 store 包实现；未注入时回退原文首行标题。
type SourceResolver interface {
	SourceNameForItem(ctx context.Context, itemID int64) (string, error)
}

// DegradeEvent 降级/重试事件（结构化记录：模型降级、think 降级、锚点重清、
// item 级退避重排与终败）。
type DegradeEvent struct {
	Kind    string // model_fallback | think_downgrade | anchor_missing | anchor_reclean | retry_scheduled | retry_requeue | retry_exhausted
	ItemID  int64
	Stage   string
	From    string
	To      string
	Attempt int
	Reason  string
}

// logDegrade 结构化输出降级事件（单行 key=value，便于日志检索）。
func logDegrade(ev DegradeEvent) {
	log.Printf("cleaner: degrade kind=%s item=%d stage=%s from=%s to=%s attempt=%d reason=%q",
		ev.Kind, ev.ItemID, ev.Stage, ev.From, ev.To, ev.Attempt, ev.Reason)
}

// ItemStore worker 所需存储的最小接口（*store.Store 满足；测试注入内存假实现）。
type ItemStore interface {
	ClaimPending(ctx context.Context) (*store.TaskItem, error)
	ClaimEmbedded(ctx context.Context) (*store.TaskItem, error)
	CompleteCleaned(ctx context.Context, itemID int64, cleanText, cleanPointID string) error
	FailItem(ctx context.Context, itemID int64, status store.TaskStatus, msg string) error
	// RequeueItem failed → pending（G11：item 级退避到点后自动重排）。
	RequeueItem(ctx context.Context, kbID, itemID int64) (store.TaskStatus, error)
	BumpVersion(ctx context.Context, kbID int64) (uint64, error)
	RefreshTaskStatus(ctx context.Context, taskID int64) (store.TaskStatus, error)
}

// Embedder worker 所需 embedding 最小接口（*embedding.Client 满足；返回向量与
// 输入文本一一对应、保持顺序）。
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// PointSink worker 所需 qdrant 写入最小接口（*qdrant.Client 满足；测试注入假实现）。
type PointSink interface {
	Upsert(ctx context.Context, collection string, points []qdrant.Point) error
}

// Worker 轮询 MySQL 任务表并推进状态机。
type Worker struct {
	Store    ItemStore
	Embedder Embedder
	Qdrant   PointSink
	LLM      LLM
	// CollectionResolver kb_id → qdrant collection 名（= kb 名，多 kb 各自 collection）。
	CollectionResolver func(kbID int64) string
	Interval           time.Duration
	// DirResolver 把 dir_id 解析为目录名；clean 条目目录名 = `<dir>-clean`。
	// 待评审：payload.dir 存目录名还是 dir_id；切换只改本函数注入。
	DirResolver func(dirID int64) string
	// SparseEnabled 混合检索写入开关（AGMEM_HYBRID!=off 时由 agmemserv 注入）：
	// clean 点额外计算 bm25 稀疏向量一并 upsert。前置条件：目标 collection 已
	// 配置 bm25 命名稀疏向量（EnsureCollection/EnsureSparseVector 已幂等补齐，
	// 上传路径建任务前必经）——对未配置的 collection 写命名稀疏向量会被 qdrant
	// 400 拒绝，届时整体回退 AGMEM_HYBRID=off（查询与写入同时回旧路径）。
	// original 点不写稀疏向量（检索只搜 clean，省一次编码与存储）。
	SparseEnabled bool
	// Sources 入库文档名解析器（可选）：非 nil 时来源行取 documents.source_name；
	// nil 或解析为空时回退原文首行标题，再无则 unnamedSource。
	Sources SourceResolver
	// MaxAttempts item 级最大尝试次数（含首次）；≤0 默认 3。失败后按指数退避
	// 自动重排（RequeueItem failed→pending），达上限标记终败。
	MaxAttempts int
	// RetryBase 退避基数（首次退避 = 基数，之后翻倍）；≤0 默认 30s。
	RetryBase time.Duration
	// OnDegrade 降级/重试事件钩子（可选）；未设置时仅结构化日志。
	OnDegrade func(DegradeEvent)
	// PrefixSource 已清洗条目来源（S1）；非 nil 且 LLM 实现 PrefixCleaner 时启用
	// 不变前缀清洗，否则回退既有 CleanWithSource/CleanWithAnchors 路径。生产在
	// cmd/agmemserv 注入 store 适配器；未注入时 worker 明确记一次日志（不静默降级）。
	PrefixSource PrefixSource
	// PrefixBuilder 前缀构建器（S1）；nil 时用默认预算（DefaultPrefixBudgetTokens、
	// 仅同目录，父目录钩子缺省为 nil）。
	PrefixBuilder *PrefixBuilder
	// TargetDocResolver 解析当前条目的目标文档标识（L3 【本次提取目标文档】）。未
	// 注入时回退 resolveSourceName（入库文档名）——与覆盖声明同一套文档名标识
	// （无 wiki URL，见 prefix.go 文件头「已知偏差」）。
	TargetDocResolver func(ctx context.Context, item *store.TaskItem) string
	// OnCleanStats 带前缀清洗观测钩子（S3）；未设置时仅结构化日志。
	OnCleanStats func(CleanStats)
	// BaseBackoff 空轮询退避基数（CI MySQL 审计 F5，2026-09-30）：连续空领取时
	// 轮询间隔从基数起指数翻倍（1s→2s→4s→…）；≤0 默认 1s。有效下限为 Interval
	// （退避不会快于满速）。
	BaseBackoff time.Duration
	// MaxBackoff 空轮询退避上限；≤0 默认 30s。稳态空闲开销 = 每步 2 条 claim 探测
	// SELECT / 30s ≈ 0.07 QPS（基线恒频 ≈1 QPS）。
	MaxBackoff time.Duration

	wake     chan struct{} // Poke 唤醒通道（惰性创建，容量 1；见 wakeCh）
	wakeOnce sync.Once

	retryMu  sync.Mutex
	retries  map[int64]*retryState
	prefixMu sync.Mutex
	// prefixSess 当前轮前缀（见 prefixFor / CLAUDE.md §2、§3.1）：**轮 = (kb, 目标
	// 目录)**，同一轮内复用同一个冻结 Prefix，目录（或 kb）切换才重建。冻结 = 构建
	// 时取一次 entries 快照，本轮不再重读。按预算换轮（真正的轮次切分）留待后续。
	prefixSess *prefixSession
	// prefixLogged 前缀未启用/降级的日志只记一次，避免每个条目刷屏。
	prefixLogged bool
	clock        func() time.Time
}

// prefixSession 缓存当前轮（kb + 目标目录）的冻结前缀（见 Worker.prefixSess）。
type prefixSession struct {
	kbID   int64
	dir    string
	prefix Prefix
}

// retryState 单条目退避状态（内存态：重启后未到期的重排丢失，需人工 requeue）。
type retryState struct {
	kbID     int64
	itemID   int64
	taskID   int64
	attempts int
	nextAt   time.Time // 零值 = 已重排、等待再次失败后才安排下一轮
}

// Run 阻塞运行直到 ctx 取消。
//
// 轮询节奏（F5 空轮询退避）：满速间隔为 Interval；本轮空领取（pending+embedded
// 均无活）则下一轮按 pollDelay 指数退避；领取到条目或收到 Poke（新任务入队）
// 立即恢复满速。
func (w *Worker) Run(ctx context.Context) {
	timer := time.NewTimer(w.interval())
	defer timer.Stop()
	streak := 0 // 连续空领取轮数（0 = 满速）
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wakeCh():
			// 新任务入队信号：恢复满速，把下一轮提前到满速间隔。
			streak = 0
			if !timer.Stop() { // 计时器已到期：排空残留 tick 防重复步进
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(w.interval())
		case <-timer.C:
			if w.step(ctx) {
				streak = 0
			} else {
				streak++
			}
			timer.Reset(w.pollDelay(streak))
		}
	}
}

// wakeCh 惰性创建唤醒通道（容量 1：Poke 幂等，运行中只需一个待处理信号）。
func (w *Worker) wakeCh() chan struct{} {
	w.wakeOnce.Do(func() { w.wake = make(chan struct{}, 1) })
	return w.wake
}

// Poke 通知 worker 有新任务入队：立即恢复满速轮询（非阻塞，可从任意 goroutine
// 调用；worker 未运行时无害）。
func (w *Worker) Poke() {
	select {
	case w.wakeCh() <- struct{}{}:
	default: // 已有待处理唤醒，无需重复
	}
}

// interval 满速轮询间隔（未配置时回落 2s，与 config 默认一致；旧 ticker 对
// 非正值会 panic，这里改为安全回落）。
func (w *Worker) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return 2 * time.Second
}

// pollDelay 计算连续空领取 streak 轮后的下一轮延迟。
func (w *Worker) pollDelay(streak int) time.Duration {
	return backoffDelay(streak, w.interval(), w.baseBackoff(), w.maxBackoff())
}

func (w *Worker) baseBackoff() time.Duration {
	if w.BaseBackoff > 0 {
		return w.BaseBackoff
	}
	return time.Second
}

func (w *Worker) maxBackoff() time.Duration {
	if w.MaxBackoff > 0 {
		return w.MaxBackoff
	}
	return 30 * time.Second
}

// backoffDelay 指数退避纯函数（可测）：streak<=0 → fast（满速）；否则
// max(fast, base<<(streak-1)) 封顶 limit。用循环翻倍而非移位，streak 极大时
// 不溢出。默认参数下的有效序列（Interval=2s）：2s,2s,4s,8s,16s,30s,30s,…——
// 前两级被满速下限抬高，与「1s→2s→4s→…→cap 30s」同范式。
func backoffDelay(streak int, fast, base, limit time.Duration) time.Duration {
	if streak <= 0 {
		return fast
	}
	d := base
	for i := 1; i < streak; i++ {
		d *= 2
		if d >= limit {
			return limit
		}
	}
	if d < fast {
		d = fast
	}
	if d > limit {
		d = limit // 防御：Interval > MaxBackoff 的退化配置下，以上限为准
	}
	return d
}

// step 执行一轮：先重排到期的退避条目，再嵌入原始文本，最后做清洗。
// 返回本轮是否领取到条目（领取成功即复位空轮询退避；与处理成败无关——
// 处理失败走 item 级 retry 退避，见 failRetryable）。
func (w *Worker) step(ctx context.Context) bool {
	w.requeueDue(ctx)
	didPending, err := w.processPending(ctx)
	if err != nil {
		log.Printf("cleaner: pending step: %v", err)
	}
	didEmbedded, err := w.processEmbedded(ctx)
	if err != nil {
		log.Printf("cleaner: embedded step: %v", err)
	}
	return didPending || didEmbedded
}

// processPending：取一条 pending 项，embedding 原始文本并 upsert point_type=original。
// 返回（是否领取到条目, 处理错误）；ErrNotFound 返回 (false, nil)。
func (w *Worker) processPending(ctx context.Context) (bool, error) {
	item, err := w.Store.ClaimPending(ctx)
	if err != nil {
		return false, nil // ErrNotFound 或竞争失败：本轮无事可做
	}
	vec, err := w.embedOriginal(ctx, item.OriginalText)
	if err != nil {
		return true, w.failRetryable(ctx, item, "embed original", err)
	}
	collection, err := w.collection(ctx, item.KBID)
	if err != nil {
		return true, w.fail(ctx, item, "resolve collection", err)
	}
	pointID := newPointID(item.ID, "original")
	if err := w.Qdrant.Upsert(ctx, collection, []qdrant.Point{{
		ID:     pointID,
		Vector: vec,
		Payload: qdrant.PointPayload{
			Dir:       w.dirName(item.DirID),
			PointType: "original",
			TaskID:    item.TaskID,
			KB:        collection, // 来源 kb 标注（--global 跨库检索用）
			ItemID:    item.ID,
			Text:      item.OriginalText,
		},
	}}); err != nil {
		return true, w.failRetryable(ctx, item, "upsert original", err)
	}
	w.clearRetry(item.ID)
	return true, nil // 状态已在 ClaimPending 推进为 embedded
}

// embedOriginal 对 original 文本取单个向量：不超过单段上限（wikisync.DocChunkRunes）
// 时整篇一次 embedding；超限时按同一 SplitDocChunks 切段、逐段 embedding，再取各段
// 向量逐维平均并 L2 归一，合成一个 original 点向量。
//
// 背景（2026-09-10 线上缺陷修复）：入库切段只作用于 wiki 同步路径
// （wikisync.ingest → SplitDocChunks，每段 ≤4000 rune）；POST /docs 与迁移工具可
// 写入任意长度原文，worker 此前对整篇调 embedding，大文档单请求超限/超时 → 该条
// 反复失败直至终败（实测 item 1155 原文 20691 字、item 1196 104795 字，均报
// "embed original"）。本函数是所有 original 入库的公共收口，故在 embed 前统一保护。
//
// 为什么取「各段向量平均 + L2 归一」而非「只 embed 首段」：original 点是文档级
// 语义检索入口，平均向量覆盖全篇内容；只 embed 首段会让长文后半段在 original 检索
// 路径上不可达。代价是多花 N-1 次 embedding 调用（每篇一次性、可接受）。payload.Text
// 仍保留整篇原文，不受本函数影响。
//
// 失败时把实际尝试的文本长度（与切段数）写进 error，便于区分「超限」与「网络」。
func (w *Worker) embedOriginal(ctx context.Context, text string) ([]float32, error) {
	n := runeLen(text)
	if n <= wikisync.DocChunkRunes {
		vecs, err := w.Embedder.Embed(ctx, []string{text})
		if err != nil {
			return nil, fmt.Errorf("文本 %d 字: %w", n, err)
		}
		if len(vecs) == 0 {
			return nil, fmt.Errorf("文本 %d 字: embedding 未返回向量", n)
		}
		return vecs[0], nil
	}
	chunks := wikisync.SplitDocChunks(text, wikisync.DocChunkRunes)
	if len(chunks) <= 1 {
		// 防御：理论不可达（runeLen > DocChunkRunes 必切出 ≥2 段）。退回整篇，
		// 保持与旧行为一致而不是 panic。
		vecs, err := w.Embedder.Embed(ctx, []string{text})
		if err != nil {
			return nil, fmt.Errorf("文本 %d 字: %w", n, err)
		}
		if len(vecs) == 0 {
			return nil, fmt.Errorf("文本 %d 字: embedding 未返回向量", n)
		}
		return vecs[0], nil
	}
	vecs, err := w.Embedder.Embed(ctx, chunks)
	if err != nil {
		return nil, fmt.Errorf("文本 %d 字（切 %d 段）: %w", n, len(chunks), err)
	}
	if len(vecs) != len(chunks) {
		return nil, fmt.Errorf("文本 %d 字（切 %d 段）: embedding 返回 %d 个向量，数量不符",
			n, len(chunks), len(vecs))
	}
	return meanL2(vecs), nil
}

// meanL2 对一组等长向量逐维取算术平均，再按 L2 范数归一（零向量原样返回均值）。
// 长度不一致的向量被跳过；全部跳过时返回等长零向量。
func meanL2(vecs [][]float32) []float32 {
	dim := len(vecs[0])
	acc := make([]float64, dim)
	n := 0
	for _, v := range vecs {
		if len(v) != dim {
			continue
		}
		for i, x := range v {
			acc[i] += float64(x)
		}
		n++
	}
	out := make([]float32, dim)
	if n == 0 {
		return out
	}
	var norm float64
	for i := range acc {
		acc[i] /= float64(n)
		norm += acc[i] * acc[i]
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		for i := range acc {
			out[i] = float32(acc[i])
		}
		return out
	}
	for i := range acc {
		out[i] = float32(acc[i] / norm)
	}
	return out
}

// processEmbedded：取一条 embedded 项 → GLM 清洗 → 分条 → 锚点校验（G02）→
// 每条 embed + upsert point_type=clean（1 条目 1 点）→ cleaned → 版本递增。
// 返回（是否领取到条目, 处理错误）；ErrNotFound 返回 (false, nil)。
//
// 只对「变更文档」清洗（已核实）：清洗侧不主动重扫全库、不重复清洗未变文档——
// worker 每轮只 ClaimEmbedded 一条队列项（见 ClaimEmbedded），队列里的条目只由
// 「新增/变更文档」写入产生。改判在 internal/wikisync：state 表
// wiki_sync_state.obj_edit_time 变大才入队新版本（syncer.go 的 sync 循环：
// !known → 入队；obj_edit_time > prev → 入队；否则 default 跳过「零成本、零外部
// 调用」）。手动 push（POST /docs）与迁移工具（cmd/agmem-migrate）是仅有的其它
// 入队来源，二者也都是显式触发，不存在「重扫全库重清洗」路径。
func (w *Worker) processEmbedded(ctx context.Context) (bool, error) {
	item, err := w.Store.ClaimEmbedded(ctx)
	if err != nil {
		return false, nil
	}
	source := w.resolveSourceName(ctx, item)
	run := w.newCleanRun(ctx, item)
	cleanText, err := w.cleanOne(ctx, item, item.OriginalText, source, run, nil)
	if err != nil {
		return true, w.failRetryable(ctx, item, "glm clean", err)
	}
	// 分条（2026-09-07 分条批）：GLM 输出按 markdown 结构拆成 ≤MaxEntryLen 的
	// 独立记忆条目，一条目一个向量点——单条命中长度从整段清洗文（p50≈1960 字）
	// 降到 ≤600 字。SplitEntries 内容无损（合成行见 G01/G14）。
	// 来源行注入（G01）：每条目强制带「来源：<入库文档名>」。
	entries := cleanEntriesWithSource(cleanText, source)
	if len(entries) == 0 {
		// 占位条目过滤（2026-09-07 线上实证：GLM 偶尔输出字面 `""`、`（空）` 等
		// 占位元素，照常 embed 会产生污染检索的无意义向量点）。与「GLM 返回空=
		// 全文重复」同语义：全部条目为占位符（含空响应）→ 标记 cleaned 且不落
		// clean 点，original 照旧，不算失败。original 侧不过滤（用户原文不判占位）。
		log.Printf("cleaner: item %d cleaned to placeholder/micro entries only (dedup)", item.ID)
		if err := w.Store.CompleteCleaned(ctx, item.ID, "", ""); err != nil {
			return true, w.failRetryable(ctx, item, "complete", err)
		}
		w.clearRetry(item.ID)
		return true, w.refreshTask(ctx, item.TaskID)
	}
	// 锚点校验（G02）：原文事实锚点必须全部出现在条目文本并集；缺失则严格重清
	// 一次，仍缺 → failed（可重试）并记录缺失清单。
	if missing := VerifyAnchors(item.OriginalText, entries); len(missing) > 0 {
		w.degrade(DegradeEvent{Kind: "anchor_missing", ItemID: item.ID, Stage: "anchor verify",
			Reason: strings.Join(missing, ",")})
		entries, missing = w.recleanForAnchors(ctx, item, source, run, missing)
		if len(missing) > 0 {
			return true, w.failRetryable(ctx, item, "anchor verify",
				fmt.Errorf("原文锚点缺失（严格重清后仍缺）：%s", strings.Join(missing, ", ")))
		}
	}
	// 每条目独立 embedding（一次批量调用，顺序与 entries 一致）。
	vecs, err := w.Embedder.Embed(ctx, entries)
	if err != nil {
		return true, w.failRetryable(ctx, item, "embed clean", err)
	}
	if len(vecs) != len(entries) {
		return true, w.fail(ctx, item, "embed clean",
			fmt.Errorf("embedding returned %d vectors for %d entries", len(vecs), len(entries)))
	}
	collection, err := w.collection(ctx, item.KBID)
	if err != nil {
		return true, w.fail(ctx, item, "resolve collection", err)
	}
	points := make([]qdrant.Point, 0, len(entries))
	for i, e := range entries {
		pt := qdrant.Point{
			ID:     newEntryPointID(item.ID, "clean", i),
			Vector: vecs[i],
			Payload: qdrant.PointPayload{
				Dir:        w.cleanDirName(item.DirID),
				PointType:  "clean",
				TaskID:     item.TaskID,
				KB:         collection, // 来源 kb 标注（--global 跨库检索用）
				ItemID:     item.ID,
				Text:       e,
				SourceDoc:  source, // 入库文档名（G01 结构化核实字段）
				EntryIndex: i,
				EntryCount: len(entries),
			},
		}
		if w.SparseEnabled {
			idx, vals := bm25.SparseVector(e)
			pt.Sparse = map[string]qdrant.SparseVector{
				qdrant.SparseVectorName: {Indices: idx, Values: vals},
			}
		}
		points = append(points, pt)
	}
	if err := w.Qdrant.Upsert(ctx, collection, points); err != nil {
		return true, w.failRetryable(ctx, item, "upsert clean", err)
	}
	// clean_text 存条目拼接全文；clean_point_id 存首条目点 id（回查锚点，分条批语义）。
	if err := w.Store.CompleteCleaned(ctx, item.ID, strings.Join(entries, "\n"), points[0].ID); err != nil {
		return true, w.failRetryable(ctx, item, "complete", err)
	}
	if _, err := w.Store.BumpVersion(ctx, item.KBID); err != nil {
		return true, fmt.Errorf("cleaner: bump version: %w", err)
	}
	w.clearRetry(item.ID)
	return true, w.refreshTask(ctx, item.TaskID)
}

// cleanRun 一次清洗的前缀上下文（在 processEmbedded 内固定，供锚点重清复用，
// 保证同一 item 的初次清洗与严格重清命中同一前缀）。
type cleanRun struct {
	usePrefix bool
	prefix    Prefix
	targetDoc string
}

// newCleanRun 计算本次清洗是否带前缀：需 PrefixSource 已注入且 LLM 实现
// PrefixCleaner。任一不满足则回退既有路径（行为与改动前一致）并记一次「前缀未启用」
// 日志（不静默降级）。
func (w *Worker) newCleanRun(ctx context.Context, item *store.TaskItem) cleanRun {
	if w.PrefixSource == nil {
		w.logPrefixOffOnce("PrefixSource 未注入")
		return cleanRun{}
	}
	if _, ok := w.LLM.(PrefixCleaner); !ok {
		w.logPrefixOffOnce("LLM 未实现 PrefixCleaner")
		return cleanRun{}
	}
	prefix, ok := w.prefixFor(ctx, item.KBID, w.cleanDirName(item.DirID))
	if !ok {
		return cleanRun{}
	}
	return cleanRun{usePrefix: true, prefix: prefix, targetDoc: w.targetDoc(ctx, item)}
}

// logPrefixOffOnce 前缀未启用时记一次日志（不静默降级；避免每条目刷屏）。
func (w *Worker) logPrefixOffOnce(reason string) {
	w.prefixMu.Lock()
	already := w.prefixLogged
	w.prefixLogged = true
	w.prefixMu.Unlock()
	if !already {
		log.Printf("cleaner: 清洗前缀未启用（%s）：走无前缀清洗路径", reason)
	}
}

// prefixFor 返回当前轮（目标 kb + 目标目录）的不变前缀。
//
// 轮次模型（CLAUDE.md §2、§3.1 必要条件 2）：**轮 = (kb, 目标目录)**。同一目录内复用
// 同一个冻结 Prefix（构建时取一次条目快照，本轮内不再重读）；目录或 kb 切换才重建。
// 冻结由本函数承担——PrefixBuilder.Build 只按传入的 entries 拼一次。
//
// 取条目失败时返回 ok=false，调用方回退非前缀清洗——前缀是增强，不得阻断清洗。
func (w *Worker) prefixFor(ctx context.Context, kbID int64, targetDir string) (Prefix, bool) {
	w.prefixMu.Lock()
	if s := w.prefixSess; s != nil && s.kbID == kbID && s.dir == targetDir {
		p := s.prefix
		w.prefixMu.Unlock()
		return p, true
	}
	w.prefixMu.Unlock()

	// 冻结：本轮第一次遇到该 (kb, 目录) 时取一次条目快照。
	entries, err := w.PrefixSource.KnowledgeEntries(ctx, kbID)
	if err != nil {
		log.Printf("cleaner: prefix entries kb=%d dir=%s: %v", kbID, targetDir, err)
		return Prefix{}, false
	}
	b := w.PrefixBuilder
	if b == nil {
		b = &PrefixBuilder{}
	}
	p := b.Build(entries, targetDir)

	w.prefixMu.Lock()
	w.prefixSess = &prefixSession{kbID: kbID, dir: targetDir, prefix: p}
	w.prefixMu.Unlock()
	return p, true
}

// targetDoc 解析 L3 【本次提取目标文档】的标识：TargetDocResolver 优先，回退文档名。
func (w *Worker) targetDoc(ctx context.Context, item *store.TaskItem) string {
	if w.TargetDocResolver != nil {
		if u := strings.TrimSpace(w.TargetDocResolver(ctx, item)); u != "" {
			return u
		}
	}
	return w.resolveSourceName(ctx, item)
}

// cleanOne 执行一次清洗（含可选锚点约束）：带前缀时走 PrefixCleaner 并观测 usage；
// 否则回退既有 AnchorCleaner/SourceCleaner/LLM 路径。
func (w *Worker) cleanOne(ctx context.Context, item *store.TaskItem, text, source string, run cleanRun, anchors []string) (string, error) {
	if run.usePrefix {
		pc, ok := w.LLM.(PrefixCleaner)
		if !ok { // 理论上 newCleanRun 已保证；兜底回退而非 panic。
			return w.clean(ctx, text, source)
		}
		out, usage, err := pc.CleanWithPrefix(ctx, run.prefix, run.targetDoc, source, text, anchors)
		if err != nil {
			return "", err
		}
		w.observeClean(CleanStats{
			PrefixHash:      run.prefix.Hash,
			PrefixTokens:    run.prefix.EstTokens,
			L1Tokens:        run.prefix.L1Tokens,
			L2Tokens:        run.prefix.L2Tokens,
			DirDepth:        run.prefix.DirDepth,
			PromptTokens:    usage.PromptTokens,
			CacheHitTokens:  usage.PromptCacheHitTokens,
			CacheMissTokens: usage.PromptCacheMissTokens,
			ReasoningTokens: usage.ReasoningTokens,
		})
		return out, nil
	}
	if len(anchors) > 0 {
		if ac, ok := w.LLM.(AnchorCleaner); ok {
			return ac.CleanWithAnchors(ctx, text, source, anchors)
		}
	}
	return w.clean(ctx, text, source)
}

// observeClean 记录一次带前缀清洗（S3）：结构化日志 + 可选钩子（接 /metrics）。
func (w *Worker) observeClean(st CleanStats) {
	log.Printf("clean: prefix_hash=%s prefix_tokens=%d l1=%d l2=%d depth=%d prompt=%d cache_hit=%d cache_miss=%d reasoning=%d",
		st.PrefixHash, st.PrefixTokens, st.L1Tokens, st.L2Tokens, st.DirDepth, st.PromptTokens, st.CacheHitTokens, st.CacheMissTokens, st.ReasoningTokens)
	if w.OnCleanStats != nil {
		w.OnCleanStats(st)
	}
}

// recleanForAnchors 用严格 prompt 重清一次并复验锚点，返回（条目, 仍缺失锚点）。
// 带前缀时复用同一前缀（run），否则走既有 AnchorCleaner；LLM 不支持时原样返回。
func (w *Worker) recleanForAnchors(ctx context.Context, item *store.TaskItem, source string, run cleanRun, missing []string) ([]string, []string) {
	var (
		text string
		err  error
	)
	if run.usePrefix {
		text, err = w.cleanOne(ctx, item, item.OriginalText, source, run, missing)
	} else {
		ac, ok := w.LLM.(AnchorCleaner)
		if !ok {
			return nil, missing
		}
		text, err = ac.CleanWithAnchors(ctx, item.OriginalText, source, missing)
	}
	if err != nil {
		log.Printf("cleaner: item %d anchor reclean: %v", item.ID, err)
		return nil, missing
	}
	w.degrade(DegradeEvent{Kind: "anchor_reclean", ItemID: item.ID, Stage: "anchor verify",
		Attempt: 1, Reason: strings.Join(missing, ",")})
	entries := cleanEntriesWithSource(text, source)
	if len(entries) == 0 {
		return nil, missing
	}
	return entries, VerifyAnchors(item.OriginalText, entries)
}

// resolveSourceName 解析条目入库文档名（G01）：
// Sources 解析成功 → source_name；否则原文首行标题（# 标题）；再无 → unnamedSource。
func (w *Worker) resolveSourceName(ctx context.Context, item *store.TaskItem) string {
	if w.Sources != nil {
		if name, err := w.Sources.SourceNameForItem(ctx, item.ID); err != nil {
			log.Printf("cleaner: item %d source name: %v", item.ID, err)
		} else if n := strings.TrimSpace(name); n != "" {
			return n
		}
	}
	if t := titleFromOriginal(item.OriginalText); t != "" {
		return t
	}
	return unnamedSource
}

// titleFromOriginal 取原文首行标题（首行是 markdown 标题时返回标题文本，否则 ""）。
func titleFromOriginal(text string) string {
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if isHeadingLine(t) {
			return strings.TrimSpace(strings.TrimLeft(t, "#"))
		}
		return ""
	}
	return ""
}

// clean 调 LLM 清洗：优先注入来源文档名（SourceCleaner），否则回退旧接口。
func (w *Worker) clean(ctx context.Context, text, source string) (string, error) {
	if sc, ok := w.LLM.(SourceCleaner); ok {
		return sc.CleanWithSource(ctx, text, source)
	}
	return w.LLM.Clean(ctx, text)
}

// refreshTask 条目终态后推进任务级状态（finding 1）：全终态 → completed /
// completed_with_errors；失败路径同样推进（fail 内部调用）。仅记日志不阻断。
func (w *Worker) refreshTask(ctx context.Context, taskID int64) error {
	if _, err := w.Store.RefreshTaskStatus(ctx, taskID); err != nil {
		log.Printf("cleaner: refresh task %d: %v", taskID, err)
	}
	return nil
}

func (w *Worker) fail(ctx context.Context, item *store.TaskItem, stage string, err error) error {
	if ferr := w.Store.FailItem(ctx, item.ID, item.Status, stage+": "+err.Error()); ferr != nil {
		return fmt.Errorf("cleaner: fail item %d (%s): %v (orig err: %w)", item.ID, stage, ferr, err)
	}
	log.Printf("cleaner: item %d failed at %s: %v", item.ID, stage, err)
	w.clearRetry(item.ID)
	return w.refreshTask(ctx, item.TaskID)
}

// failRetryable 标记失败并按 item 级指数退避安排自动重排（G11）：
//   - 先 FailItem 置 failed（RequeueItem 仅接受 failed 态）；
//   - attempts < MaxAttempts → 记 nextAt = now + RetryBase<<(attempts-1)，等
//     requeueDue 到点后 RequeueItem 重排（pending → 重新走 embed/clean）；
//   - 达上限 → 终败，保持 failed（可人工 requeue）。
func (w *Worker) failRetryable(ctx context.Context, item *store.TaskItem, stage string, err error) error {
	if ferr := w.Store.FailItem(ctx, item.ID, item.Status, stage+": "+err.Error()); ferr != nil {
		return fmt.Errorf("cleaner: fail item %d (%s): %v (orig err: %w)", item.ID, stage, ferr, err)
	}
	log.Printf("cleaner: item %d failed at %s: %v", item.ID, stage, err)
	if w.scheduleRetry(item) {
		w.degrade(DegradeEvent{Kind: "retry_scheduled", ItemID: item.ID, Stage: stage,
			Attempt: w.attemptsOf(item.ID), Reason: err.Error()})
	} else {
		w.degrade(DegradeEvent{Kind: "retry_exhausted", ItemID: item.ID, Stage: stage,
			Attempt: w.attemptsOf(item.ID), Reason: err.Error()})
	}
	return w.refreshTask(ctx, item.TaskID)
}

// scheduleRetry 记录一次失败并安排退避；返回 false 表示已达上限（终败）。
func (w *Worker) scheduleRetry(item *store.TaskItem) bool {
	w.retryMu.Lock()
	defer w.retryMu.Unlock()
	if w.retries == nil {
		w.retries = map[int64]*retryState{}
	}
	st := w.retries[item.ID]
	if st == nil {
		st = &retryState{}
		w.retries[item.ID] = st
	}
	st.kbID = item.KBID
	st.itemID = item.ID
	st.taskID = item.TaskID
	st.attempts++
	if st.attempts >= w.maxAttempts() {
		delete(w.retries, item.ID)
		return false
	}
	st.nextAt = w.now().Add(w.retryBase() * time.Duration(1<<(st.attempts-1)))
	return true
}

// requeueDue 把退避到点的条目 RequeueItem（failed→pending），由 step 每轮调用。
func (w *Worker) requeueDue(ctx context.Context) {
	now := w.now()
	type task struct {
		kbID, itemID, taskID int64
		attempts             int
	}
	var due []task
	w.retryMu.Lock()
	for _, st := range w.retries {
		if st.nextAt.IsZero() || st.nextAt.After(now) {
			continue
		}
		st.nextAt = time.Time{} // 已到点：本轮重排，待再次失败才安排下一轮
		due = append(due, task{st.kbID, st.itemID, st.taskID, st.attempts})
	}
	w.retryMu.Unlock()
	for _, t := range due {
		if _, err := w.Store.RequeueItem(ctx, t.kbID, t.itemID); err != nil {
			log.Printf("cleaner: requeue item %d: %v", t.itemID, err)
			continue
		}
		w.degrade(DegradeEvent{Kind: "retry_requeue", ItemID: t.itemID, Stage: "retry", Attempt: t.attempts})
	}
}

// clearRetry 成功后清除条目退避状态。
func (w *Worker) clearRetry(itemID int64) {
	w.retryMu.Lock()
	delete(w.retries, itemID)
	w.retryMu.Unlock()
}

// attemptsOf 读取条目已失败次数（事件记录用）。
func (w *Worker) attemptsOf(itemID int64) int {
	w.retryMu.Lock()
	defer w.retryMu.Unlock()
	if st := w.retries[itemID]; st != nil {
		return st.attempts
	}
	return 0
}

func (w *Worker) maxAttempts() int {
	if w.MaxAttempts > 0 {
		return w.MaxAttempts
	}
	return 3
}

func (w *Worker) retryBase() time.Duration {
	if w.RetryBase > 0 {
		return w.RetryBase
	}
	return 30 * time.Second
}

func (w *Worker) now() time.Time {
	if w.clock != nil {
		return w.clock()
	}
	return time.Now()
}

// degrade 记录降级/重试事件（钩子优先，否则结构化日志）。
func (w *Worker) degrade(ev DegradeEvent) {
	if w.OnDegrade != nil {
		w.OnDegrade(ev)
		return
	}
	logDegrade(ev)
}

// collection kb_id → qdrant collection 名。
func (w *Worker) collection(ctx context.Context, kbID int64) (string, error) {
	if w.CollectionResolver != nil {
		return w.CollectionResolver(kbID), nil
	}
	return "", fmt.Errorf("cleaner: CollectionResolver 未配置")
}

func (w *Worker) dirName(dirID int64) string {
	if w.DirResolver != nil {
		return w.DirResolver(dirID)
	}
	return fmt.Sprintf("dir-%d", dirID)
}

// cleanDirName：清洗镜像目录命名 `<dir>-clean`（原始需求 10）。
func (w *Worker) cleanDirName(dirID int64) string {
	return w.dirName(dirID) + "-clean"
}

// wrapChars 清洗输出中常见的成对包裹字符：半角/全角引号与括号。
const wrapChars = "\"'“”‘’()（）"

// isPlaceholderEntry 判定清洗输出条目是否为占位符（宁宽勿误杀）。
//
// 线上实证（2026-09-07）：GLM 清洗偶尔输出字面 `""`、`（空）`、引号包裹空串等
// 占位条目，照常 embed+upsert 会产生无意义向量点并污染检索结果。判定规则：
//  1. trim 空白后为空；
//  2. 剥离成对包裹字符（见 wrapChars，允许嵌套/混用）后为空；
//  3. 剥离后内容 ∈ {"", "空", "无", "（空）", "(空)", "N/A", "n/a"}（大小写不敏感；
//     括号项在剥包裹后通常已归约为「空」，保留以对齐规格集合兜底）。
//
// 其余一律 false——含正常短条目（如「镜像使用方法」）与恰好以引号开头的正文。
func isPlaceholderEntry(s string) bool {
	t := s
	for {
		t = strings.TrimSpace(t)
		stripped := strings.Trim(t, wrapChars)
		if stripped == t {
			break
		}
		t = stripped
	}
	switch strings.ToLower(t) {
	case "", "空", "无", "（空）", "(空)", "n/a":
		return true
	}
	return false
}

// dropPlaceholderLines 行级占位中和：占位行（isPlaceholderEntry）替换为空行而非
// 删除——保留原文段落边界，分条（SplitEntries）不会把占位行与相邻正文粘进同一
// 段；非占位行原样。分条批把上个占位过滤批的「行级剔除」升级为本函数（行级中和）
// + filterPlaceholderEntries（条目级兜底）双层。
func dropPlaceholderLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if isPlaceholderEntry(line) {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// filterPlaceholderEntries 剔除占位条目；全部为占位（含空输入）时返回空切片，
// 由调用方走「全文重复」语义（cleaned、不落 clean 点）。
// 分条批：输入从行级改为 SplitEntries 产出的条目级——先分条后过滤，段落/标题
// 边界不被行级预过滤破坏，占位行独占条目时按条目剔除。
func filterPlaceholderEntries(entries []string) []string {
	var kept []string
	for _, e := range entries {
		if isPlaceholderEntry(e) {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// filterMicroEntries 剔除 <MinEntryLen 字（rune）的微型条目（混合检索批
// 2026-09-08，动机与取舍见 MinEntryLen 注释）。顺序保持；全部被剔除时返回
// nil，调用方走「全文重复」语义（cleaned、不落 clean 点、不 bump 版本）。
func filterMicroEntries(entries []string) []string {
	var kept []string
	for _, e := range entries {
		if runeLen(e) < MinEntryLen {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// cleanEntries 条目级清洗管线（GLM 输出 → 落点条目）单一事实源：
// 行级占位中和 → 分条（≤MaxEntryLen）→ 占位条目过滤 → 微型条目过滤。
// worker（processEmbedded）与迁移工具（CleanEntries 导出）共用，保证两处
// 落点形态一致；测试直接针对本管线断言。
func cleanEntries(cleanText string) []string {
	return filterMicroEntries(
		filterPlaceholderEntries(SplitEntries(dropPlaceholderLines(cleanText), MaxEntryLen)))
}

// cleanEntriesWithSource 在条目级清洗管线之后强制注入规范来源行（G01：来源行
// 必出，值取入库文档名）。sourceName 为空时保持旧形态（供迁移侧按需调用）。
func cleanEntriesWithSource(cleanText, sourceName string) []string {
	entries := cleanEntries(cleanText)
	if strings.TrimSpace(sourceName) == "" {
		return entries
	}
	return injectSourceLines(entries, sourceName, MaxEntryLen)
}

// CleanEntries 导出条目级清洗管线（cmd/agmem-migrate RECLEAN 模式与 worker
// 共用，保证迁移重建的条目与在线清洗形态完全一致）。
func CleanEntries(cleanText string) []string { return cleanEntries(cleanText) }

// newPointID：确定性 point id（与 item 一一对应，便于幂等 upsert/重跑）。
// Qdrant point id 只接受 u64 或 UUID 字符串，"item-<n>" 这类自由字符串会被
// 400 拒绝（2026-09-06 smoke 实证）；且 original/clean 两条必须共存（搜索按
// point_type 过滤），故用 uuid.NewSHA1 对 (kind, itemID) 派生两个不同 UUID。
// 分条批后仅 original（单点全文）继续使用本函数——存量 original 点位 id 不可变。
func newPointID(itemID int64, kind string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID,
		[]byte("agmem/"+kind+"/"+strconv.FormatInt(itemID, 10))).String()
}

// newEntryPointID：entry 粒度确定性 point id（kind + itemID + entryIdx，分条批）。
// 一条 clean 拆多个点，每条目一个稳定 id：同 item 同 idx 重跑幂等覆盖。与
// newPointID 命名空间隔离（"…/entry/<idx>" 后缀），互不碰撞。
// 已知边界：重跑拆出条数变少时，旧高位 idx 点会残留——item 状态机无重清路径，
// 存量/异常重写统一归后续迁移脚本处理（本批不引入 qdrant delete）。
func newEntryPointID(itemID int64, kind string, entryIdx int) string {
	return uuid.NewSHA1(uuid.NameSpaceOID,
		[]byte("agmem/"+kind+"/"+strconv.FormatInt(itemID, 10)+"/entry/"+strconv.Itoa(entryIdx))).String()
}

// EntryPointID 导出条目点 id 派生（迁移工具 cmd/agmem-migrate 与 worker 共用，
// 保证两处生成的确定性 UUID 完全一致；internal 使用方仍走 newEntryPointID）。
func EntryPointID(itemID int64, kind string, entryIdx int) string {
	return newEntryPointID(itemID, kind, entryIdx)
}
