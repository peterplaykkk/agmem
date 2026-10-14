// Package cachectl：agmemd 本地守护进程的 HTTP 服务端逻辑（可注入测试）。
//
// 端点：
//
//	GET  /healthz    状态（engine ready 与否）
//	POST /api/ask    查询：版本校验 → 本地近义命中（>0.8 且版本一致）→ 否则 agmem 上游并落缓存
//	                 单库版（2026-09-08）：版本锚点恒为 /api/version/{cfg.KB}（全局唯一 kb）；
//	                 请求里的 kb/global 字段保留解析（旧 CLI wire 兼容）但忽略。
//	POST /api/clear  {id?: string, all?: bool} → {freed_bytes}
package cachectl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"sre/agmem/internal/cache"
	"sre/agmem/internal/localembed"
)

// Config agmemd 配置（env 注入）。
type Config struct {
	Addr       string // 默认 127.0.0.1:8377
	CacheDir   string // 默认 ~/.cache/agmemd
	MaxBytes   int64  // LRU 上限，默认 500MB
	AgmemURL   string // 例 https://agmem-test.example.com
	AgmemToken string
	// KB 全局唯一知识库名（AGMEM_KB，默认 config.DefaultKB="sre"，agmemd 启动注入）。
	// 单库化后所有查询的版本门禁锚点都是 /api/version/{KB}。
	KB string
}

// Server agmemd 服务端。
type Server struct {
	Cfg    Config
	Store  *cache.Store
	Engine *localembed.Engine
	HTTP   *http.Client // 调 agmem 用（默认 15s 超时）
	// EmbedFn 测试注入点：非 nil 时优先于 Engine（生产为 nil）。
	EmbedFn func(text string) ([]float32, error)
}

// embed 用注入的 EmbedFn 或真实引擎。
func (s *Server) embed(text string) ([]float32, error) {
	if s.EmbedFn != nil {
		return s.EmbedFn(text)
	}
	if s.Engine == nil || !s.Engine.Ready() {
		return nil, localembed.ErrModelUnavailable
	}
	return s.Engine.Embed(text)
}

// engineReady 引擎是否可用。
func (s *Server) engineReady() bool {
	if s.EmbedFn != nil {
		return true
	}
	return s.Engine != nil && s.Engine.Ready()
}

// Routes 注册路由。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /api/ask", s.handleAsk)
	mux.HandleFunc("POST /api/clear", s.handleClear)
	return s.securityHeaders(mux)
}

// securityHeaders 本地回环也统一安全头（不新增绕过路径）。
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'")
		next.ServeHTTP(w, r)
	})
}

// handleHealthz GET /healthz：状态 + 引擎就绪 + 缓存有效上限。
// max_bytes 暴露 LRU 上限（AGMEMD_MAX_BYTES 覆盖后的有效值），供 UC-004 C1
// 回读默认 524288000，并让运维不查进程环境即可核对实际容量。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	status := "degraded"
	if s.engineReady() {
		status = "ok"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    status,
		"engine":    map[string]bool{"ready": s.engineReady()},
		"max_bytes": s.Cfg.MaxBytes,
	})
}

// askReq 与 agmem /api/ask 入参一致（透传 dir/top_k）。
// KB / Global：旧 wire 字段保留解析（老 CLI/老上游兼容）但单库版一律忽略。
type askReq struct {
	KB     string `json:"kb"` // deprecated：单库版忽略
	Dir    string `json:"dir,omitempty"`
	Query  string `json:"query"`
	TopK   int    `json:"top_k,omitempty"`
	Global bool   `json:"global,omitempty"` // deprecated：单库版忽略
}

