package api

// lifecycle.go：知识生命周期管理端点（UC-011 A1/A3/A4/A5）——条目删除、失败重试、
// kb reader 增删查。
//
// 权限：全部经 s.auth（未登录 401）+ ownerKB 写权限校验（reader 403；非读者按
// 404 掩蔽，与既有 kbAccess 语义一致）；机器 token 全量可写。
//
// 一致性：删除/重试/reader 确有变更后一律 BumpVersion（沿用既有 GetVersion /
// BumpVersion 通道，不新增表），使 /api/version/global 指纹变化、本地缓存失效
// （UC-011 A1/A4）。幂等重放（reader 已存在/已删除）不 bump，避免缓存抖动。
//
// 审计：每个成功变更写一条结构化 log（principal/kb/动作/结果），非静默（A5）。
//
// 排除：UC-011 A6「kb 退役」在单库形态下会删掉唯一知识库（不可恢复），本批不实现，
// 见 usecase/UC-011-knowledge-lifecycle.md 状态行。
//
// 删除顺序：先删 qdrant 点位（显式 id 清单，绝不用 item_id filter 删——2026-09-07
// 误删事故，见 qdrant.DeleteByID 注释）再删 MySQL 行。反序会出现「行已删但点位仍在」
// → ask 继续命中已删知识（A1 直接失败）。点位删除成功而 MySQL 删除失败时返回 500
// 并留审计行，避免静默半完成。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"sre/agmem/internal/qdrant"
	"sre/agmem/internal/store"
)

// ownerKB 写路径守卫（UC-011 写端点共用）：先做单库写守卫（路径 {kb} 必须等于
// cfg.KB，G36 跨库写/删除防护——线上存在第二 kb `otherkb`，历史行为是忽略路径段、
// 实际作用在 cfg.KB 上），再解析 cfg.KB 并校验 owner。
// 返回 false 时已写出响应：路径 kb 不匹配 404 / 非读者 404（掩蔽）/
// 非 owner 的 reader 403 / 内部错误 500。
func (s *Server) ownerKB(w http.ResponseWriter, r *http.Request) (*store.KB, bool) {
	if !s.requireWritableKB(w, r) {
		return nil, false
	}
	return s.ownerKBFor(w, r, s.Cfg.KB)
}

// ownerKBFor 解析指定 kb 名并校验读+owner 权限（reader 403；非读者/不存在 404
// 掩蔽；机器 token 全量）。只读管理端点（reader 列表）用它按路径 {kb} 读实际库，
// 使第二 kb 可只读（G36）而不误作用到 cfg.KB。
func (s *Server) ownerKBFor(w http.ResponseWriter, r *http.Request, name string) (*store.KB, bool) {
	p := PrincipalOf(r.Context())
	st := s.lifecycle()
	kb, err := st.GetKB(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "kb 不存在: "+name)
			return nil, false
		}
		internalErr(w, err)
		return nil, false
	}
	ok, err := st.HasReadAccess(r.Context(), kb.ID, p.Sub, p.Machine)
	if err != nil {
		internalErr(w, err)
		return nil, false
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "kb 不存在: "+name) // 掩蔽
		return nil, false
	}
	if !p.Machine && p.Sub != kb.OwnerSub {
		writeErr(w, http.StatusForbidden, "仅 kb owner 可执行该操作")
		return nil, false
	}
	return kb, true
}

// principalID 审计用主体标识（机器 token → "machine"）。
func principalID(p Principal) string {
	switch {
	case p.Machine:
		return "machine"
	case p.Readonly:
		return "client-readonly"
	case p.Sub == "":
		return "anonymous"
	default:
		return p.Sub
	}
}

// principalRole 审计用角色标识（与 handleMe 的 role 口径同源）。
func principalRole(p Principal) string {
	switch {
	case p.Machine:
		return "machine"
	case p.Readonly:
		return "client-readonly"
	case p.Sub == "":
		return "anonymous"
	default:
		return "user"
	}
}

