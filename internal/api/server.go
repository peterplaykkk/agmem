// Package api：agmem HTTP API（Go 1.22+ net/http 路由模式）。
//
// 路由表：
//
//	GET  /api/healthz                       健康检查（含 wiki_sync 段）
//	GET  /api/wiki-sync/status              飞书 wiki 同步状态（同 healthz 的 wiki_sync 段）
//	GET  /api/me                            当前身份（sub/role，机器 token → machine）
//	GET  /api/login                         Logto 飞书登录（OIDC 授权码）
//	GET  /api/callback                      OIDC 回调 → session cookie
//	GET  /api/kb                            知识库列表（owner+reader 可见，含汇总；web 用）
//	POST /api/kb                            建知识库            {"name":...}
//	GET  /api/kb/{kb}/dirs                  1 级目录列表
//	POST /api/kb/{kb}/dir                   建 1 级目录          {"name":...}
//	POST /api/kb/{kb}/docs                  上传文档/目录（多文件文本）→ task_id
//	GET  /api/kb/{kb}/tasks/{task_id}       任务状态与条目进度
//	GET  /api/kb/{kb}/points?status=cleaning&page=1&page_size=20  分页列条目
//	DELETE /api/kb/{kb}/items/{item_id}     删除条目（点位+元数据，版本 +1；404 幂等语义）
//	POST /api/kb/{kb}/items/{item_id}/retry 失败条目重排（仅 failed→pending；非 failed 409）
//	GET  /api/kb/{kb}/readers               reader 列表（owner-only）
//	POST /api/kb/{kb}/readers              新增 reader {"sub":...}（owner-only，幂等）
//	DELETE /api/kb/{kb}/readers/{sub}      删除 reader（owner-only，幂等）
//	POST /api/ask                           查询 {"query","dir","top_k"}（kb/global 已废弃忽略）
//	GET  /api/version/{kb}                  kb 数据版本号
//	GET  /api/version/global                单库版兼容端点：返回 cfg.KB 的版本（同 /api/version/{kb}）
//
// 单库化（2026-09-08 拍板）：产品只有一个知识库（cfg.KB，默认 sre，不迁移存量
// 数据）。ask 的 kb 请求字段忽略（旧 CLI wire 兼容）；写/删除端点（docs 上传、
// 建目录、items/readers 增删）的 URL {kb} 必须等于 cfg.KB，否则 404（G36 跨库
// 写防护）——只读端点（dirs/points/tasks/version/readers）按路径 {kb} 解析，
// 允许读第二库。ListKBs 权限列表保留（web 还用）。
//
// 权限：机器 token（agmem）全量；只读客户端令牌（AGMEM_CLIENT_READONLY_TOKEN）
// 仅 /api/ask 与 /api/version/*（其余按 404 掩蔽）；OIDC 登录按 kb owner_sub /
// kb_readers 过滤（404 掩蔽无权 kb）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"sre/agmem/internal/auth"
	"sre/agmem/internal/bm25"
	"sre/agmem/internal/cache"
	"sre/agmem/internal/cleaner"
	"sre/agmem/internal/config"
	"sre/agmem/internal/embedding"
	"sre/agmem/internal/qdrant"
	"sre/agmem/internal/store"
	"sre/agmem/internal/wikisync"
)

// Principal 已认证身份。
type Principal struct {
	Sub      string // OIDC sub；机器 token 为 ""
	Machine  bool   // 机器 token（全量读写）
	Readonly bool   // 只读客户端令牌（仅 /api/ask 与 /api/version/*）
}

// readAll 是否具备全库读权限（机器 token 或只读客户端令牌）。写权限一律另判
// （写端点对只读令牌已在 auth 层按 404 掩蔽拒绝；owner/机器判定仍走 Machine）。
func (p Principal) readAll() bool { return p.Machine || p.Readonly }

type ctxKey int

const principalKey ctxKey = 0

// PrincipalOf 从 context 取身份。
func PrincipalOf(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey).(Principal)
	return p
}

// Server 聚合各组件。
type Server struct {
	Cfg      config.Config
	Store    *store.Store
	Embedder *embedding.Client
	Qdrant   *qdrant.Client
	LLM      cleaner.LLM
	Machine  *auth.MachineAuth
	// Readonly 只读客户端令牌校验器（AGMEM_CLIENT_READONLY_TOKEN；nil/空 = 未配置）。
	// 未配置时鉴权行为与既有完全一致。
	Readonly *auth.ReadonlyAuth
	Session  *auth.Session
	OIDC     *auth.OIDC
	// SingleKB 单库路径（ask / push 上传 / version-global）存储注入点：
	// 生产 nil → Store；测试注入 fake 免 MySQL（沿用原 kbLister 注入模式）。
	SingleKB singleKBStore
	// Lifecycle 生命周期管理路径（删除/重试/reader，UC-011）存储注入点：
	// 生产 nil → Store；测试注入 fake 免 MySQL。
	Lifecycle lifecycleStore
	// KBCreator 建库路径存储注入点（G20）：生产 nil → Store；测试注入 fake 免 MySQL，
	// 用于断言「EnsureCollection 失败时不落 kb 行」。
	KBCreator kbCreator
	// Metrics 指标注册表（生产 main 注入以接线 GLM HTTP 计数；nil → 首次使用时按
	// Cfg.Version 惰性创建，见 metrics.go）。
	Metrics *Metrics
	// QueueStats 清洗队列 gauge 数据源（生产 nil → Store；测试注入 fake 免 MySQL）。
	QueueStats queueStatter
	// WikiSync 飞书 wiki 增量同步状态源（生产 *wikisync.Syncer；nil → healthz/
	// status 返回 {"enabled":false}）。同步写入/删除经本 Server 的 EnqueueDoc/
	// DeleteItem（与 HTTP 端点同一实现）。
	WikiSync wikiSyncProvider
	// Cache 进程内本地查询缓存（*cache.Store；2026-09-11 接入 /api/ask）。
	// 生产由 main 装配：落 Pod 的 PVC（AGMEMD_DIR），只读根文件系统下目录不可写
	// 时 fail-open 为 nil——缓存禁用但服务与 /api/ask 照常（不查、不写、不报错）。
	// 测试注入 t.TempDir() 实例。nil 时 /api/ask 完全跳过缓存。
	Cache *cache.Store
	// CacheMaxBytes 缓存 LRU 上限（<=0 → cache.DefaultMaxBytes，500MB）。
	CacheMaxBytes int64
	// OnEnqueue 可选钩子：清洗条目成功入队后调用（生产注入 cleaner.Worker.Poke，
	// 让空轮询退避中的 worker 立即恢复满速；见 CI MySQL 审计 F5，2026-09-30）。
	// 必须非阻塞；nil 时无操作。
	OnEnqueue func()
}