// askResp 输出：cache_hit 标注 + 结果。
//
// Result 是 agmemd 的原始 wrapper（CLI `agmem` 消费，见 cmd/agmem 的 askResult）。
// agmemserv 的 /api/ask 契约则是顶层直接给 hits/kb/version/filter/global——
// 上游客户端（如 claude-ask-server）按该契约解析。为让这类客户端把 AGMEM_URL
// 指向本地 agmemd 即可零改动复用缓存，响应同时透传上游顶层字段（additive，
// 契约超集）：hits/kb/version/filter/global。CLI 读 result，其余消费者读顶层，
// 两条读法互不影响；透传字段缺失时按 omitempty 省略，wrapper 主字段不变。
type askResp struct {
	CacheHit   bool            `json:"cache_hit"`
	ID         string          `json:"id"`
	Similarity float64         `json:"similarity,omitempty"`
	KBVersion  uint64          `json:"kb_version"`
	Result     json.RawMessage `json:"result"`
	// —— 上游 agmem /api/ask 顶层字段透传（additive，契约超集）——
	Hits    json.RawMessage `json:"hits,omitempty"`
	KB      string          `json:"kb,omitempty"`
	Version uint64          `json:"version,omitempty"`
	Filter  json.RawMessage `json:"filter,omitempty"`
	Global  bool            `json:"global,omitempty"`
}

// newAskResp 构造响应并从上游体（或缓存里存的同一体）透传顶层字段。
// 解析失败/字段缺失时保持零值（omitempty 不出现）；绝不因透传失败改变状态码
// 或丢弃 wrapper 主字段——缓存命中与未命中走同一构造，保证结果语义一致。
func newAskResp(cacheHit bool, id string, sim float64, kbVersion uint64, upstream json.RawMessage) askResp {
	r := askResp{CacheHit: cacheHit, ID: id, Similarity: sim, KBVersion: kbVersion, Result: upstream}
	if len(upstream) == 0 {
		return r
	}
	var u struct {
		Hits    json.RawMessage `json:"hits"`
		Filter  json.RawMessage `json:"filter"`
		KB      string          `json:"kb"`
		Version uint64          `json:"version"`
		Global  bool            `json:"global"`
	}
	if json.Unmarshal(upstream, &u) == nil {
		r.Hits, r.Filter, r.KB, r.Version, r.Global = u.Hits, u.Filter, u.KB, u.Version, u.Global
	}
	return r
}

// handleAsk：版本校验（/api/version/{cfg.KB}）→ 本地命中（scope=(dir,question)）
// → 上游并落缓存。
// 单库化（2026-09-08）：不再有「无 kb 全局查询回退直通」分支——所有查询都有
// 确定的版本锚点；版本端点不可用 → 502（上游既已不可用，直通同样会失败），
// 本地 embedding 未就绪 → 503（与旧 kb 级路径一致）。
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req askReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Query == "" {
		writeErr(w, http.StatusBadRequest, "query 必填")
		return
	}
	version, err := s.remoteVersion(r.Context(), s.Cfg.KB)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "agmem 版本校验失败: "+err.Error())
		return
	}
	if !s.engineReady() {
		writeErr(w, http.StatusServiceUnavailable,
			"本地 embedding 未就绪（检查 onnxruntime 库与模型；见 agmemd 启动日志/README）")
		return
	}
	s.askCached(w, r, req, version)
}

