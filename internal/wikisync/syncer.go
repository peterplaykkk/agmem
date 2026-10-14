// syncer.go：遍历 + diff + 入队 + 替换 + 状态快照。
//
// 一轮流程（SyncOnce）：
//  1. 递归遍历 wiki 全树（任一层失败即整轮失败，不动数据——宁可不做也不误判「文档被删」）；
//  2. 载入 state 全表，内存做 diff；
//  3. 逐 docx：新增 → 入队；变更（obj_edit_time 变大）→ 先入队新版本，成功后再删旧版本；
//     未变 → 跳过；单文档失败只记错误继续，不中断整轮；
//  4. state 有、树中没有的文档（仅当全树扫描成功）→ 删旧条目、删 state 行；
//  5. 一轮结束统一写状态快照 + 回调 OnStatus。
package wikisync

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// DocPart 一段待入库正文：Name 为该段来源名（整篇一段时为标题，多段时带「（第N/M段）」），
// Text 为段正文。切段口径见 SplitDocChunks / ChunkName。
type DocPart struct {
	Name string
	Text string
}

// KBUpdater 知识库写入/删除（由 api.Server 实现；wikisync 不反向依赖 api 包）。
type KBUpdater interface {
	// EnqueueDoc 一次性入队文档的全部段落（dir=目录名），返回 task id 与新条目 id 列表；
	// 全部段落都与库内既有条目重复时 dup=true（未新增条目，调用方应保留旧版本）。
	EnqueueDoc(ctx context.Context, dir string, parts []DocPart) (taskID int64, itemIDs []int64, dup bool, err error)
	// DeleteItem 删除条目（qdrant 点位 + MySQL 行 + 版本 +1），与
	// DELETE /api/kb/{kb}/items/{id} 同一实现。
	DeleteItem(ctx context.Context, itemID int64) error
}

// DocNode 遍历结果：一个 docx 文档及其在树中的父目录标题与树结构信息。
type DocNode struct {
	ObjToken    string
	Title       string
	Dir         string
	ObjEditTime int64
	// NodeToken 该文档在 wiki 树中的 node token（URL 组成部分）。
	NodeToken string
	// ParentNodeToken 父节点 node token（顶层节点为空间根，记空串）。
	ParentNodeToken string
	// Depth 该节点深度：根层节点 = 0，其子 = 1，依此类推。
	Depth int
	// NodeURL 该节点的 wiki 链接（见 nodeURL）。
	NodeURL string
}

// Tree 一次全树遍历的产物。
type Tree struct {
	Docs         []DocNode
	ScannedNodes int // 访问到的节点总数（含非 docx）
	Skipped      int // 非 docx 跳过数
}

// Status 同步状态快照（healthz / /api/wiki-sync/status / metrics 共用）。
type Status struct {
	Enabled  bool   `json:"enabled"`
	Running  bool   `json:"running"`
	Hours    string `json:"hours,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	SpaceID  string `json:"space_id,omitempty"`

	LastRunAt      string `json:"last_run_at,omitempty"` // RFC3339（配置时区）
	LastRunUnix    int64  `json:"last_run_unix"`
	LastDurationMS int64  `json:"last_duration_ms"`
	LastScanned    int    `json:"last_scanned_nodes"`
	// TreeNodes / MaxDepth 已记录树信息的规模（state 表 node_token 非空行数与其最大
	// depth；根 = 0），供核对父子关系与深度是否符合预期。
	TreeNodes   int `json:"tree_nodes"`
	MaxDepth    int `json:"max_depth"`
	LastAdded   int `json:"last_added"`
	LastChanged int `json:"last_changed"`
	LastRemoved int `json:"last_removed"`
	LastSkipped int `json:"last_skipped"`
	// LastUnchanged 本轮「obj_edit_time 未变、整篇跳过」的文档数——**根本没读正文**。
	// 与 LastSkipped 区分：后者是「读了正文但没入库」（空正文/判重）。
	// 这是「没有变更就不提取」的直接证据：扫描数应约等于 未变数+新增+变更+跳过+失败。
	LastUnchanged int    `json:"last_unchanged"`
	LastFailed    int    `json:"last_failed"`
	NextRunAt     string `json:"next_run_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`

	// 累计计数（进程生命周期内；metrics 计数器用）。
	Runs         int64 `json:"runs"`
	TotalAdded   int64 `json:"total_added"`
	TotalChanged int64 `json:"total_changed"`
	TotalRemoved int64 `json:"total_removed"`
	TotalFailed  int64 `json:"total_failed"`
	// TotalUnchanged 累计跳过（未变）文档数。
	TotalUnchanged int64 `json:"total_unchanged"`
}