// wikiSyncProvider 状态快照来源（*wikisync.Syncer 实现；测试注入 fake）。
type wikiSyncProvider interface {
	Snapshot() wikisync.Status
}

// kbCreator 建库最小接口（*store.Store 满足）。
type kbCreator interface {
	CreateKB(ctx context.Context, name, ownerSub string) (int64, error)
}

// kbCreatorStore 建库路径存储来源（测试注入优先，生产回落 Store）。
func (s *Server) kbCreatorStore() kbCreator {
	if s.KBCreator != nil {
		return s.KBCreator
	}
	return s.Store
}

// kbResolver 单库路径解析 kb + 读权限的最小接口（singleKBStore/lifecycleStore 均满足）。
type kbResolver interface {
	GetKB(ctx context.Context, name string) (*store.KB, error)
	HasReadAccess(ctx context.Context, kbID int64, sub string, machine bool) (bool, error)
}

// singleKBStore 单库路径的存储最小接口（*store.Store 隐式满足）。
// 单库化（2026-09-08）后原跨 kb fan-out 的 kbLister 注入点收敛于此。
type singleKBStore interface {
	kbResolver
	GetVersion(ctx context.Context, kbID int64) (uint64, error)
	GetKBDirByName(ctx context.Context, kbID int64, name string) (*store.Dir, error)
	CreateTaskWithItems(ctx context.Context, kbID, dirID int64, texts []string, sourceNames []string) (int64, int, error)
}

// lifecycleStore 生命周期管理端点（UC-011）所需存储最小接口（*store.Store 满足）。
type lifecycleStore interface {
	kbResolver
	GetVersion(ctx context.Context, kbID int64) (uint64, error)
	BumpVersion(ctx context.Context, kbID int64) (uint64, error)
	GetItem(ctx context.Context, kbID, itemID int64) (*store.TaskItem, error)
	DeleteItem(ctx context.Context, kbID, itemID int64) error
	RequeueItem(ctx context.Context, kbID, itemID int64) (store.TaskStatus, error)
	RefreshTaskStatus(ctx context.Context, taskID int64) (store.TaskStatus, error)
	AddReader(ctx context.Context, kbID int64, sub string) (bool, error)
	RemoveReader(ctx context.Context, kbID int64, sub string) (bool, error)
	ListReaders(ctx context.Context, kbID int64) ([]string, error)
}

// kbStore 单库路径存储来源（测试注入优先，生产回落 Store）。
func (s *Server) kbStore() singleKBStore {
	if s.SingleKB != nil {
		return s.SingleKB
	}
	return s.Store
}

// lifecycle 生命周期管理路径存储来源（测试注入优先，生产回落 Store）。
func (s *Server) lifecycle() lifecycleStore {
	if s.Lifecycle != nil {
		return s.Lifecycle
	}
	return s.Store
}

// getSingleKB 解析全局唯一 kb（cfg.KB）并校验读权限（无权限按 404 掩蔽，
// 避免枚举）。ask / push / version-global 三路共用。
func (s *Server) getSingleKB(ctx context.Context, p Principal) (*store.KB, error) {
	return s.resolveSingleKB(ctx, s.kbStore(), p)
}

// resolveSingleKB 解析 kb + 读权限的单一事实源（singleKBStore / lifecycleStore 共用）。
func (s *Server) resolveSingleKB(ctx context.Context, st kbResolver, p Principal) (*store.KB, error) {
	kb, err := st.GetKB(ctx, s.Cfg.KB)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errNotFoundMasked
		}
		return nil, err
	}
	ok, err := st.HasReadAccess(ctx, kb.ID, p.Sub, p.readAll())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errNotFoundMasked // 掩蔽
	}
	return kb, nil
}

// errNotFoundMasked 内部哨兵：kb 不存在或无权限统一对外 404（掩蔽语义）。
var errNotFoundMasked = errors.New("api: kb 不存在或无权限")

// bm25Sparse 查询文本 → qdrant 稀疏向量装配（bm25 编码器与 qdrant wire 结构解耦）。
func bm25Sparse(query string) qdrant.SparseVector {
	idx, vals := bm25.SparseVector(query)
	return qdrant.SparseVector{Indices: idx, Values: vals}
}

// Routes 注册全部路由。每条路由经 instrument 打点（route 标签 = 注册模板，
// 不含原始路径/用户输入）；/metrics 不在本表（webui 只透传 /api/*，见 RootHandler）。
//
// 鉴权接线：route 注册「需鉴权」路由并把注册模板一并交给 auth（只读客户端令牌
// 按模板白名单放行，见 auth/readonlyRouteAllow）；public 注册无需鉴权的路由
// （healthz 与 OIDC 登录/回调）。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, s.instrument(pattern, s.auth(pattern, handler)))
	}
	public := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, s.instrument(pattern, handler))
	}
	public("GET /api/healthz", s.handleHealthz)
	// wiki 同步状态（与 healthz 的 wiki_sync 段同内容；需鉴权，便于单独查看）
	route("GET /api/wiki-sync/status", s.handleWikiSyncStatus)
	route("GET /api/me", s.handleMe)
	public("GET /api/login", s.handleLogin)
	public("GET /api/callback", s.handleCallback)

	route("GET /api/kb", s.handleListKB)
	route("POST /api/kb", s.handleCreateKB)
	route("GET /api/kb/{kb}/dirs", s.handleListDirs)
	route("POST /api/kb/{kb}/dir", s.handleCreateDir)
	route("POST /api/kb/{kb}/docs", s.handleUploadDocs)
	route("GET /api/kb/{kb}/tasks/{task_id}", s.handleTaskStatus)
	route("GET /api/kb/{kb}/points", s.handlePoints)
	route("DELETE /api/kb/{kb}/items/{item_id}", s.handleDeleteItem)
	route("POST /api/kb/{kb}/items/{item_id}/retry", s.handleRetryItem)
	route("GET /api/kb/{kb}/readers", s.handleListReaders)
	route("POST /api/kb/{kb}/readers", s.handleAddReader)
	route("DELETE /api/kb/{kb}/readers/{sub}", s.handleRemoveReader)
	route("POST /api/ask", s.handleAsk)
	route("GET /api/version/{kb}", s.handleVersion)
	// 字面路径优先于 {kb} 通配（Go 1.22 ServeMux 规则）；"global" 由此成为保留 kb 名，
	// handleCreateKB 拒绝该名防版本端点被遮蔽。单库化后本端点返回 cfg.KB 版本
	// （兼容旧 agmemd 的全局查询路径；旧指纹语义已退役）。
	route("GET /api/version/global", s.handleVersionGlobal)

	return s.securityHeaders(mux)
}