// clientIP 解析请求来源 IP：优先 X-Forwarded-For 首个（经反向代理，
// RemoteAddr 是网关地址），否则取 RemoteAddr 的 host 部分。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if v := strings.TrimSpace(xff); v != "" {
			return v
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// handleDeleteItem DELETE /api/kb/{kb}/items/{item_id}（UC-011 A1）：
// 删除条目全部 qdrant 点位（original + 全部 clean entry）→ 删除 MySQL 行 →
// 版本 +1 → 审计。响应 {"deleted_points":N,"version":M}。
// 条目不存在（含二次调用）→ 404 明确错误体，不静默 200。
func (s *Server) handleDeleteItem(w http.ResponseWriter, r *http.Request) {
	kb, ok := s.ownerKB(w, r)
	if !ok {
		return
	}
	itemID, ok := int64Path(w, r, "item_id")
	if !ok {
		return
	}
	p := PrincipalOf(r.Context())
	// 审计主体挂 context：store.DeleteItem 据此把「删行 + 审计」写进同一事务。
	actx := store.WithAudit(r.Context(), store.AuditEntry{
		ActorSub: principalID(p), ActorRole: principalRole(p), Action: "delete_item",
		KB: kb.Name, ObjectID: itoa(itemID), SourceIP: clientIP(r),
	})
	points, taskID, version, err := s.deleteItemCascade(actx, kb, itemID)
	if err != nil {
		var rg *raceGoneError
		var ue *upstreamError
		var be *bumpError
		switch {
		case errors.As(err, &rg):
			// 并发删除：点位已清、元数据已被他处删除 → 仍按 404 幂等语义返回
			log.Printf("api: audit action=delete_item principal=%s kb=%s item_id=%d result=race_gone",
				principalID(p), kb.Name, itemID)
			writeErr(w, http.StatusNotFound, "条目不存在: "+itoa(itemID))
		case errors.As(err, &ue):
			writeErr(w, http.StatusBadGateway, ue.msg)
		case errors.As(err, &be):
			// 点位与元数据已删，版本未递增 → 缓存可能仍持有旧命中；显式审计 + 500。
			log.Printf("api: audit action=delete_item principal=%s kb=%s item_id=%d result=bump_failed err=%v",
				principalID(p), kb.Name, itemID, be.err)
			internalErr(w, be.err)
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, "条目不存在: "+itoa(itemID))
		default:
			internalErr(w, err)
		}
		return
	}
	s.refreshTask(r.Context(), taskID)
	log.Printf("api: audit action=delete_item principal=%s kb=%s item_id=%d result=ok deleted_points=%d version=%d",
		principalID(p), kb.Name, itemID, points, version)
	writeJSON(w, http.StatusOK, map[string]any{
		"kb": kb.Name, "item_id": itemID, "deleted_points": points, "version": version,
	})
}

// —— 删除核心（DELETE 端点与 wiki 同步换版共用同一实现） ——

// upstreamError qdrant 上游失败：与内部错误区分（API 映射 502 且保留原始文案）。
type upstreamError struct{ msg string }

func (e *upstreamError) Error() string { return e.msg }

// raceGoneError 并发删除（GetItem 命中后行被他人删除）：映射 404 幂等语义。
type raceGoneError struct{ err error }

func (e *raceGoneError) Error() string { return e.err.Error() }
func (e *raceGoneError) Unwrap() error { return e.err }

// bumpError 版本递增失败（点位与元数据已删）：映射 500 且写 bump_failed 审计。
type bumpError struct{ err error }

func (e *bumpError) Error() string { return e.err.Error() }
func (e *bumpError) Unwrap() error { return e.err }

// deleteItemCascade 条目删除核心：qdrant 点位（显式 id 清单）→ MySQL 行 → 版本 +1。
// DELETE /items/{id} 与 wiki 同步换版共用同一实现——先删点位再删行（反序会出现
// 「行已删但点位仍在」→ ask 继续命中已删知识）。返回删除点位数、条目 taskID、新版本。
// 错误分类：条目不存在 → store.ErrNotFound；qdrant 失败 → *upstreamError；
// 并发删除 → *raceGoneError（Unwrap 到 ErrNotFound）；bump 失败 → *bumpError。
func (s *Server) deleteItemCascade(ctx context.Context, kb *store.KB, itemID int64) (int, int64, uint64, error) {
	item, err := s.lifecycle().GetItem(ctx, kb.ID, itemID)
	if err != nil {
		return 0, 0, 0, err // ErrNotFound → 调用方 404
	}
	ids, err := s.itemPointIDs(ctx, kb.Name, itemID)
	if err != nil {
		return 0, item.TaskID, 0, &upstreamError{"qdrant 枚举条目点位失败: " + err.Error()}
	}
	if len(ids) > 0 {
		if err := s.Qdrant.DeleteByID(ctx, kb.Name, ids); err != nil {
			return 0, item.TaskID, 0, &upstreamError{"qdrant 删除条目点位失败: " + err.Error()}
		}
	}
	if err := s.lifecycle().DeleteItem(ctx, kb.ID, itemID); err != nil {
		return len(ids), item.TaskID, 0, &raceGoneError{err}
	}
	v, err := s.lifecycle().BumpVersion(ctx, kb.ID)
	if err != nil {
		return len(ids), item.TaskID, 0, &bumpError{err}
	}
	return len(ids), item.TaskID, v, nil
}