// Syncer 周期同步器（单 goroutine 驱动）。
type Syncer struct {
	Client  *FeishuClient
	SpaceID string
	State   *StateStore
	KB      KBUpdater

	Hours     map[int]bool
	Loc       *time.Location
	HoursSpec string
	// Timeout 单轮超时（≤0 → 20 分钟）。
	Timeout time.Duration
	// Now 注入时钟（测试用）；nil → time.Now。
	Now func() time.Time
	// OnStatus 每轮结束回调（生产接 api.Metrics.ObserveWikiSync，可空）。
	OnStatus func(Status)

	runMu       sync.Mutex // TryLock 防重入
	mu          sync.Mutex
	lastSlotKey string
	status      Status
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Syncer) loc() *time.Location { return locOrUTC(s.Loc) }

// Run 阻塞运行直到 ctx 取消：每分钟 tick 一次判定，在允许小时窗口内每小时执行一次。
// 启动即判定一次（重启落在允许小时内可立即补跑；同一小时重复重启则以内存键去重，
// 未变文档会被 diff 跳过，无副作用）。
func (s *Syncer) Run(ctx context.Context) {
	s.mu.Lock()
	s.status.Enabled = true
	s.status.Hours = s.HoursSpec
	s.status.Timezone = s.loc().String()
	s.status.SpaceID = s.SpaceID
	s.mu.Unlock()

	s.Tick(ctx)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick 执行一次调度判定：在允许窗口且本 (日期,小时) 未跑过 → 跑一轮并返回 true，
// 否则返回 false。测试直接调用本方法并注入 Now 覆盖边界。
func (s *Syncer) Tick(ctx context.Context) bool {
	now := s.now()
	s.mu.Lock()
	last := s.lastSlotKey
	s.mu.Unlock()
	if !ShouldRun(now, s.loc(), s.Hours, last) {
		return false
	}
	s.mu.Lock()
	s.lastSlotKey = SlotKey(now, s.loc())
	s.mu.Unlock()
	s.SyncOnce(ctx)
	return true
}

// SyncOnce 同步执行一轮（并发重入时直接跳过）。
func (s *Syncer) SyncOnce(ctx context.Context) {
	if !s.runMu.TryLock() {
		log.Printf("wikisync: 上一轮仍在运行，跳过本轮")
		return
	}
	defer s.runMu.Unlock()

	start := s.now()
	s.mu.Lock()
	s.status.Running = true
	s.mu.Unlock()

	runCtx := ctx
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}

	st, err := s.sync(runCtx)
	now := s.now()
	st.Enabled = true
	st.Hours = s.HoursSpec
	st.Timezone = s.loc().String()
	st.SpaceID = s.SpaceID
	st.Running = false
	st.LastRunAt = start.In(s.loc()).Format(time.RFC3339)
	st.LastRunUnix = start.Unix()
	st.LastDurationMS = now.Sub(start).Milliseconds()

	s.mu.Lock()
	st.Runs = s.status.Runs + 1
	st.TotalAdded = s.status.TotalAdded + int64(st.LastAdded)
	st.TotalChanged = s.status.TotalChanged + int64(st.LastChanged)
	st.TotalRemoved = s.status.TotalRemoved + int64(st.LastRemoved)
	st.TotalFailed = s.status.TotalFailed + int64(st.LastFailed)
	st.TotalUnchanged = s.status.TotalUnchanged + int64(st.LastUnchanged)
	if err != nil {
		st.LastError = err.Error()
		s.status = st
		s.mu.Unlock()
		log.Printf("wikisync: 一轮失败（scanned=%d）: %v", st.LastScanned, err)
		if s.OnStatus != nil {
			s.OnStatus(s.Snapshot())
		}
		return
	}
	s.status = st
	s.mu.Unlock()
	log.Printf("wikisync: 一轮完成 scanned=%d added=%d changed=%d removed=%d failed=%d duration=%dms",
		st.LastScanned, st.LastAdded, st.LastChanged, st.LastRemoved, st.LastFailed, st.LastDurationMS)
	if s.OnStatus != nil {
		s.OnStatus(s.Snapshot())
	}
}