// securityHeaders 全局安全响应头（CSP 等；不新增绕过路径）。
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; object-src 'none'")
		next.ServeHTTP(w, r)
	})
}

// readonlyRouteAllow 只读客户端令牌可访问的路由模板白名单（键为 Routes 里注册的
// 模板字符串）。刻意只放行「检索 + 缓存失效锚点 + 身份自省」三类只读操作：
//
//	GET  /api/me              → 身份自省（返回 role=client-readonly，供客户端自检）
//	POST /api/ask             → 检索（客户端执行任务前先查流程）
//	GET  /api/version/{kb}    → 缓存失效锚点
//	GET  /api/version/global  → 缓存失效锚点（兼容端点）
//
// 其余端点（含全部写/删端点与列表/详情/reader 端点）一律拒绝。
var readonlyRouteAllow = map[string]bool{
	"GET /api/me":             true,
	"POST /api/ask":           true,
	"GET /api/version/{kb}":   true,
	"GET /api/version/global": true,
}

// auth：机器 token / 只读客户端令牌 / 登录 session 任一通过即可，注入 Principal。
//
// pattern 是本路由在 Routes 里注册的模板，只读令牌据此做端点白名单判定。
// 只读令牌被拒时返回 404（而非 403）：与既有「非读者 kb 访问按 404 掩蔽」
// 同一惯例（kbAccess/server.go 的掩蔽语义）——只读令牌本就是分发给终端用户的
// 低权凭证，一旦泄露，404 使攻击者无法区分「端点存在但无权」与「端点不存在」，
// 不泄露 api 面清单；403 则会确认端点存在，等于给出枚举信号。
func (s *Server) auth(pattern string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Machine.Check(r) {
			next(w, r.WithContext(context.WithValue(r.Context(), principalKey, Principal{Machine: true})))
			return
		}
		if s.Readonly.Check(r) {
			if !readonlyRouteAllow[pattern] {
				writeErr(w, http.StatusNotFound, "not found") // 掩蔽：不泄露端点存在
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), principalKey, Principal{Readonly: true})))
			return
		}
		if sid, err := r.Cookie("agmem_session"); err == nil {
			if sub := s.Session.Get(sid.Value); sub != "" {
				next(w, r.WithContext(context.WithValue(r.Context(), principalKey, Principal{Sub: sub})))
				return
			}
		}
		writeErr(w, http.StatusUnauthorized, "unauthorized: 需要 Bearer token 或登录 session")
	})
}

// —— 身份/权限 ——

// kbAccess 解析路径 kb 并校验读权限（无权限按 404 掩蔽，避免枚举）。
func (s *Server) kbAccess(w http.ResponseWriter, r *http.Request) (*store.KB, bool) {
	name := r.PathValue("kb")
	kb, err := s.Store.GetKB(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "kb 不存在: "+name)
			return nil, false
		}
		internalErr(w, err)
		return nil, false
	}
	p := PrincipalOf(r.Context())
	ok, err := s.Store.HasReadAccess(r.Context(), kb.ID, p.Sub, p.readAll())
	if err != nil {
		internalErr(w, err)
		return nil, false
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "kb 不存在: "+name) // 掩蔽
		return nil, false
	}
	return kb, true
}

// requireWritableKB 单库写守卫（G36）：路径 {kb} 必须等于 cfg.KB，否则 404 明确
// 拒绝——单库模式下写/删除操作只作用于唯一库，绝不静默改到别的 kb（线上存在第二
// 个 kb `otherkb`，历史行为是路径段被忽略、实际作用在 cfg.KB 上，构成跨库误删）。
// 只读端点（dirs/points/tasks/version/readers）仍按路径 {kb} 解析，允许读第二库。
func (s *Server) requireWritableKB(w http.ResponseWriter, r *http.Request) bool {
	name := r.PathValue("kb")
	if name != s.Cfg.KB {
		writeErr(w, http.StatusNotFound,
			"kb 不可写: "+name+"（单库模式写操作仅支持 "+s.Cfg.KB+"；只读查询见 /api/kb/"+name+"/points）")
		return false
	}
	return true
}

// —— handlers ——