// handleRetryItem POST /api/kb/{kb}/items/{item_id}/retry（UC-011 主流程 4）：
// 仅 failed → pending 重排（worker 轮询自动重跑）；非 failed → 409（附当前状态）；
// 成功后版本 +1（状态变化必须让缓存失效）→ 审计。响应含新状态。
func (s *Server) handleRetryItem(w http.ResponseWriter, r *http.Request) {
	kb, ok := s.ownerKB(w, r)
	if !ok {
		return
	}
	itemID, ok := int64Path(w, r, "item_id")
	if !ok {
		return
	}
	p := PrincipalOf(r.Context())
	actx := store.WithAudit(r.Context(), store.AuditEntry{
		ActorSub: principalID(p), ActorRole: principalRole(p), Action: "retry_item",
		KB: kb.Name, ObjectID: itoa(itemID), SourceIP: clientIP(r),
	})
	item, err := s.lifecycle().GetItem(r.Context(), kb.ID, itemID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "条目不存在: "+itoa(itemID))
			return
		}
		internalErr(w, err)
		return
	}
	if item.Status != store.StatusFailed {
		writeErr(w, http.StatusConflict, "仅 failed 条目可重试，当前状态: "+string(item.Status))
		return
	}
	st, err := s.lifecycle().RequeueItem(actx, kb.ID, itemID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, "条目不存在: "+itoa(itemID))
		case errors.Is(err, store.ErrInvalidState):
			writeErr(w, http.StatusConflict, "仅 failed 条目可重试，当前状态: "+string(st))
		default:
			internalErr(w, err)
		}
		return
	}
	v, err := s.lifecycle().BumpVersion(r.Context(), kb.ID)
	if err != nil {
		log.Printf("api: audit action=retry_item principal=%s kb=%s item_id=%d result=bump_failed err=%v",
			principalID(p), kb.Name, itemID, err)
		internalErr(w, err)
		return
	}
	s.refreshTask(r.Context(), item.TaskID)
	log.Printf("api: audit action=retry_item principal=%s kb=%s item_id=%d result=ok status=%s version=%d",
		principalID(p), kb.Name, itemID, st, v)
	writeJSON(w, http.StatusOK, map[string]any{
		"kb": kb.Name, "item_id": itemID, "status": string(st), "version": v,
	})
}

// handleListReaders GET /api/kb/{kb}/readers（owner-only；只读，按路径 {kb} 解析，
// 第二 kb 可读——G36 写/删除才拒绝）。
func (s *Server) handleListReaders(w http.ResponseWriter, r *http.Request) {
	kb, ok := s.ownerKBFor(w, r, r.PathValue("kb"))
	if !ok {
		return
	}
	readers, err := s.lifecycle().ListReaders(r.Context(), kb.ID)
	if err != nil {
		internalErr(w, err)
		return
	}
	if readers == nil {
		readers = []string{} // JSON [] 而非 null（空列表契约）
	}
	writeJSON(w, http.StatusOK, map[string]any{"kb": kb.Name, "readers": readers})
}

// readersReq 新增 reader 请求体：sub 为主字段；feishu_user 为 UC-011 术语别名
// （Logto 飞书登录下 sub 即飞书用户标识），二者取其一。
type readersReq struct {
	Sub        string `json:"sub"`
	FeishuUser string `json:"feishu_user"`
}