// Snapshot 返回当前状态快照（含动态计算的下次计划时间）。并发安全。
func (s *Syncer) Snapshot() Status {
	s.mu.Lock()
	st := s.status
	hours := s.Hours
	s.mu.Unlock()
	st.Enabled = true
	st.Hours = s.HoursSpec
	st.Timezone = s.loc().String()
	st.SpaceID = s.SpaceID
	next := NextRun(s.now(), s.loc(), hours)
	if !next.IsZero() {
		st.NextRunAt = next.In(s.loc()).Format(time.RFC3339)
	}
	return st
}

// sync 一轮实际动作。返回的状态含本轮计数；err 非空表示整轮级失败（部分计数有效）。
func (s *Syncer) sync(ctx context.Context) (Status, error) {
	var res Status
	if s.Client == nil {
		return res, fmt.Errorf("飞书客户端未配置")
	}
	if s.State == nil {
		return res, fmt.Errorf("state store 未配置")
	}
	if s.KB == nil {
		return res, fmt.Errorf("知识库写入未配置")
	}

	// 1) 遍历全树：任何一层失败即整轮失败，不动数据。
	tree, err := s.walk(ctx)
	res.LastScanned = tree.ScannedNodes
	if err != nil {
		return res, fmt.Errorf("遍历 wiki 树失败（本轮不改动数据）: %w", err)
	}

	// 2) 载入 state 全表。
	state, err := s.State.LoadAll(ctx)
	if err != nil {
		return res, err
	}

	// 3) 逐文档 diff。单文档失败不中断整轮。
	seen := make(map[string]bool, len(tree.Docs))
	for _, d := range tree.Docs {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("同步中断: %w", err)
		}
		seen[d.ObjToken] = true
		prev, known := state[d.ObjToken]
		switch {
		case !known:
			ingested, err := s.ingest(ctx, d, nil, &res)
			if err != nil {
				res.LastFailed++
				log.Printf("wikisync: 新增文档 %q(%s) 失败: %v", d.Title, d.ObjToken, err)
				continue
			}
			if ingested {
				res.LastAdded++
			}
		case d.ObjEditTime > prev.ObjEditTime:
			ingested, err := s.ingest(ctx, d, &prev, &res)
			if err != nil {
				res.LastFailed++
				log.Printf("wikisync: 变更文档 %q(%s) 失败: %v", d.Title, d.ObjToken, err)
				continue
			}
			if ingested {
				res.LastChanged++
			}
		default:
			// 未变：跳过（本轮零成本、零外部调用——不读正文、不入队、不删旧）。
			res.LastUnchanged++
			// 例外：树结构列回填（2026-09-10 扩列的迁移期需要）——旧行缺 node_token 时
			// 只补写树列，不触变更判定字段，也不计入 added/changed。回填后逐轮比对，
			// 无漂移即无写（不是每轮都写）。
			if d.NodeToken != "" && (prev.NodeToken != d.NodeToken ||
				prev.ParentNodeToken != d.ParentNodeToken || prev.Depth != d.Depth || prev.NodeURL != d.NodeURL) {
				if err := s.State.UpdateTree(ctx, d.ObjToken, d.NodeToken, d.ParentNodeToken, d.Depth, d.NodeURL); err != nil {
					res.LastFailed++
					log.Printf("wikisync: 回填树字段 %q(%s) 失败: %v", d.Title, d.ObjToken, err)
				}
			}
		}
	}

	// 4) 移除：state 有、全树无（仅当全树扫描成功——否则会误删）。
	removed := make([]State, 0)
	for token, prev := range state {
		if !seen[token] {
			removed = append(removed, prev)
		}
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i].DocToken < removed[j].DocToken })
	for _, prev := range removed {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("同步中断: %w", err)
		}
		if err := s.remove(ctx, prev); err != nil {
			res.LastFailed++
			log.Printf("wikisync: 移除文档 %q(%s) 失败: %v", prev.Title, prev.DocToken, err)
			continue
		}
		res.LastRemoved++
	}

	// 5) 树规模快照（可观测，供 status 的 tree_nodes/max_depth 核对父子关系与深度）。
	// 取数失败只记日志、不判整轮失败——它是观测项，不得阻断同步。
	if n, d, err := s.State.TreeStats(ctx); err != nil {
		log.Printf("wikisync: tree stats: %v", err)
	} else {
		res.TreeNodes = n
		res.MaxDepth = d
	}
	return res, nil
}