// askCached 缓存路径（单库版，UC-3）：本地近义命中（相似度 >0.8 且版本一致）→
// 秒回；未命中 → agmem 上游并落缓存（Entry.KBVersion=单 kb 版本锚点）。
func (s *Server) askCached(w http.ResponseWriter, r *http.Request, req askReq, version uint64) {
	vec, err := s.embed(req.Query)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "本地 embedding 失败: "+err.Error())
		return
	}
	// 单库 scope = (dir, question)：kb/global 维度已随单库化退役（Nearest 仅按 dir 过滤）
	if e, sim, err := s.Store.Nearest(req.Dir, vec); err == nil {
		if cache.ShouldHitLocal(sim, e.KBVersion, version) {
			_ = s.Store.Touch(e.ID)
			writeJSON(w, http.StatusOK,
				newAskResp(true, e.ID, sim, version, json.RawMessage(e.ResultJSON)))
			return
		}
		// 版本不匹配且相似度过线：旧结果已失效，删除僵尸条目——否则确定性 id 被
		// exists 检查判碰撞而加时间戳后缀，旧条目永久遮蔽新结果（同问此后永不命中）。
		// 删除后同问可复用原 id 重写（ids.go 设计本意：确定性 id 供重写覆盖）。
		if sim > cache.CurrentSimThreshold() && e.KBVersion != version {
			if _, err := s.Store.Clear(e.ID); err != nil {
				log.Printf("agmemd: evict stale %s: %v", cache.SanitizeID(e.ID), err)
			}
		}
		log.Printf("agmemd: miss id=%s sim=%.4f local_v=%d remote_v=%d",
			cache.SanitizeID(e.ID), sim, e.KBVersion, version)
	}
	// 未命中 → agmem
	upstream, status, err := s.remoteAsk(r.Context(), req)
	if err != nil {
		writeErr(w, status, "agmem 查询失败: "+err.Error())
		return
	}
	// 落缓存：唯一 id（sha256(dir|question)，碰撞加时间戳）；KB/Global 为溯源字段
	id := cache.QueryIDUnique(req.Dir, req.Query, func(id string) bool {
		_, err := s.Store.Get(id)
		return err == nil
	})
	upstreamJSON, _ := json.Marshal(upstream)
	if err := s.Store.Put(cache.Entry{
		ID: id, KB: s.Cfg.KB, Dir: req.Dir, Global: req.Global, Question: req.Query,
		QuestionVec: cache.EncodeVec(vec), ResultJSON: string(upstreamJSON),
		KBVersion: version,
	}); err != nil {
		log.Printf("agmemd: store cache: %v", err) // 缓存失败不影响返回
	} else if freed, err := s.Store.EvictOver(s.Cfg.MaxBytes); err != nil {
		log.Printf("agmemd: evict: %v", err)
	} else if freed > 0 {
		log.Printf("agmemd: LRU evicted %d bytes", freed)
	}
	writeJSON(w, http.StatusOK, newAskResp(false, id, 0, version, upstreamJSON))
}

// remoteVersion GET agmem /api/version/{kb}。
func (s *Server) remoteVersion(ctx context.Context, kb string) (uint64, error) {
	body, status, err := s.agmemGet(ctx, "/api/version/"+kb)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("status %d: %s", status, truncate(body))
	}
	var vr struct {
		Version uint64 `json:"version"`
	}
	if err := json.Unmarshal(body, &vr); err != nil {
		return 0, err
	}
	return vr.Version, nil
}

// remoteAsk POST agmem /api/ask。
func (s *Server) remoteAsk(ctx context.Context, req askReq) (json.RawMessage, int, error) {
	if s.HTTP == nil {
		s.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	b, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.Cfg.AgmemURL+"/api/ask", bytes.NewReader(b))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.Cfg.AgmemToken)
	resp, err := s.HTTP.Do(httpReq)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, http.StatusBadGateway, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(body))
	}
	return json.RawMessage(body), http.StatusOK, nil
}

func (s *Server) agmemGet(ctx context.Context, path string) ([]byte, int, error) {
	if s.HTTP == nil {
		s.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Cfg.AgmemURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Cfg.AgmemToken)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, err
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID  string `json:"id"`
		All bool   `json:"all"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !req.All && req.ID == "" {
		writeErr(w, http.StatusBadRequest, "需要 id 或 all=true")
		return
	}
	var freed int64
	var err error
	if req.All {
		freed, err = s.Store.ClearAll()
	} else {
		freed, err = s.Store.Clear(req.ID)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"freed_bytes": freed})
}

// —— helpers ——

func truncate(b []byte) string {
	if len(b) > 256 {
		return string(b[:256]) + "..."
	}
	return string(b)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
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