// handleAddReader POST /api/kb/{kb}/readers（owner-only，幂等）：
// 新插入 → 版本 +1；已存在（重放）→ 版本不变。
func (s *Server) handleAddReader(w http.ResponseWriter, r *http.Request) {
	kb, ok := s.ownerKB(w, r)
	if !ok {
		return
	}
	var req readersReq
	if !readJSON(w, r, &req) {
		return
	}
	sub := strings.TrimSpace(req.Sub)
	if sub == "" {
		sub = strings.TrimSpace(req.FeishuUser)
	}
	if sub == "" {
		writeErr(w, http.StatusBadRequest, "sub 必填")
		return
	}
	if strings.Contains(sub, "/") {
		writeErr(w, http.StatusBadRequest, "sub 不得包含 /（会破坏 readers/{sub} 路径语义）")
		return
	}
	p := PrincipalOf(r.Context())
	actx := store.WithAudit(r.Context(), store.AuditEntry{
		ActorSub: principalID(p), ActorRole: principalRole(p), Action: "add_reader",
		KB: kb.Name, ObjectID: sub, SourceIP: clientIP(r),
	})
	added, err := s.lifecycle().AddReader(actx, kb.ID, sub)
	if err != nil {
		internalErr(w, err)
		return
	}
	v, err := s.versionAfterChange(r.Context(), kb.ID, added)
	if err != nil {
		internalErr(w, err)
		return
	}
	log.Printf("api: audit action=add_reader principal=%s kb=%s sub=%s result=added:%t version=%d",
		principalID(p), kb.Name, sub, added, v)
	writeJSON(w, http.StatusOK, map[string]any{
		"kb": kb.Name, "sub": sub, "added": added, "version": v,
	})
}

// handleRemoveReader DELETE /api/kb/{kb}/readers/{sub}（owner-only，幂等）：
// 确有删除 → 版本 +1；本就不存在（重放）→ 版本不变。
func (s *Server) handleRemoveReader(w http.ResponseWriter, r *http.Request) {
	kb, ok := s.ownerKB(w, r)
	if !ok {
		return
	}
	sub := strings.TrimSpace(r.PathValue("sub"))
	if sub == "" {
		writeErr(w, http.StatusBadRequest, "sub 必填")
		return
	}
	p := PrincipalOf(r.Context())
	actx := store.WithAudit(r.Context(), store.AuditEntry{
		ActorSub: principalID(p), ActorRole: principalRole(p), Action: "remove_reader",
		KB: kb.Name, ObjectID: sub, SourceIP: clientIP(r),
	})
	removed, err := s.lifecycle().RemoveReader(actx, kb.ID, sub)
	if err != nil {
		internalErr(w, err)
		return
	}
	v, err := s.versionAfterChange(r.Context(), kb.ID, removed)
	if err != nil {
		internalErr(w, err)
		return
	}
	log.Printf("api: audit action=remove_reader principal=%s kb=%s sub=%s result=removed:%t version=%d",
		principalID(p), kb.Name, sub, removed, v)
	writeJSON(w, http.StatusOK, map[string]any{
		"kb": kb.Name, "sub": sub, "removed": removed, "version": v,
	})
}

// versionAfterChange 变更类操作后的版本取值：确有变更 → BumpVersion；
// 幂等重放（无变更）→ GetVersion（不抖动缓存指纹）。
func (s *Server) versionAfterChange(ctx context.Context, kbID int64, changed bool) (uint64, error) {
	if changed {
		return s.lifecycle().BumpVersion(ctx, kbID)
	}
	return s.lifecycle().GetVersion(ctx, kbID)
}

// refreshTask 条目终态变化后收敛任务级状态（best-effort，仅记日志不阻断——
// 与 cleaner.refreshTask 同语义）。
func (s *Server) refreshTask(ctx context.Context, taskID int64) {
	if taskID <= 0 {
		return
	}
	if _, err := s.lifecycle().RefreshTaskStatus(ctx, taskID); err != nil {
		log.Printf("api: refresh task %d: %v", taskID, err)
	}
}

// itemPointIDs 枚举条目在 qdrant 的全部点位 id（original + 全部 clean entry）。
//
// 删除必须用显式点 id 清单（DeleteByID 语义；绝不用 item_id filter 直删——同 item
// 并发写入会被误删，2026-09-07 事故教训，见 qdrant.DeleteByID 注释）。id 清单经
// qdrant.ScrollPointIDs 分页 scroll 取得：scroll 的 wire 形状与翻页细节封在
// internal/qdrant，api 层不感知 qdrant 协议。
func (s *Server) itemPointIDs(ctx context.Context, collection string, itemID int64) ([]string, error) {
	if s.Qdrant == nil {
		return nil, fmt.Errorf("qdrant 客户端未配置")
	}
	return s.Qdrant.ScrollPointIDs(ctx, collection, qdrant.SearchRequest{ItemID: itemID})
}