// ingest 处理单个新增/变更文档：
//
//	读正文 → 切段 → 一次入队全部段落 →（成功后）删旧版本条目 → 写 state。
//
// 切段：正文按 DocChunkRunes（4000 rune）切段后**在同一次 EnqueueDoc 调用内**入队
// 全部段落（不一段一个请求），避免整篇长文在清洗/embed 阶段撞 aigateway 504。
//
// 顺序取舍（用户已确认，务必保留）：先入队、再删旧。反序一旦新版本入队失败就两头空
// （旧版本已删、新版本没有）。代价：删旧失败会残留旧条目并进入 failed 计数——本轮
// 不重试、不阻断写入新 state（state 记录新版本 item_ids），旧条目需靠下次变更或人工
// 清理。此处选择「新版本可见性」优先于「旧版本零残留」。
// ingested=true 表示确实入队了新版本（应计入 added/changed）；false 表示跳过
// （空正文或与库内既有条目重复，已在函数内计入 LastSkipped）。
// 返回 error 仅在「读正文失败 / 入队失败 / 写 state 失败」——此时不动旧版本、不改 state。
func (s *Syncer) ingest(ctx context.Context, d DocNode, prev *State, res *Status) (bool, error) {
	text, err := s.Client.RawContent(ctx, d.ObjToken)
	if err != nil {
		return false, fmt.Errorf("读取正文失败: %w", err)
	}
	if strings.TrimSpace(text) == "" {
		log.Printf("wikisync: 文档 %q(%s) 正文为空，跳过（不覆盖既有条目）", d.Title, d.ObjToken)
		res.LastSkipped++
		return false, nil
	}
	chunks := SplitDocChunks(text, DocChunkRunes)
	parts := make([]DocPart, len(chunks))
	for i, c := range chunks {
		parts[i] = DocPart{Name: ChunkName(d.Title, i, len(chunks)), Text: c}
	}
	// dup 语义：仅当**全部段落都重复**（store 返回 ErrAllDuplicates）时 dup=true——
	// 此时库内既有条目已覆盖整篇正文，保留旧版本、只推进 edit_time。
	//
	// 部分段落重复（多段文档只改了其中一部分）**不算 dup**：store 的 CreateTaskWithItems
	// 按 original_text 逐条去重，重复段跳过、非重复段照常建条目，只有「一条都没建」才回
	// ErrAllDuplicates。故此处按 store 既有语义走「非 dup」分支——新版本可见性优先。
	// 已知代价（沿用既有 store 语义，本次不改 store）：此时被跳过的重复段不会产生新条目，
	// 而后续 delete-old 会删掉旧版本全部条目，导致「改一段、其它段恰好与旧版本同文」的
	// 文档丢失那些未变段落的条目；彻底消除需 store 侧返回被跳过的文本（遗留项）。
	taskID, itemIDs, dup, err := s.KB.EnqueueDoc(ctx, d.Dir, parts)
	if err != nil {
		return false, fmt.Errorf("入队失败: %w", err)
	}
	if dup {
		// 正文与库内既存条目完全重复：未新增条目，保留旧版本、只推进 edit_time，
		// 避免每小时重复拉取同一正文。state 沿用旧 item_ids。
		var keep []int64
		var keepTask int64
		if prev != nil {
			keep = prev.ItemIDs
			keepTask = prev.TaskID
		}
		log.Printf("wikisync: 文档 %q(%s) 正文与库内既有条目重复，保留旧版本", d.Title, d.ObjToken)
		res.LastSkipped++
		return false, s.saveState(ctx, d, keepTask, keep)
	}
	// —— 顺序保证：新版本已入队成功后，才删旧版本。 ——
	if prev != nil {
		for _, id := range prev.ItemIDs {
			if err := s.KB.DeleteItem(ctx, id); err != nil {
				res.LastFailed++
				log.Printf("wikisync: 删除旧条目 %d（文档 %q）失败（新版本已入库，旧条目残留待清理）: %v",
					id, d.Title, err)
				continue
			}
		}
	}
	return true, s.saveState(ctx, d, taskID, itemIDs)
}