// handleHealthz GET /api/healthz：既有 {status,version} 语义不变，additive 追加
// wiki_sync 段（enabled/上次运行/计数/下次计划/最近错误；未启用时 enabled=false）。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"version":   s.Cfg.Version,
		"wiki_sync": s.wikiSyncStatus(),
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := PrincipalOf(r.Context())
	out := map[string]any{"role": "reader"}
	switch {
	case p.Machine:
		out["role"] = "machine"
		out["sub"] = "machine"
	case p.Readonly:
		// 只读客户端令牌：role 与 machine 区分，便于客户端自检与审计。
		out["role"] = "client-readonly"
		out["sub"] = "client-readonly"
	default:
		out["sub"] = p.Sub
		// 列出可访问 kb 数（Store 缺省时跳过——纯鉴权冒烟场景）
		if s.Store != nil {
			kbs, err := s.Store.ListKBs(r.Context(), p.Sub)
			if err == nil {
				out["kb_count"] = len(kbs)
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListKB GET /api/kb：owner+reader 可见列表（含 dir 数/条目数/版本/最近任务摘要）。
func (s *Server) handleListKB(w http.ResponseWriter, r *http.Request) {
	p := PrincipalOf(r.Context())
	sub := ""
	if !p.Machine {
		sub = p.Sub
	}
	kbs, err := s.Store.ListKBs(r.Context(), sub)
	if err != nil {
		internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kbs": kbs})
}

func (s *Server) handleCreateKB(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name 必填")
		return
	}
	if req.Name == "global" {
		// 保留名：/api/version/global 字面路由优先于 /api/version/{kb}，
		// 允许建同名声 kb 会遮蔽其版本端点（agmemd 版本门禁将命中全局指纹 → 缓存脏读）
		writeErr(w, http.StatusBadRequest, "kb 名 global 为保留名（版本指纹端点冲突）")
		return
	}
	p := PrincipalOf(r.Context())
	owner := "machine"
	if !p.Machine {
		owner = p.Sub
	}
	// G20：先建 qdrant collection 再落 MySQL kb 行。取舍——反序（先 MySQL 后
	// qdrant）在 EnsureCollection 失败时会留下「有 kb 无 collection」的脏状态：
	// 该库 push/ask 必然失败且无自愈路径（GET /api/kb 还会列出它）。
	// 正序失败最多留下一个空 collection（GET /api/kb 不列出；重建同名库时
	// EnsureCollection 幂等复用），可自愈且不影响既有库。
	if err := s.Qdrant.EnsureCollection(r.Context(), req.Name, s.Cfg.QdrantDim); err != nil {
		writeErr(w, http.StatusBadGateway, "qdrant 建 collection 失败: "+err.Error())
		return
	}
	actx := store.WithAudit(r.Context(), store.AuditEntry{
		ActorSub: principalID(p), ActorRole: principalRole(p), Action: "create_kb",
		KB: req.Name, ObjectID: req.Name, SourceIP: clientIP(r),
	})
	id, err := s.kbCreatorStore().CreateKB(actx, req.Name, owner)
	if err != nil {
		if errors.Is(err, store.ErrExists) {
			writeErr(w, http.StatusConflict, "kb 已存在: "+req.Name)
			return
		}
		internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": req.Name})
}

func (s *Server) handleListDirs(w http.ResponseWriter, r *http.Request) {
	kb, ok := s.kbAccess(w, r)
	if !ok {
		return
	}
	dirs, err := s.Store.ListDirs(r.Context(), kb.ID)
	if err != nil {
		internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kb": kb.Name, "dirs": dirs})
}

func (s *Server) handleCreateDir(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritableKB(w, r) {
		return
	}
	kb, ok := s.kbAccess(w, r)
	if !ok {
		return
	}
	p := PrincipalOf(r.Context())
	if !p.Machine && p.Sub != kb.OwnerSub {
		// 仅 owner 可建目录（reader 只读）
		writeErr(w, http.StatusForbidden, "仅 kb owner 可建目录")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name 必填")
		return
	}
	if containsSlash(req.Name) {
		// 1 级目录硬约束（原始需求 7）
		writeErr(w, http.StatusBadRequest, "只支持 1 级目录，name 不得包含 /")
		return
	}
	actx := store.WithAudit(r.Context(), store.AuditEntry{
		ActorSub: principalID(p), ActorRole: principalRole(p), Action: "create_dir",
		KB: kb.Name, ObjectID: req.Name, SourceIP: clientIP(r),
	})
	id, err := s.Store.CreateDir(actx, kb.ID, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrExists) {
			writeErr(w, http.StatusConflict, "dir 已存在: "+req.Name)
			return
		}
		internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "kb": kb.Name, "dir": req.Name})
}

// docsReq 支持多文件文本上传（目录上传由 agmem 展开为多条 texts）。
type docsReq struct {
	Dir   string   `json:"dir"`   // 1 级目录名
	Texts []string `json:"texts"` // 每条一段原始文本
	Files []struct {
		Name string `json:"name"`
		Text string `json:"text"`
	} `json:"files"`
}

// textDefaultName 为没有文件名的 texts[] 条目派生文档名。
//
// 取首个非空行（剥掉 markdown 标题前缀、截断 60 字）；全空则回退 未命名文本[i]。
// 该名字会原样出现在 clean 条目的「来源：」行与 payload.source_doc 上，是核实
// 依据（v5 写入红线：每条可追溯到入库文档名）。旧行为是 fmt.Sprintf("texts[%d]", i)
// ——数组下标当来源，等于没有来源，按文本 push 的条目一律不可追溯。
func textDefaultName(text string, i int) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line == "" {
			continue
		}
		r := []rune(line)
		if len(r) > 60 {
			return string(r[:60]) + "…"
		}
		return line
	}
	return fmt.Sprintf("未命名文本[%d]", i)
}

// handleUploadDocs：契约 {dir, texts[], files[]} → 目录存在则建任务+拆条目（pending）→ 返回 task_id。
// push 路径单库化（2026-09-08）：URL 的 {kb} 段保留（旧 CLI wire 兼容）但忽略，
// 实际写入恒为 cfg.KB。入库去重：original_text 已存在的条目由 CreateTaskWithItems
// 跳过；全部重复时不建任务（task_id=0，status=duplicate）。
// 清洗管线由 worker 异步推进（UC-1）。
func (s *Server) handleUploadDocs(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritableKB(w, r) {
		return
	}
	kb, err := s.getSingleKB(r.Context(), PrincipalOf(r.Context()))
	if err != nil {
		if errors.Is(err, errNotFoundMasked) {
			writeErr(w, http.StatusNotFound, "kb 不存在: "+s.Cfg.KB)
			return
		}
		internalErr(w, err)
		return
	}
	// 上传保持 owner-only（原 push 权限语义不变；getSingleKB 只保证读权限）
	p := PrincipalOf(r.Context())
	if !p.Machine && p.Sub != kb.OwnerSub {
		writeErr(w, http.StatusForbidden, "仅 kb owner 可上传")
		return
	}
	var req docsReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Dir == "" {
		writeErr(w, http.StatusBadRequest, "dir 必填")
		return
	}
	var texts, names []string
	for i, t := range req.Texts {
		texts = append(texts, t)
		names = append(names, textDefaultName(t, i))
	}
	for _, f := range req.Files {
		texts = append(texts, f.Text)
		names = append(names, f.Name)
	}
	if len(texts) == 0 {
		writeErr(w, http.StatusBadRequest, "texts / files 至少其一非空")
		return
	}
	if len(texts) > 200 {
		writeErr(w, http.StatusBadRequest, "单批最多 200 条")
		return
	}
	// 目录必须已存在（防手滑建错目录；不存在返回 404 指引先 POST /dir）
	dir, err := s.kbStore().GetKBDirByName(r.Context(), kb.ID, req.Dir)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "目录不存在: "+req.Dir+"（先 POST /api/kb/"+kb.Name+"/dir）")
			return
		}
		internalErr(w, err)
		return
	}
	// 确保 qdrant collection 就绪（幂等）
	if err := s.Qdrant.EnsureCollection(r.Context(), kb.Name, s.Cfg.QdrantDim); err != nil {
		log.Printf("api: ensure collection %s: %v", kb.Name, err)
	}
	actx := store.WithAudit(r.Context(), store.AuditEntry{
		ActorSub: principalID(p), ActorRole: principalRole(p), Action: "upload_docs",
		KB: kb.Name, ObjectID: req.Dir, SourceIP: clientIP(r),
	})
	taskID, inserted, err := s.kbStore().CreateTaskWithItems(actx, kb.ID, dir.ID, texts, names)
	if err != nil {
		if errors.Is(err, store.ErrAllDuplicates) {
			writeJSON(w, http.StatusOK, map[string]any{
				"task_id": 0, "kb": kb.Name, "dir": req.Dir, "items": 0,
				"status": "duplicate",
				"hint":   "提交内容均已存在（入库去重跳过），未创建任务",
			})
			return
		}
		internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id": taskID, "kb": kb.Name, "dir": req.Dir, "items": inserted,
		"status": "pending", "hint": "用 GET /api/kb/" + kb.Name + "/tasks/" + itoa(taskID) + " 查进度",
	})
	// 有新条目入队：唤醒空轮询退避中的 worker（非阻塞；nil 钩子无操作）。
	if inserted > 0 && s.OnEnqueue != nil {
		s.OnEnqueue()
	}
}

