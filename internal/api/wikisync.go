// wikisync.go：飞书 wiki 增量同步的进程内适配层。
//
// internal/wikisync 定义 KBUpdater 接口，本包实现（避免服务自己 HTTP 打自己）：
//   - EnqueueDoc：与 POST /api/kb/{kb}/docs 同一入队原语（CreateTaskWithItems）；
//   - DeleteItem：与 DELETE /api/kb/{kb}/items/{id} 同一删除实现（deleteItemCascade）。
//
// 另提供 /api/wiki-sync/status（与 /api/healthz 的 wiki_sync 段同内容）。
package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"sre/agmem/internal/store"
	"sre/agmem/internal/wikisync"
)

// EnqueueDoc 实现 wikisync.KBUpdater：把文档（可能已切段）的全部段落一次入队为清洗任务。
// dir=父节点标题；目录不存在时自动创建（wiki 目录由树结构决定，与 POST /docs 的
// 「目录必须先存在」语义不同）。每段的来源名与正文按下标一一对应传给
// CreateTaskWithItems(texts, names)；返回 task id 与新条目 id 列表；
// 全部段落都与库内既有条目重复时 dup=true（store 返回 ErrAllDuplicates，调用方保留旧版本）。
func (s *Server) EnqueueDoc(ctx context.Context, dir string, parts []wikisync.DocPart) (int64, []int64, bool, error) {
	kb, err := s.Store.GetKB(ctx, s.Cfg.KB)
	if err != nil {
		return 0, nil, false, err
	}
	dir = normalizeWikiDir(dir)
	d, err := s.Store.GetKBDirByName(ctx, kb.ID, dir)
	if errors.Is(err, store.ErrNotFound) {
		if _, cerr := s.Store.CreateDir(ctx, kb.ID, dir); cerr != nil && !errors.Is(cerr, store.ErrExists) {
			return 0, nil, false, cerr
		}
		d, err = s.Store.GetKBDirByName(ctx, kb.ID, dir)
	}
	if err != nil {
		return 0, nil, false, err
	}
	if s.Qdrant != nil {
		if err := s.Qdrant.EnsureCollection(ctx, kb.Name, s.Cfg.QdrantDim); err != nil {
			log.Printf("api: ensure collection %s: %v", kb.Name, err)
		}
	}
	// 段名与正文同序：texts[i] ↔ names[i]（CreateTaskWithItems 按同一下标取 source_name）。
	texts := make([]string, len(parts))
	names := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p.Text
		names[i] = wikiSourceName(p.Name)
	}
	// 用 NoDedup 变体：wiki 同步自己负责「先入队新版、再删旧版」，走常规去重会让
	// 变更文档里未改动的段被判重复跳过，随后旧条目被删 → 内容丢失（见 store 注释）。
	// 审计：actor=system:wikisync（自动写入也留痕），与业务写同事务（见 store.WithAudit）。
	actx := store.WithAudit(ctx, store.AuditEntry{
		ActorSub: "system:wikisync", ActorRole: "system", Action: "wikisync_enqueue",
		KB: kb.Name, ObjectID: dir,
	})
	taskID, _, err := s.Store.CreateTaskWithItemsNoDedup(actx, kb.ID, d.ID, texts, names)
	if err != nil {
		if errors.Is(err, store.ErrAllDuplicates) {
			return 0, nil, true, nil
		}
		return 0, nil, false, err
	}
	// 有新条目入队：唤醒空轮询退避中的 worker（非阻塞；nil 钩子无操作）。
	if s.OnEnqueue != nil {
		s.OnEnqueue()
	}
	// 回查新条目 id：state 需要它们以便下次变更时删旧版本。
	task, err := s.Store.GetTask(ctx, kb.ID, taskID)
	if err != nil {
		return taskID, nil, false, err
	}
	ids := make([]int64, 0, len(task.Items))
	for _, it := range task.Items {
		ids = append(ids, it.ID)
	}
	return taskID, ids, false, nil
}

// DeleteItem 实现 wikisync.KBUpdater：与 DELETE /api/kb/{kb}/items/{id} 同一删除
// 实现（qdrant 点位 → MySQL 行 → 版本 +1 + 审计），供 wiki 同步替换旧版本调用。
func (s *Server) DeleteItem(ctx context.Context, itemID int64) error {
	kb, err := s.Store.GetKB(ctx, s.Cfg.KB)
	if err != nil {
		return err
	}
	actx := store.WithAudit(ctx, store.AuditEntry{
		ActorSub: "system:wikisync", ActorRole: "system", Action: "wikisync_delete_item",
		KB: kb.Name, ObjectID: itoa(itemID),
	})
	points, taskID, _, err := s.deleteItemCascade(actx, kb, itemID)
	if err != nil {
		return err
	}
	s.refreshTask(ctx, taskID)
	log.Printf("api: audit action=wikisync_delete_item kb=%s item_id=%d result=ok deleted_points=%d",
		kb.Name, itemID, points)
	return nil
}

// handleWikiSyncStatus GET /api/wiki-sync/status：与 /api/healthz 的 wiki_sync 段
// 同内容（便于单独查看同步状态）。
func (s *Server) handleWikiSyncStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.wikiSyncStatus())
}

// wikiSyncStatus 组装 wiki 同步状态段（provider 缺失时 enabled=false 的空快照）。
func (s *Server) wikiSyncStatus() wikisync.Status {
	if s.WikiSync == nil {
		return wikisync.Status{Enabled: false}
	}
	return s.WikiSync.Snapshot()
}

// normalizeWikiDir 目录名归一（与 wikisync.sanitizeDir 同口径的兜底）：
// 去空白、"/"→"-"、空→wiki、超长截断（directories.name VARCHAR(128)）。
func normalizeWikiDir(dir string) string {
	d := strings.ReplaceAll(strings.TrimSpace(dir), "/", "-")
	d = strings.TrimSpace(d)
	if d == "" {
		return "wiki"
	}
	if r := []rune(d); len(r) > 100 {
		d = string(r[:100])
	}
	return d
}

// wikiSourceName 入库文档名归一（documents.source_name VARCHAR(255)）：去换行/回车、
// 去首尾空白、空→未命名文档、超长截断。名字原样进入 clean 条目「来源：」行，是核实依据。
func wikiSourceName(title string) string {
	n := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(title)
	n = strings.TrimSpace(n)
	if n == "" {
		return "未命名文档"
	}
	if r := []rune(n); len(r) > 255 {
		n = string(r[:255])
	}
	return n
}