// remove 文档从 wiki 树消失：删其全部条目 + 删 state 行。
func (s *Syncer) remove(ctx context.Context, prev State) error {
	for _, id := range prev.ItemIDs {
		if err := s.KB.DeleteItem(ctx, id); err != nil {
			return fmt.Errorf("删除条目 %d: %w", id, err)
		}
	}
	if err := s.State.Delete(ctx, prev.DocToken); err != nil {
		return err
	}
	log.Printf("wikisync: 文档 %q(%s) 已从 wiki 移除，删除 %d 条旧条目",
		prev.Title, prev.DocToken, len(prev.ItemIDs))
	return nil
}

func (s *Syncer) saveState(ctx context.Context, d DocNode, taskID int64, itemIDs []int64) error {
	return s.State.Save(ctx, State{
		DocToken:        d.ObjToken,
		Title:           d.Title,
		Dir:             d.Dir,
		ObjEditTime:     d.ObjEditTime,
		TaskID:          taskID,
		ItemIDs:         itemIDs,
		NodeToken:       d.NodeToken,
		ParentNodeToken: d.ParentNodeToken,
		Depth:           d.Depth,
		NodeURL:         d.NodeURL,
	})
}

// maxNodes / maxDepth 遍历保护：异常数据（环/极深/极宽）不得拖垮整轮。
const (
	maxNodes = 5000
	maxDepth = 10
)

// walk 递归遍历 wiki 全树（顶层 → has_child 子树）。循环保护用 visited。
func (s *Syncer) walk(ctx context.Context) (Tree, error) {
	var out Tree
	rootDir := s.spaceDir(ctx)
	visited := map[string]bool{}

	var visit func(parent, dir string, depth int) error
	visit = func(parent, dir string, depth int) error {
		if depth > maxDepth {
			return fmt.Errorf("目录层级超过 %d", maxDepth)
		}
		nodes, err := s.Client.ListNodes(ctx, s.SpaceID, parent)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if n.NodeToken != "" {
				if visited[n.NodeToken] {
					continue
				}
				visited[n.NodeToken] = true
			}
			out.ScannedNodes++
			if out.ScannedNodes > maxNodes {
				return fmt.Errorf("节点数超过上限 %d（疑似异常数据）", maxNodes)
			}
			if n.ObjType != "docx" {
				out.Skipped++
				log.Printf("wikisync: 跳过非 docx 节点 type=%s title=%q", n.ObjType, n.Title)
			} else if n.ObjToken != "" {
				out.Docs = append(out.Docs, DocNode{
					ObjToken:        n.ObjToken,
					Title:           strings.TrimSpace(n.Title),
					Dir:             dir,
					ObjEditTime:     int64(n.ObjEditTime),
					NodeToken:       n.NodeToken,
					ParentNodeToken: parent,
					Depth:           depth,
					NodeURL:         nodeURL(n.NodeToken),
				})
			}
			if n.HasChild && n.NodeToken != "" {
				childDir := dir
				if t := strings.TrimSpace(n.Title); t != "" {
					childDir = sanitizeDir(t)
				}
				if err := visit(n.NodeToken, childDir, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit("", rootDir, 0); err != nil {
		return out, err
	}
	return out, nil
}

// feishuTenantHost 飞书租户域名：wiki 节点链接形如
// https://<tenant>.feishu.cn/wiki/<node_token>。
const feishuTenantHost = "example.feishu.cn"

// nodeURL 由 node token 拼该节点的 wiki 链接；node token 为空时返回空串。
func nodeURL(nodeToken string) string {
	if nodeToken == "" {
		return ""
	}
	return "https://" + feishuTenantHost + "/wiki/" + nodeToken
}

// spaceDir 顶层节点的目录名：取空间名（best-effort 失败降级为 wiki）。
func (s *Syncer) spaceDir(ctx context.Context) string {
	name, err := s.Client.SpaceName(ctx, s.SpaceID)
	if err != nil || strings.TrimSpace(name) == "" {
		return "wiki"
	}
	return sanitizeDir(name)
}

// sanitizeDir 目录名归一：去空白、"/"→"-"（CreateDir 拒绝含 "/" 的名字）、空→未命名、
// 超长截断（directories.name VARCHAR(128)，留余量取 100 字符）。入队侧还会再兜一层。
func sanitizeDir(title string) string {
	t := strings.ReplaceAll(strings.TrimSpace(title), "/", "-")
	t = strings.TrimSpace(t)
	if t == "" {
		return "未命名目录"
	}
	if r := []rune(t); len(r) > 100 {
		t = string(r[:100])
	}
	return t
}