func (s *Server) handleTaskStatus(w http.ResponseWriter, r *http.Request) {
	kb, ok := s.kbAccess(w, r)
	if !ok {
		return
	}
	taskID, ok := int64Path(w, r, "task_id")
	if !ok {
		return
	}
	task, err := s.Store.GetTask(r.Context(), kb.ID, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "task 不存在")
			return
		}
		internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// handlePoints 分页列条目（page/page_size/total；默认 status=cleaning 清洗中视图）。
func (s *Server) handlePoints(w http.ResponseWriter, r *http.Request) {
	kb, ok := s.kbAccess(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	if status == "" {
		status = string(store.StatusCleaning) // 默认：清洗中视图（原始需求 11）
	}
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	res, err := s.Store.ListItemsByStatusPage(r.Context(), kb.ID, store.TaskStatus(status), page, pageSize)
	if err != nil {
		internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kb": kb.Name, "status": status,
		"page": res.Page, "page_size": res.PageSize, "total": res.Total, "items": res.Items,
	})
}

// askReq 查询语义（单库化定稿，2026-09-08）：
//
//	检索范围恒为 cfg.KB 全局唯一库；Dir 非空时收窄到单 1 级目录。
//	KB / Global 字段保留解析（旧 CLI wire 兼容）但一律忽略（deprecated）。
type askReq struct {
	KB     string `json:"kb"` // deprecated：单库版忽略（旧 CLI 兼容）
	Dir    string `json:"dir,omitempty"`
	Query  string `json:"query"`
	TopK   int    `json:"top_k,omitempty"`
	Global bool   `json:"global,omitempty"` // deprecated：单库版忽略（旧 CLI 兼容）
}

// handleAsk POST /api/ask（单库语义）：query 必填；kb/global 字段忽略，
// 范围恒为 cfg.KB（getSingleKB 做权限掩蔽），dir 仍可收窄。
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req askReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Query == "" {
		writeErr(w, http.StatusBadRequest, "query 必填")
		return
	}
	s.askKB(w, r, req)
}

// askKB 单库检索（cfg.KB 全库，dir 收窄单目录）。
// 权限检查先于 embedding（未授权/不存在 kb 不消耗外部 embedding 配额）。
// 响应保留 kb/global/version 字段：kb=实际检索库名，global=请求 deprecated 字段原样回显。
//
// 进程内本地缓存（2026-09-11）：拿到查询向量后先查本地缓存（Store.Nearest +
// ShouldHitLocal），命中直接返回首次落库的上游响应体、不查 qdrant；未命中走既有
// 检索链路，成功后落缓存。缓存全程 fail-open——Cache 为 nil 或版本锚点不可读时
// 自动跳过，绝不改变成功/失败语义。
func (s *Server) askKB(w http.ResponseWriter, r *http.Request, req askReq) {
	kb, err := s.getSingleKB(r.Context(), PrincipalOf(r.Context()))
	if err != nil {
		if errors.Is(err, errNotFoundMasked) {
			writeErr(w, http.StatusNotFound, "kb 不存在: "+s.Cfg.KB)
			return
		}
		internalErr(w, err)
		return
	}
	// 版本锚点：与 GET /api/version/{kb} 同源（store.GetVersion），缓存命中判定与
	// 响应 version 字段共用。读取失败（verr != nil）时缓存整体跳过——不无版本命中、
	// 也不落无版本条目（避免脏读）；响应仍按既有行为写 version=0。
	version, verr := s.kbStore().GetVersion(r.Context(), kb.ID)

	// ① 精确命中（同名 (dir,question)）：只查本地缓存、不做 embedding——同问复答
	// 不付云端 embedding 往返（其量级 ~0.1–0.3s，是本路径唯一剩余的大头）。
	if hit, core := s.askCacheExact(req, version, verr); hit {
		s.writeAskJSON(w, core, true)
		return
	}

	// 查询向量：默认只搜 clean 条目（清洗后的记忆条目）。
	// 查询侧走 EmbedQuery（Qwen3-Embedding 非对称指令前缀，AGMEM_QUERY_INSTRUCT）：
	// 指令只加查询侧，写入/ingest 路径恒用 Embed 不加前缀。
	// 该向量同时用于本地缓存近邻（缓存与检索共用同一查询向量，不引入 onnxruntime）。
	// 待评审：是否提供 include_original 开关（接口已留 PointType 字段）。
	vecs, err := s.Embedder.EmbedQuery(r.Context(), []string{req.Query})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "embedding 失败: "+err.Error())
		return
	}
	qvec := vecs[0]

	// ② 向量近邻命中（同义/改写问句：复用同一查询向量 + ShouldHitLocal 判定）。
	if hit, core := s.askCacheNearest(req, qvec, version, verr); hit {
		s.writeAskJSON(w, core, true)
		return
	}

	// ③ 未命中 → 既有检索链路。
	// Vector 在这里统一注入（hybrid/off 两条分支共用）；off 分支此前漏传
	// 导致 legacy Search 收到 vector=null → qdrant 400（2026-09-08 实测）。
	// Dir 归一（G05）：clean 点 payload.dir 是 `<dir>-clean`（worker 写入），
	// 用户目录名须在查询侧加后缀，否则 README 示例 `agmem ask --dir k8s` 恒 0 命中。
	sreq := qdrant.SearchRequest{Limit: req.TopK, Dir: normalizeQueryDir(req.Dir), PointType: "clean", Vector: qvec}
	var hits []qdrant.SearchHit
	if s.Cfg.HybridEnabled {
		hits, err = s.Qdrant.SearchHybrid(r.Context(), kb.Name, qvec, bm25Sparse(req.Query), sreq)
	} else {
		hits, err = s.Qdrant.Search(r.Context(), kb.Name, sreq) // AGMEM_HYBRID=off 降级路径
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "qdrant 查询失败: "+err.Error())
		return
	}
	// 分数阈值（UC-1）：qdrant 路径统一在返回前过滤一次；filter 诊断随响应返回
	// （G06：CLI --debug 展示过滤原因，--json 可见；additive 字段，缓存兼容）。
	kept, diag := s.filterByThreshold(hits)
	// 近重复折叠（2026-09-10）：同一事实常被多篇文档分别讲过，过滤后仍可能并存
	// 多条近乎逐字相同的条目——返回前折叠，只留排序最高的一条（见 dedupe.go）。
	var dropped int
	kept, dropped = collapseNearDuplicates(kept)
	// 语义去重（2026-09-10）：字面判据抓不住同义改写，再叠一层向量余弦折叠。
	// 失败降级为不折（见 dedupe_semantic.go）；阈值 AGMEM_DEDUP_COSINE。
	var semDropped int
	kept, semDropped = s.semanticCollapse(r.Context(), kept)
	diag.Deduped = dropped + semDropped

	// 序列化一次：未命中直接写它、命中写落库的同一份字节——命中与未命中的
	// hits 内容因同源而逐字节一致（md5 相等）。
	core, merr := json.Marshal(map[string]any{
		"kb": kb.Name, "global": req.Global, "version": version, "hits": kept, "filter": diag,
	})
	if merr != nil {
		internalErr(w, merr)
		return
	}
	// ③ 落缓存（失败/版本不可读不落；落库失败仅记日志，绝不影响响应）。
	s.putAskCache(req, qvec, version, core, verr)
	s.writeAskJSON(w, core, false)
}

// cacheMaxBytes 生效的缓存 LRU 上限。
func (s *Server) cacheMaxBytes() int64 {
	if s.CacheMaxBytes > 0 {
		return s.CacheMaxBytes
	}
	return cache.DefaultMaxBytes
}

// askCacheExact 精确命中：按确定性 id = sha256(dir|question)（ids.go）直取条目——
// 同问复答不必再做 embedding（缓存路径唯一剩余的网络往返即在此省掉）。
// 命中判定与近邻路径同语义：ShouldHitLocal(1.0, 本地版本, 远端版本)（相似度恒 1
// > 阈值 且 kb_version 一致）；版本不符 → 视为未命中（交由后续路径 + Put 覆盖刷新）。
// fail-open：Cache==nil、版本锚点不可读、缓存查询出错 一律 (false,nil)。
func (s *Server) askCacheExact(req askReq, version uint64, verr error) (bool, []byte) {
	if s.Cache == nil || verr != nil {
		return false, nil
	}
	e, err := s.Cache.Get(cache.QueryID(req.Dir, req.Query))
	if err != nil {
		return false, nil // ErrCacheNotFound（常态）或读库错误，均按未命中 fail-open
	}
	if !cache.ShouldHitLocal(1.0, e.KBVersion, version) {
		return false, nil
	}
	_ = s.Cache.Touch(e.ID) // 命中刷新 LRU 时间戳
	s.MetricsRegistry().ObserveAskCache("hit")
	log.Printf("agmem: ask cache HIT(exact) id=%s version=%d", cache.SanitizeID(e.ID), version)
	return true, []byte(e.ResultJSON)
}

// askCacheNearest 向量近邻命中：在 dir scope 内找与 qvec 最相似的历史问题，命中
// 判定沿用 internal/cache 的 ShouldHitLocal（相似度严格 > 阈值 且 kb_version 一致）
// ——知识库变版整体失效回上游。仅此函数（每次请求的最后一处缓存判定）计 miss，
// 与 askCacheExact 的 hit 计点合起来保证「每请求恰好一次 hit/miss 打点」。
// fail-open：Cache==nil、版本锚点不可读、查询出错 一律 (false,nil)。
func (s *Server) askCacheNearest(req askReq, qvec []float32, version uint64, verr error) (bool, []byte) {
	if s.Cache == nil || verr != nil {
		return false, nil
	}
	e, sim, err := s.Cache.Nearest(req.Dir, qvec)
	if err != nil {
		if err != cache.ErrCacheNotFound {
			log.Printf("agmem: ask cache lookup: %v", err) // 查询失败按未命中处理（fail-open）
		}
		s.MetricsRegistry().ObserveAskCache("miss")
		return false, nil
	}
	if cache.ShouldHitLocal(sim, e.KBVersion, version) {
		_ = s.Cache.Touch(e.ID) // 命中刷新 LRU 时间戳
		s.MetricsRegistry().ObserveAskCache("hit")
		log.Printf("agmem: ask cache HIT(nearest) id=%s sim=%.4f version=%d", cache.SanitizeID(e.ID), sim, version)
		return true, []byte(e.ResultJSON)
	}
	// 版本失配且相似度过线：旧结果已失效，删除僵尸条目——否则确定性 id 的旧版本
	// 行会一直被 Nearest 选中做无用比较（与 agmemd askCached 同一清理语义）。
	if sim > cache.CurrentSimThreshold() && e.KBVersion != version {
		if _, cerr := s.Cache.Clear(e.ID); cerr != nil {
			log.Printf("agmem: ask cache evict stale %s: %v", cache.SanitizeID(e.ID), cerr)
		}
	}
	log.Printf("agmem: ask cache miss sim=%.4f local_v=%d remote_v=%d", sim, e.KBVersion, version)
	s.MetricsRegistry().ObserveAskCache("miss")
	return false, nil
}

// putAskCache 落缓存：键 = sha256(dir|question) 确定性 id，ON CONFLICT 覆盖为最新
// 结果（同一问题重写替换旧结果；见 ids.go 设计本意）。KB 为溯源字段。
// fail-open：Cache==nil 或版本锚点不可读（verr != nil）时不落；落库/LRU 失败仅日志。
func (s *Server) putAskCache(req askReq, qvec []float32, version uint64, core []byte, verr error) {
	if s.Cache == nil || verr != nil {
		return
	}
	if err := s.Cache.Put(cache.Entry{
		ID: cache.QueryID(req.Dir, req.Query), KB: s.Cfg.KB, Dir: req.Dir,
		Global: req.Global, Question: req.Query,
		QuestionVec: cache.EncodeVec(qvec), ResultJSON: string(core), KBVersion: version,
	}); err != nil {
		log.Printf("agmem: ask cache store: %v", err)
		return
	}
	if freed, err := s.Cache.EvictOver(s.cacheMaxBytes()); err != nil {
		log.Printf("agmem: ask cache evict: %v", err)
	} else if freed > 0 {
		log.Printf("agmem: ask cache LRU evicted %d bytes", freed)
	}
}

// writeAskJSON 写出 /api/ask 响应：上游契约核心体（kb/global/version/hits/filter）
// 前置一个 additive 顶层字段 cache_hit（true=本地命中，false=走检索链路），供验收与
// 观测判定。
//
// 为什么手工拼接而非二次 marshal：core 由首次序列化得到、命中时直接复用其落库字节，
// 只把 core 去掉首个 '{' 后拼到 `{"cache_hit":<bool>,` 之后——hits 子串与落库字节
// 完全相同，杜绝「命中重编导致 hits 与之漂移」。core 恒为合法 JSON 对象（map 序列化）。
func (s *Server) writeAskJSON(w http.ResponseWriter, core []byte, hit bool) {
	flag := "false"
	if hit {
		flag = "true"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if len(core) > 0 && core[0] == '{' {
		_, _ = w.Write([]byte(`{"cache_hit":` + flag + `,`))
		_, _ = w.Write(core[1:])
		return
	}
	_, _ = w.Write([]byte(`{"cache_hit":` + flag + `}`))
}

// normalizeQueryDir 把用户目录名归一为 clean 镜像目录名（G05）：clean 点的
// payload.dir 写为 `<dir>-clean`（cleaner worker），查询侧统一加后缀；已带
// `-clean` 的写法原样接受（幂等）；空目录名（不限目录）保持空。目录不存在时
// qdrant filter 自然 0 命中，不报错（保持既有空结果语义）。
func normalizeQueryDir(dir string) string {
	if dir == "" || strings.HasSuffix(dir, "-clean") {
		return dir
	}
	return dir + "-clean"
}

// filterDiag 阈值过滤诊断（G06）：本次检索的过滤口径、保留/丢弃数量与丢弃原因。
// 随 /api/ask 响应恒返回（additive）。口径与 UC-001 A6 对齐：hybrid 保留条件 =
// dense_score≥阈值 或 sparse 强命中（响应 score 是 rrf 排名分，不参与阈值）；
// legacy（AGMEM_HYBRID=off）保留条件 = score≥阈值。
type filterDiag struct {
	Mode          string  `json:"mode"`      // hybrid | score
	Threshold     float64 `json:"threshold"` // AGMEM_SCORE_THRESHOLD
	Rule          string  `json:"rule"`      // 保留条件（人类可读）
	Kept          int     `json:"kept"`
	Dropped       int     `json:"dropped"`
	DroppedReason string  `json:"dropped_reason,omitempty"`
	// Deduped 阈值过滤之后被近重复折叠掉的条数（dedupe.go）。
	// Kept 是阈值过滤后的条数，不含折叠；最终返回 len(hits) == Kept - Deduped。
	Deduped int `json:"deduped"`
}

// filterHitsByScoreDiag legacy 阈值过滤 + 诊断（score < threshold 丢弃）。
func filterHitsByScoreDiag(hits []qdrant.SearchHit, threshold float64) ([]qdrant.SearchHit, filterDiag) {
	out := make([]qdrant.SearchHit, 0, len(hits))
	for _, h := range hits {
		if h.Score >= threshold {
			out = append(out, h)
		}
	}
	d := filterDiag{Mode: "score", Threshold: threshold, Rule: "score>=threshold",
		Kept: len(out), Dropped: len(hits) - len(out)}
	if d.Dropped > 0 {
		d.DroppedReason = "score<threshold"
	}
	return out, d
}

// filterHitsByScore 分数阈值过滤（UC-1）：丢弃 score < threshold 的命中（阈值本身保留）。
// 始终返回非 nil 切片——JSON 序列化为 [] 而非 null（全被过滤时空数组语义）。
func filterHitsByScore(hits []qdrant.SearchHit, threshold float64) []qdrant.SearchHit {
	out, _ := filterHitsByScoreDiag(hits, threshold)
	return out
}

// filterHybridHits 混合检索阈值过滤（2026-09-08 设计拍板，2026-09-09 词法门槛
// 下移 qdrant 层后语义不变）：
//
//	保留条件 = DenseScore（dense 余弦相似度）≥ threshold
//	          或 SparseStrong（qdrant 层判定的词法强命中）
//	排序保持 Score（rrf 排名分）降序不变，过滤不改序。
//
// 设计语义：dense 达标 = 语义相关；SparseStrong = 精确词法相关（端口/命令号/
// 专有名词类查询——如「npm go pip 依赖下载走哪个端口」的 3128 条目 bm25 rank0，
// dense < 0.5，纯 dense 阈值曾误杀）。两者并集即「相关性强的知识」。
// 为什么不能直接拿 rrf 分过阈值：rrf 分 = Σ 1/(k+rank)（k=2，两分支）是排名
// 分，量纲与 cosine 无关——同一个 0.5 在两种分数下含义完全不同。dense 维度
// 阈值向后兼容（SearchHybrid 的 hit 恒带 DenseScore：稀疏空退化路径
// DenseScore=Score）；词法豁免只放行带稀疏证据的命中（SparseStrong 由
// qdrant.SearchHybrid 计算，无稀疏数据恒为 false）。
// 已知边界：sparse 弱命中（相对门槛不达）且 dense 不可见的点位仍被过滤——
// 两路都看不见的点位宁可漏过不误放（UC-1 宁缺毋滥同语义）。
// 始终返回非 nil 切片（同 filterHitsByScore）。
//
// 词法门槛下移依据（2026-09-09 实测）：qdrant 真实查询 sparse 分只有 1.0–7.5，
// 旧本地门槛 8.0 不可达 = 死代码；「i-0123456789abcdef0」「csidriver 扩容」等精确 token
// 查询 dense 最高只有 0.36–0.44，会被阈值清空。改由 qdrant 层按「该池榜首的
// 相对比例 + 绝对下限」判定（见 qdrant.sparseStrong* 常量），精确 token 查询可召回。
func filterHybridHits(hits []qdrant.SearchHit, threshold float64) []qdrant.SearchHit {
	out, _ := filterHybridHitsDiag(hits, threshold)
	return out
}

// filterHybridHitsDiag hybrid 阈值过滤 + 诊断（G06：丢弃原因可观测）。
func filterHybridHitsDiag(hits []qdrant.SearchHit, threshold float64) ([]qdrant.SearchHit, filterDiag) {
	out := make([]qdrant.SearchHit, 0, len(hits))
	for _, h := range hits {
		if h.DenseScore >= threshold || h.SparseStrong {
			out = append(out, h)
		}
	}
	d := filterDiag{Mode: "hybrid", Threshold: threshold,
		Rule: "dense_score>=threshold || sparse_strong",
		Kept: len(out), Dropped: len(hits) - len(out)}
	if d.Dropped > 0 {
		d.DroppedReason = "dense_score<threshold 且非 sparse 强命中（响应 score 为 rrf 排名分，不参与阈值）"
	}
	return out, d
}

// filterByThreshold 按 HybridEnabled 分发 UC-1 阈值过滤：hybrid 用「dense 达标
// 或 sparse 强命中」并集语义（rrf 分数量纲不同，不参与阈值），legacy 用 Score
// （cosine）。单库检索路径唯一出口；同时返回过滤诊断（G06）。
func (s *Server) filterByThreshold(hits []qdrant.SearchHit) ([]qdrant.SearchHit, filterDiag) {
	if s.Cfg.HybridEnabled {
		return filterHybridHitsDiag(hits, s.Cfg.ScoreThreshold)
	}
	return filterHitsByScoreDiag(hits, s.Cfg.ScoreThreshold)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	kb, ok := s.kbAccess(w, r)
	if !ok {
		return
	}
	v, err := s.Store.GetVersion(r.Context(), kb.ID)
	if err != nil {
		internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kb": kb.Name, "version": v})
}

// handleVersionGlobal GET /api/version/global（单库化兼容端点，2026-09-08）：
// 返回全局唯一 kb（cfg.KB）的数据版本，形状与 /api/version/{kb} 完全一致。
// 历史语义（跨 kb 版本指纹 sha256 hex）已随单库化退役——旧 agmemd 把本端点当
// 指纹解析：数值 version 无法按 hex 字符串解码会自动回退直通不缓存（安全降级，
// 不产生脏读）；新 agmemd 版本门禁一律走 /api/version/{cfg.KB}，本端点仅为
// 旧 wire 兼容保留。
func (s *Server) handleVersionGlobal(w http.ResponseWriter, r *http.Request) {
	kb, err := s.getSingleKB(r.Context(), PrincipalOf(r.Context()))
	if err != nil {
		if errors.Is(err, errNotFoundMasked) {
			writeErr(w, http.StatusNotFound, "kb 不存在: "+s.Cfg.KB)
			return
		}
		internalErr(w, err)
		return
	}
	v, err := s.kbStore().GetVersion(r.Context(), kb.ID)
	if err != nil {
		internalErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kb": kb.Name, "version": v})
}

// —— OIDC 链路 ——

// handleLogin GET /api/login → 302 Logto。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeErr(w, http.StatusServiceUnavailable, "OIDC 未配置（AGMEM_LOGTO_* / AGMEM_EXTERNAL_URL）")
		return
	}
	if ok, err := s.OIDC.HandleLogin(w, r); err != nil {
		log.Printf("api: oidc login: %v", err)
		writeErr(w, http.StatusBadGateway, "OIDC discovery 失败: "+err.Error())
	} else if !ok {
		writeErr(w, http.StatusInternalServerError, "login redirect 未发送")
	}
}

// handleCallback GET /api/callback → 建 session → 302 /。
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeErr(w, http.StatusServiceUnavailable, "OIDC 未配置")
		return
	}
	sub, err := s.OIDC.HandleCallback(w, r)
	if err != nil {
		log.Printf("api: oidc callback: %v", err)
		writeErr(w, http.StatusBadRequest, "OIDC callback 失败: "+err.Error())
		return
	}
	sid, err := s.Session.Put(sub)
	if err != nil {
		internalErr(w, err)
		return
	}
	secure := s.Cfg.ExternalURL != "" && len(s.Cfg.ExternalURL) > 8 && s.Cfg.ExternalURL[:8] == "https://"
	http.SetCookie(w, &http.Cookie{
		Name: "agmem_session", Value: sid, Path: "/",
		HttpOnly: true, Secure: secure,
		MaxAge: 24 * 3600, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// —— helpers ——

func int64Path(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	var v int64
	for _, c := range r.PathValue(name) {
		if c < '0' || c > '9' {
			writeErr(w, http.StatusBadRequest, name+" 须为正整数")
			return 0, false
		}
		v = v*10 + int64(c-'0')
	}
	if v <= 0 {
		writeErr(w, http.StatusBadRequest, name+" 须为正整数")
		return 0, false
	}
	return v, true
}

func containsSlash(s string) bool {
	for _, c := range s {
		if c == '/' {
			return true
		}
	}
	return false
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func internalErr(w http.ResponseWriter, err error) {
	log.Printf("api: internal error: %v", err)
	writeErr(w, http.StatusInternalServerError, "internal error")
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
