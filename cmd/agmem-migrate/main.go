// agmem-migrate: one-shot migrations of existing clean points. Modes:
//
//	(default)  scroll 全部 clean 点 → 长点位按条目重建（SplitEntries ≤600，
//	           确定性 point id 与在线 worker 一致 cleaner.EntryPointID）→ 重新
//	           embed → 先 upsert 新点 → 再按「旧显式 id − 新 id」差集删除。
//	           **绝不用 item_id filter 删除**（会连刚写入的新点一起删，2026-09-07
//	           事故根因，静态门禁 TestNoFilterDeleteInPackage 守门）。kb 版本 bump
//	           需另在 buildhost 执行。
//	SPARSE=1   为存量 clean 点回填 bm25 命名稀疏向量。
//	RECLEAN=1  originals dump → GLM 重清洗重建：差集删旧点、判空清旧点、失败清单
//	           落盘、版本 bump、执行锁（AGMEM_MYSQL_DSN）。
//	RESPLIT=1  clean_text dump → 用修好的分条器重排（不调 GLM）+ 差集删多余旧点
//	           + 工具内 bump kb 版本（AGMEM_MYSQL_DSN，store.BumpVersion 通道）。
//	REINDEX_ENTRY=1  entry 字段规范化（G15/G13）：按确定性 id/序号重排
//	           entry_index/entry_count 并删除高位残留点；DEDUP 删除重复点造成的 entry
//	           空洞按保留点实际数量连续重编号（只改 payload，不改点 id）；可选 SRC_FILE
//	           限定 item 子集。
//	DEDUP=1    全库 clean 点文本归一化去重复核/清理：同文本保留
//	           (item_id, entry_index, id) 最小者，其余按显式 id 删除（G04）。
//	SET_SOURCE=1  SRC_FILE=[{id,source_doc}] → 回填 payload.source_doc 并 bump 版本（G01）。
//
// 输入 dump 一律经 SRC_FILE（JSON）。RESPLIT 形态：[{"id","dir","task_id","text"}]，
// 只含受影响 item；dir 为 clean 镜像目录名 `<dir>-clean`。
//
// 注入条目 id 约定（G49）：以 SRC_FILE 直写 qdrant 的策展/迁移条目必须使用保留段
// item_id ≥ 900000；低于该值默认拒绝并报错，仅显式 SRC_ALLOW_REAL_IDS=1 才放行
// （真实条目误删恢复用，需人工确认；见 README「注入条目的 id 约定」）。
//
// 出口约定：Qdrant/aigateway 走公网域名，Mac 可跑；正式运行（非 DRY_RUN）另需
// AGMEM_MYSQL_DSN 直连 MySQL（版本 bump + 迁移执行锁），通常在 buildhost 执行。
//
// Usage (Mac / buildhost):
//
//	QDRANT_URL=https://qdrant-test.example.com QDRANT_API_KEY=... \
//	AIGATEWAY_URL=https://ai-gateway.example.com/v1 AIGATEWAY_TOKEN=sk-... \
//	COLLECTION=sre DRY_RUN=1 go run ./cmd/agmem-migrate
//
//	RESPLIT=1 SRC_FILE=/tmp/resplit.json DRY_RUN=1 ... go run ./cmd/agmem-migrate
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sre/agmem/internal/bm25"
	"sre/agmem/internal/cleaner"
	"sre/agmem/internal/embedding"
	"sre/agmem/internal/qdrant"
	"sre/agmem/internal/store"
)

// minInjectedItemID SRC_FILE 直写注入条目的保留段下界（G49，README 事故约定）。
const minInjectedItemID = 900000

// runLockName 迁移工具串行化执行锁名（MySQL 命名咨询锁）。
func runLockName(collection string) string { return "agmem:migrate:" + collection }

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		panic("missing env " + k)
	}
	return v
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		panic("invalid env " + k + "=" + v)
	}
	return n
}

type scrollResp struct {
	Result struct {
		Points []struct {
			ID      string         `json:"id"`
			Payload map[string]any `json:"payload"`
		} `json:"points"`
		NextPageOffset any `json:"next_page_offset"`
	} `json:"result"`
}

func rawQdrant(method, base, key, path string, body any) (scrollResp, error) {
	var out scrollResp
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", key)
	cl := &http.Client{Timeout: 60 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return out, fmt.Errorf("qdrant %s %s: %d %s", method, path, resp.StatusCode, b[:min(200, len(b))])
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func headInt64(in []int64, n int) []int64 {
	if len(in) > n {
		return in[:n]
	}
	return in
}

// rawSetPayload 按显式点 id 合并 payload（qdrant POST /points/payload，不覆盖其他键）。
func rawSetPayload(base, key, collection string, ids []string, payload map[string]any) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := rawQdrant("POST", base, key, "/collections/"+collection+"/points/payload",
		map[string]any{"payload": payload, "points": ids})
	return err
}

// rawDeletePoints 按显式点 id 删除（唯一删除通道；禁 filter 语义，见包注释）。
func rawDeletePoints(base, key, collection string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := rawQdrant("POST", base, key, "/collections/"+collection+"/points/delete",
		map[string]any{"points": ids})
	return err
}

func main() {
	dry := os.Getenv("DRY_RUN") == "1"
	collection := os.Getenv("COLLECTION")
	if collection == "" {
		collection = "sre"
	}
	base := mustEnv("QDRANT_URL")
	key := mustEnv("QDRANT_API_KEY")
	qc := qdrant.New(base, key, 60*time.Second)
	ctx := context.Background()

	// SPARSE=1 混合检索回填模式：scroll 全部 clean 点 → 每点 payload.text 计算
	// bm25 稀疏向量 → 按命名向量部分更新（不动稠密向量与 payload）。批量 100/次。
	// Mac 可跑（公网域名）；DRY_RUN=1 只统计不写。
	if os.Getenv("SPARSE") == "1" {
		runSparseBackfill(ctx, collection, base, key, qc, dry)
		return
	}

	// REINDEX_ENTRY=1 entry 字段规范化模式（G15/G13）：不调 GLM/embed，只按显式
	// id 改写 payload 与删除高位残留点。可选 SRC_FILE 限定 item 子集。
	if os.Getenv("REINDEX_ENTRY") == "1" {
		bump, release := writeModeBumpAndLock(ctx, collection, dry)
		if err := runReindexEntry(ctx, os.Getenv("SRC_FILE"), collection, base, key, dry, bump); err != nil {
			release()
			fmt.Fprintf(os.Stderr, "REINDEX_ENTRY: %v\n", err)
			os.Exit(1)
		}
		release()
		return
	}

	// DEDUP=1 全库重复 clean 文本去重复核/清理（G04）。
	if os.Getenv("DEDUP") == "1" {
		bump, release := writeModeBumpAndLock(ctx, collection, dry)
		if err := runDedup(ctx, collection, base, key, dry, bump); err != nil {
			release()
			fmt.Fprintf(os.Stderr, "DEDUP: %v\n", err)
			os.Exit(1)
		}
		release()
		return
	}

	// SET_SOURCE=1 来源文档回填模式（G01）：SRC_FILE=[{id,source_doc}]。
	if os.Getenv("SET_SOURCE") == "1" && os.Getenv("SRC_FILE") != "" {
		bump, release := writeModeBumpAndLock(ctx, collection, dry)
		if err := runSetSource(ctx, os.Getenv("SRC_FILE"), collection, base, key, dry, bump); err != nil {
			release()
			fmt.Fprintf(os.Stderr, "SET_SOURCE: %v\n", err)
			os.Exit(1)
		}
		release()
		return
	}

	// 嵌入客户端按需构造：REINDEX_ENTRY/DEDUP/SET_SOURCE 与 SRC_FILE 注入门禁不
	// 依赖 AIGATEWAY 凭据，缺失时不应因未用到的依赖 panic（G49 门禁须先于建连）。
	newEC := func() *embedding.Client {
		return embedding.New(mustEnv("AIGATEWAY_URL"), mustEnv("AIGATEWAY_TOKEN"), "", "", "", 32, 3)
	}

	// RESPLIT 存量重排模式（2026-09-09 纯元数据块修复批）：RESPLIT=1 + SRC_FILE
	// （task_items.clean_text 导出，只含受影响 item）→ 用修好的分条器重排 + 差集删
	// 多余旧点 + bump kb 版本，**不重新调用 GLM**。必须在下方通用 SRC_FILE 分支之前。
	if os.Getenv("RESPLIT") == "1" && os.Getenv("SRC_FILE") != "" {
		ec := newEC()
		deps := resplitDeps{
			Embed:        ec.EmbedBatch,
			Upsert:       qc.Upsert,
			Delete:       qc.DeleteByID,
			EnsureSparse: qc.EnsureSparseVector,
		}
		release := func() {}
		if !dry {
			// 版本 bump 走既有 store.BumpVersion 通道（与在线 worker 同源）；
			// DRY_RUN=1 不连 MySQL、不写库。缺 DSN 时 fail-fast——重排改变了条目
			// 文本却不 bump，agent 本地缓存会持续命中旧内容（UC-012 A5）。
			dsn := os.Getenv("AGMEM_MYSQL_DSN")
			if dsn == "" {
				panic("RESPLIT 正式运行需要 AGMEM_MYSQL_DSN 以 bump kb 版本（DRY_RUN=1 可免）")
			}
			bump, err := newBumpFunc(ctx, dsn, collection)
			if err != nil {
				panic(err)
			}
			deps.Bump = bump
			release = lockOrWarn(ctx, dsn, collection, "RESPLIT")
		}
		err := runResplit(ctx, os.Getenv("SRC_FILE"), collection, base, key, deps, dry)
		release()
		if err != nil {
			fmt.Fprintf(os.Stderr, "RESPLIT: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// RECLEAN 重清洗模式：RECLEAN=1 + SRC_FILE（originals）→ GLM v2 清洗重建。
	if os.Getenv("RECLEAN") == "1" && os.Getenv("SRC_FILE") != "" {
		glm := cleaner.NewGLMClient(
			os.Getenv("GLM_BASE_URL"), os.Getenv("GLM_MODEL"),
			os.Getenv("GLM_THINKING"), os.Getenv("GLM_TOKEN"))
		ec := newEC()
		deps := recleanDeps{
			Clean:        glm.Clean,
			Embed:        ec.EmbedBatch,
			Upsert:       qc.Upsert,
			Delete:       qc.DeleteByID,
			EnsureSparse: qc.EnsureSparseVector,
			Concurrency:  envInt("RECLEAN_CONCURRENCY", 4),
		}
		release := func() {}
		if !dry {
			dsn := os.Getenv("AGMEM_MYSQL_DSN")
			if dsn == "" {
				panic("RECLEAN 正式运行需要 AGMEM_MYSQL_DSN（版本 bump + 执行锁；DRY_RUN=1 可免）")
			}
			bump, err := newBumpFunc(ctx, dsn, collection)
			if err != nil {
				panic(err)
			}
			deps.Bump = bump
			release = lockOrWarn(ctx, dsn, collection, "RECLEAN")
		}
		err := runReclean(ctx, os.Getenv("SRC_FILE"), collection, base, key, deps, dry,
			os.Getenv("RECLEAN_FAILED_FILE"), os.Getenv("REQUIRE_FULL_DUMP") == "1")
		release()
		if err != nil {
			fmt.Fprintf(os.Stderr, "RECLEAN: %v\n", err)
			os.Exit(1)
		}
		return
	}
	// SRC_FILE 恢复模式：从 MySQL dump（[{id,dir,task_id,text}]）读取源文本，
	// 只 upsert 不删除（用于 delete-filter 误删事故后的重建，见 2026-09-07）。
	// 注入条目 id 强制保留段（G49）；恢复真实 item 需显式 SRC_ALLOW_REAL_IDS=1。
	if srcFile := os.Getenv("SRC_FILE"); srcFile != "" {
		items, err := readSrcFileItems(srcFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "SRC_FILE: %v\n", err)
			os.Exit(1)
		}
		ids := make([]int64, len(items))
		for i, it := range items {
			ids[i] = it.ID
		}
		if err := validateInjectedIDs(ids, os.Getenv("SRC_ALLOW_REAL_IDS") == "1"); err != nil {
			fmt.Fprintf(os.Stderr, "SRC_FILE: %v\n", err)
			os.Exit(1)
		}
		if err := runSrcFileItems(ctx, items, srcFile, collection, base, key, qc, newEC(), dry); err != nil {
			fmt.Fprintf(os.Stderr, "SRC_FILE: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 1. scroll all clean points; keep the longest point per item as source, and
	//    record every old point id (显式 id 差集删除的输入).
	olds := map[int64]*rewriteItem{}
	offset := any(nil)
	total := 0
	for {
		body := map[string]any{
			"limit": 256, "with_payload": true, "with_vector": false,
			"filter": map[string]any{"must": []map[string]any{
				{"key": "point_type", "match": map[string]any{"value": "clean"}}}},
		}
		if offset != nil {
			body["offset"] = offset
		}
		resp, err := rawQdrant("POST", base, key, "/collections/"+collection+"/points/scroll", body)
		if err != nil {
			panic(err)
		}
		for _, p := range resp.Result.Points {
			total++
			itemID := payloadInt64(p.Payload, "item_id")
			if itemID == 0 {
				continue
			}
			it := olds[itemID]
			if it == nil {
				it = &rewriteItem{ID: itemID}
				olds[itemID] = it
			}
			it.Count++
			it.OldIDs = append(it.OldIDs, p.ID)
			text, _ := p.Payload["text"].(string)
			if len([]rune(text)) > len([]rune(it.Text)) {
				it.Text = text
				it.Dir, _ = p.Payload["dir"].(string)
				it.TaskID = payloadInt64(p.Payload, "task_id")
			}
		}
		if resp.Result.NextPageOffset == nil {
			break
		}
		offset = resp.Result.NextPageOffset
	}
	fmt.Printf("scanned %d clean points, %d distinct items\n", total, len(olds))

	items := make([]rewriteItem, 0, len(olds))
	for _, it := range olds {
		items = append(items, *it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })

	deps := rewriteDeps{Embed: newEC().EmbedBatch, Upsert: qc.Upsert, Delete: qc.DeleteByID}
	if err := runDefaultRewrite(ctx, items, collection, deps, dry); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

// —— 默认长点位重建模式（G03：先 upsert 新点，再按显式 id 差集删除）——

// rewriteItem 默认迁移模式单 item 输入：源文本 + 该 item 现存全部 clean 点显式 id。
type rewriteItem struct {
	ID     int64
	Text   string
	Dir    string
	TaskID int64
	OldIDs []string
	Count  int
}

// rewriteDeps 默认迁移模式可注入依赖（生产注入真实 client，测试注入假实现）。
type rewriteDeps struct {
	Embed  func(ctx context.Context, texts []string) ([][]float32, error)
	Upsert func(ctx context.Context, collection string, points []qdrant.Point) error
	Delete func(ctx context.Context, collection string, ids []string) error
}

// runDefaultRewrite 默认迁移：每个 item 的源文本重新分条 → embed → 确定性 id
// upsert → 删除「旧显式 id − 新 id」差集。
//
// 顺序契约：先 upsert 后删差集，且差集恒不含新 id（staleIDsFromIDs 保证）——
// 中断重跑一致、任意时刻该 item 不空窗（2026-09-07 item_id filter 删除会把刚写
// 的新点一起删掉，造成永久零 clean 点）。
func runDefaultRewrite(ctx context.Context, items []rewriteItem, collection string, deps rewriteDeps, dry bool) error {
	rewritten, upserted, skipped, failed := 0, 0, 0, 0
	for n, it := range items {
		entries := cleaner.SplitEntries(it.Text, cleaner.MaxEntryLen)
		if len(entries) == 0 {
			entries = []string{it.Text}
		}
		// single short entry: keep as-is (already ≤600)
		if len(entries) == 1 && it.Count == 1 {
			skipped++
			continue
		}
		newIDs := make([]string, len(entries))
		for i := range entries {
			newIDs[i] = cleaner.EntryPointID(it.ID, "clean", i)
		}
		stale := staleIDsFromIDs(it.OldIDs, newIDs)
		if dry {
			fmt.Printf("item %d: old=%d new=%d stale=%d (dry)\n", it.ID, it.Count, len(entries), len(stale))
			rewritten++
			upserted += len(entries)
			continue
		}
		vecs, err := deps.Embed(ctx, entries)
		if err != nil {
			fmt.Printf("item %d: embed fail: %v\n", it.ID, err)
			failed++
			continue
		}
		if len(vecs) != len(entries) {
			fmt.Printf("item %d: vec/entry mismatch\n", it.ID)
			failed++
			continue
		}
		pts := make([]qdrant.Point, len(entries))
		for i, e := range entries {
			pts[i] = qdrant.Point{
				ID:     newIDs[i],
				Vector: vecs[i],
				Payload: qdrant.PointPayload{
					Dir: it.Dir, PointType: "clean", TaskID: it.TaskID,
					Text: e, ItemID: it.ID,
					EntryIndex: i, EntryCount: len(entries),
				},
			}
		}
		if err := deps.Upsert(ctx, collection, pts); err != nil {
			fmt.Printf("item %d: upsert fail: %v\n", it.ID, err)
			failed++
			continue
		}
		if len(stale) > 0 {
			if err := deps.Delete(ctx, collection, stale); err != nil {
				fmt.Printf("item %d: delete %d stale fail: %v\n", it.ID, len(stale), err)
				failed++
				continue
			}
		}
		upserted += len(entries)
		rewritten++
		if (n+1)%50 == 0 {
			fmt.Printf("progress %d/%d items (dry=%v)\n", n+1, len(items), dry)
		}
		time.Sleep(150 * time.Millisecond)
	}
	fmt.Printf("DONE: %d items rewritten -> %d new points (dry=%v); skipped(short)=%d failed=%d\n",
		rewritten, upserted, dry, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d items failed（旧点按未处理原样保留）", failed)
	}
	return nil
}

// staleIDsFromIDs 差集：旧显式 id 中不属于新 id 集合的部分（唯一删除输入）。
func staleIDsFromIDs(old []string, newIDs []string) []string {
	isNew := make(map[string]bool, len(newIDs))
	for _, id := range newIDs {
		isNew[id] = true
	}
	var stale []string
	for _, id := range old {
		if !isNew[id] {
			stale = append(stale, id)
		}
	}
	return stale
}

// srcFileItem SRC_FILE 恢复/注入模式输入条目（MySQL dump 或策展注入）。
type srcFileItem struct {
	ID     int64  `json:"id"`
	Dir    string `json:"dir"`
	TaskID int64  `json:"task_id"`
	Text   string `json:"text"`
}

// readSrcFileItems 读取并解析 SRC_FILE（JSON 数组）。
func readSrcFileItems(path string) ([]srcFileItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read SRC_FILE: %w", err)
	}
	var items []srcFileItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parse SRC_FILE: %w", err)
	}
	return items, nil
}

// runSrcFileItems 从已解析条目恢复 clean 点：SplitEntries → 逐条 embed → upsert
// （不删除任何既有点）。id 门禁由调用方在构造嵌入客户端之前先行校验（G49）。
func runSrcFileItems(ctx context.Context, items []srcFileItem, path, collection, base, key string,
	qc *qdrant.Client, ec *embedding.Client, dry bool) error {
	fmt.Printf("SRC_FILE mode: %d items from %s (dry=%v)\n", len(items), path, dry)
	upserted, rewritten := 0, 0
	for n, it := range items {
		entries := cleaner.SplitEntries(it.Text, cleaner.MaxEntryLen)
		if len(entries) == 0 {
			continue
		}
		vecs, err := ec.EmbedBatch(ctx, entries)
		if err != nil {
			fmt.Printf("item %d: embed fail: %v\n", it.ID, err)
			continue
		}
		if len(vecs) != len(entries) {
			fmt.Printf("item %d: vec/entry mismatch\n", it.ID)
			continue
		}
		pts := make([]qdrant.Point, len(entries))
		for i, e := range entries {
			pts[i] = qdrant.Point{
				ID:     cleaner.EntryPointID(it.ID, "clean", i),
				Vector: vecs[i],
				Payload: qdrant.PointPayload{
					Dir: it.Dir, PointType: "clean", TaskID: it.TaskID,
					Text: e, ItemID: it.ID,
					EntryIndex: i, EntryCount: len(entries),
				},
			}
		}
		if !dry {
			if err := qc.Upsert(ctx, collection, pts); err != nil {
				fmt.Printf("item %d: upsert fail: %v\n", it.ID, err)
				continue
			}
		}
		upserted += len(entries)
		rewritten++
		if (n+1)%50 == 0 {
			fmt.Printf("progress %d/%d items (dry=%v)\n", n+1, len(items), dry)
		}
		time.Sleep(150 * time.Millisecond)
	}
	fmt.Printf("SRC-FILE DONE: %d items -> %d points (dry=%v)\n", rewritten, upserted, dry)
	return nil
}

// validateInjectedIDs 注入条目 id 门禁（G49）：默认要求 ≥ minInjectedItemID，
// 否则列出违规 id 并报错；allowReal 时跳过（显式 SRC_ALLOW_REAL_IDS=1）。
func validateInjectedIDs(ids []int64, allowReal bool) error {
	if allowReal {
		fmt.Printf("WARNING: SRC_ALLOW_REAL_IDS=1 —— 跳过 item_id ≥ %d 注入门禁（真实条目恢复，需人工确认无撞车）\n", minInjectedItemID)
		return nil
	}
	var bad []int64
	for _, id := range ids {
		if id < minInjectedItemID {
			bad = append(bad, id)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i] < bad[j] })
	return fmt.Errorf("SRC_FILE 注入条目 item_id 必须 ≥ %d（保留段，见 README「注入条目的 id 约定」）；"+
		"拒绝 %d 个低位 id（前 10 个 %v）。恢复真实 item 请显式设 SRC_ALLOW_REAL_IDS=1",
		minInjectedItemID, len(bad), headInt64(bad, 10))
}

// —— 迁移工具串行化（G50：执行锁 / 明确串行窗口）——

// lockOrWarn 获取迁移执行锁（MySQL GET_LOCK，独立连接持有）；SKIP_RUN_LOCK=1
// 或 DSN 缺失时只打印串行窗口告警。返回 release（幂等，可重复调用）。
func lockOrWarn(ctx context.Context, dsn, collection, mode string) func() {
	if os.Getenv("SKIP_RUN_LOCK") == "1" {
		fmt.Printf("WARNING: SKIP_RUN_LOCK=1 —— %s 未取执行锁，必须人工保证串行窗口\n", mode)
		return func() {}
	}
	release, err := acquireRunLock(ctx, dsn, runLockName(collection))
	if err != nil {
		panic(fmt.Sprintf("%s 获取执行锁失败：%v（并发写同一确定性 id 会互相覆盖；确认无其他迁移进程后重试，或 SKIP_RUN_LOCK=1 自担风险）", mode, err))
	}
	fmt.Printf("WARNING: %s 持有迁移执行锁 %s，但在线 worker 不参与该锁——请在维护窗口串行执行（禁止与在线清洗并发写同一 item）\n",
		mode, runLockName(collection))
	return release
}

// acquireRunLock MySQL 命名咨询锁：GET_LOCK(name, 0) 立即返回；由独立 *sql.Conn
// 持有，RELEASE_LOCK 后关闭连接。同进程重复调用 release 安全。
func acquireRunLock(ctx context.Context, dsn, name string) (func(), error) {
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql for run lock: %w", err)
	}
	conn, err := st.DB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire lock conn: %w", err)
	}
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("get_lock %s: %w", name, err)
	}
	if !got.Valid || got.Int64 != 1 {
		_ = conn.Close()
		return nil, fmt.Errorf("执行锁 %q 已被其他迁移进程持有", name)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", name)
			_ = conn.Close()
		})
	}, nil
}

// writeModeBumpAndLock 纯 payload 写模式（REINDEX_ENTRY/DEDUP/SET_SOURCE）的
// bump 通道 + 执行锁：DRY_RUN 或 DSN 缺失时返回 nil bump 与空 release。
func writeModeBumpAndLock(ctx context.Context, collection string, dry bool) (func(context.Context) error, func()) {
	if dry {
		return nil, func() {}
	}
	dsn := os.Getenv("AGMEM_MYSQL_DSN")
	if dsn == "" {
		panic("正式运行需要 AGMEM_MYSQL_DSN（版本 bump + 执行锁；DRY_RUN=1 可免）")
	}
	bump, err := newBumpFunc(ctx, dsn, collection)
	if err != nil {
		panic(err)
	}
	return bump, lockOrWarn(ctx, dsn, collection, "migrate")
}

// —— RESPLIT 存量重排（2026-09-09 纯元数据块修复批）——

// resplitItem RESPLIT 输入条目（由主 agent 从 MySQL task_items 导出，只含受影响
// item）：text = task_items.clean_text，dir 为 clean 镜像目录名 `<dir>-clean`。
type resplitItem struct {
	ID     int64  `json:"id"`
	Dir    string `json:"dir"`
	TaskID int64  `json:"task_id"`
	Text   string `json:"text"`
}

// cleanPoint 存量 clean 点（id + payload，用于幂等比对与差集删除）。
type cleanPoint struct {
	ID         string
	Text       string
	ItemID     int64
	EntryIndex int
	EntryCount int
}

// resplitDeps 可注入依赖（生产由 main 注入真实 client，测试注入假实现）。
type resplitDeps struct {
	Embed  func(ctx context.Context, texts []string) ([][]float32, error)
	Upsert func(ctx context.Context, collection string, points []qdrant.Point) error
	Delete func(ctx context.Context, collection string, ids []string) error
	// EnsureSparse 幂等确保 collection 具备 bm25 命名稀疏向量（qdrant v1.18+
	// Create named vector）。在首次 upsert 前调用一次；幂等短路与 DRY_RUN 路径
	// 不触发（不写库）。nil = 跳过（测试可不注入）。
	EnsureSparse func(ctx context.Context, collection string) error
	// Bump 全部完成后递增 kb 版本；nil = 不 bump（DRY_RUN）。
	Bump func(ctx context.Context) error
}

// newBumpFunc 构造版本 bump 通道：store.Open（幂等迁移）→ GetKB(collection) →
// 返回 BumpVersion 闭包。kb 名 = collection 名（单库化约定，collection `sre`）。
func newBumpFunc(ctx context.Context, dsn, kbName string) (func(context.Context) error, error) {
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql for version bump: %w", err)
	}
	kb, err := st.GetKB(ctx, kbName)
	if err != nil {
		return nil, fmt.Errorf("resolve kb %q: %w", kbName, err)
	}
	return func(c context.Context) error {
		v, err := st.BumpVersion(c, kb.ID)
		if err != nil {
			return err
		}
		fmt.Printf("kb %s version bumped to %d\n", kbName, v)
		return nil
	}, nil
}

// runResplit RESPLIT=1 存量重排：修复存量「纯元数据块」——同一条目按修好的分条器
// （cleaner.SplitEntries，含 foldMetaOnlyEntries）重排，不重新调用 GLM。
//
// 流程（每个输入 item）：
//  1. SplitEntries(text, MaxEntryLen) → 新条目序列（确定性 point id = EntryPointID）；
//  2. 幂等短路：存量点集合与文本和新条目完全一致 → 跳过（零 embed/零写/零删除）；
//  3. 首次写入前 EnsureSparse 幂等补齐 bm25 命名稀疏向量；embed → 每新点用
//     bm25.SparseVector(entry) 随点写入稀疏向量（重排换了点文本，稀疏必须同批
//     重算，否则混合检索 sparse 分支仍指向旧文本）→ 先 upsert 新点 → 再删
//     「旧显式 id − 新 id」差集（**绝不用 item_id filter 删除**，2026-09-07 两次
//     误删事故根因；差集为空即零删除）；
//  4. 全部 item 完成后，若有任何变更 → deps.Bump 递增 kb 版本（缓存门禁）。
//
// DRY_RUN=1：只 scroll 读旧点、打印每个 item 的新旧点数与将删除数，不 embed、不写库、
// 不 EnsureSparse、不 bump。只处理 SRC_FILE 中的 item（不扫全库、不重跑 GLM）。
func runResplit(ctx context.Context, path, collection, base, key string, deps resplitDeps, dry bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read SRC_FILE: %w", err)
	}
	var items []resplitItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("parse SRC_FILE: %w", err)
	}
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		if it.ID == 0 {
			return fmt.Errorf("resplit: 输入含非法 item id 0")
		}
		if seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		ids = append(ids, it.ID)
	}
	fmt.Printf("RESPLIT mode: %d items (%d distinct) from %s (dry=%v)\n", len(items), len(ids), path, dry)

	oldPoints, err := collectCleanPoints(base, key, collection, ids)
	if err != nil {
		return fmt.Errorf("collect old clean points: %w", err)
	}

	changed, unchanged, failed := 0, 0, 0
	newPoints, staleDeleted := 0, 0
	sparseEnsured := false
	for n, it := range items {
		entries := cleaner.SplitEntries(it.Text, cleaner.MaxEntryLen)
		if len(entries) == 0 {
			fmt.Printf("item %d: split to empty, skip\n", it.ID)
			failed++
			continue
		}
		newIDs := make([]string, len(entries))
		for i := range entries {
			newIDs[i] = cleaner.EntryPointID(it.ID, "clean", i)
		}
		old := oldPoints[it.ID]
		if sameEntries(old, newIDs, entries) {
			unchanged++
			continue
		}
		stale := staleIDs(old, newIDs)
		if dry {
			fmt.Printf("item %d: old=%d new=%d stale=%d (dry)\n", it.ID, len(old), len(entries), len(stale))
			changed++
			newPoints += len(entries)
			continue
		}
		vecs, err := deps.Embed(ctx, entries)
		if err != nil {
			fmt.Printf("item %d: embed fail: %v\n", it.ID, err)
			failed++
			continue
		}
		if len(vecs) != len(entries) {
			fmt.Printf("item %d: vec/entry mismatch (%d/%d)\n", it.ID, len(vecs), len(entries))
			failed++
			continue
		}
		pts := make([]qdrant.Point, len(entries))
		for i, e := range entries {
			// 稀疏向量随点写入，与在线 worker 同源（bm25.SparseVector(payload.text)）。
			// 重排换掉了点文本，稀疏向量必须同批重算：只 upsert 稠密向量会让这批点
			// 的 sparse 分支继续命中旧文本编码（2026-09-09 线上静默失真缺陷）。
			idx, vals := bm25.SparseVector(e)
			pts[i] = qdrant.Point{
				ID:     newIDs[i],
				Vector: vecs[i],
				Sparse: map[string]qdrant.SparseVector{
					qdrant.SparseVectorName: {Indices: idx, Values: vals},
				},
				Payload: qdrant.PointPayload{
					Dir: it.Dir, PointType: "clean", TaskID: it.TaskID, KB: collection,
					Text: e, ItemID: it.ID, EntryIndex: i, EntryCount: len(entries),
				},
			}
		}
		// 首次写入前幂等补齐 bm25 命名稀疏向量（v1.18+ Create named vector）；
		// 懒触发：幂等短路（无变更）与 DRY_RUN 路径都不会走到这里，二次运行零触达。
		if !sparseEnsured {
			if deps.EnsureSparse != nil {
				if err := deps.EnsureSparse(ctx, collection); err != nil {
					fmt.Printf("item %d: ensure sparse vector fail: %v\n", it.ID, err)
					failed++
					continue
				}
			}
			sparseEnsured = true
		}
		// 顺序契约：先 upsert 后删差集（中断重跑一致、任意时刻该 item 不空窗）。
		if err := deps.Upsert(ctx, collection, pts); err != nil {
			fmt.Printf("item %d: upsert fail: %v\n", it.ID, err)
			failed++
			continue
		}
		if len(stale) > 0 {
			if err := deps.Delete(ctx, collection, stale); err != nil {
				fmt.Printf("item %d: delete %d stale fail: %v\n", it.ID, len(stale), err)
				failed++
				continue
			}
			staleDeleted += len(stale)
		}
		changed++
		newPoints += len(entries)
		if (n+1)%50 == 0 {
			fmt.Printf("progress %d/%d items (dry=%v)\n", n+1, len(items), dry)
		}
		time.Sleep(150 * time.Millisecond)
	}
	fmt.Printf("RESPLIT DONE: changed=%d unchanged=%d new_points=%d stale_deleted=%d failed=%d (dry=%v)\n",
		changed, unchanged, newPoints, staleDeleted, failed, dry)
	if dry {
		return nil
	}
	if changed > 0 && deps.Bump != nil {
		if err := deps.Bump(ctx); err != nil {
			return fmt.Errorf("bump kb version: %w", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d items failed（见上方日志；旧点按未处理原样保留）", failed)
	}
	return nil
}

// sameEntries 幂等短路判定：存量点集合与新条目（id→text）一一对应且文本一致。
// 一致则无需 embed/upsert/删除，二次运行零写入、零 bump。
func sameEntries(old []cleanPoint, newIDs, entries []string) bool {
	if len(old) != len(entries) {
		return false
	}
	byID := make(map[string]string, len(old))
	for _, p := range old {
		byID[p.ID] = p.Text
	}
	for i, e := range entries {
		if byID[newIDs[i]] != e {
			return false
		}
	}
	return true
}

// staleIDs 差集：旧点 id 中不属于新点集合的部分（set-difference 删除的输入）。
func staleIDs(old []cleanPoint, newIDs []string) []string {
	isNew := make(map[string]bool, len(newIDs))
	for _, id := range newIDs {
		isNew[id] = true
	}
	var stale []string
	for _, p := range old {
		if !isNew[p.ID] {
			stale = append(stale, p.ID)
		}
	}
	return stale
}

// collectCleanPoints 只取输入 item 的 clean 点（id+payload）：filter = item_id ∈ ids
// 且 point_type=clean。分块（128 id/请求）避免单请求过大；不扫全库。
func collectCleanPoints(base, key, collection string, itemIDs []int64) (map[int64][]cleanPoint, error) {
	out := map[int64][]cleanPoint{}
	const chunk = 128
	for start := 0; start < len(itemIDs); start += chunk {
		end := start + chunk
		if end > len(itemIDs) {
			end = len(itemIDs)
		}
		anyIDs := make([]any, 0, end-start)
		for _, id := range itemIDs[start:end] {
			anyIDs = append(anyIDs, id)
		}
		var offset any
		for {
			body := map[string]any{
				"limit": 256, "with_payload": true, "with_vector": false,
				"filter": map[string]any{"must": []map[string]any{
					{"key": "item_id", "match": map[string]any{"any": anyIDs}},
					{"key": "point_type", "match": map[string]any{"value": "clean"}},
				}},
			}
			if offset != nil {
				body["offset"] = offset
			}
			resp, err := rawQdrant("POST", base, key, "/collections/"+collection+"/points/scroll", body)
			if err != nil {
				return nil, err
			}
			appendCleanPoints(out, resp)
			if resp.Result.NextPageOffset == nil {
				break
			}
			offset = resp.Result.NextPageOffset
		}
	}
	return out, nil
}

// collectAllCleanPoints 全库 clean 点（id+payload）按 item 分组（REINDEX/DEDUP 用）。
func collectAllCleanPoints(base, key, collection string) (map[int64][]cleanPoint, error) {
	out := map[int64][]cleanPoint{}
	var offset any
	for {
		body := map[string]any{
			"limit": 256, "with_payload": true, "with_vector": false,
			"filter": map[string]any{"must": []map[string]any{
				{"key": "point_type", "match": map[string]any{"value": "clean"}}}},
		}
		if offset != nil {
			body["offset"] = offset
		}
		resp, err := rawQdrant("POST", base, key, "/collections/"+collection+"/points/scroll", body)
		if err != nil {
			return nil, err
		}
		appendCleanPoints(out, resp)
		if resp.Result.NextPageOffset == nil {
			break
		}
		offset = resp.Result.NextPageOffset
	}
	return out, nil
}

func appendCleanPoints(out map[int64][]cleanPoint, resp scrollResp) {
	for _, p := range resp.Result.Points {
		id := payloadInt64(p.Payload, "item_id")
		if id == 0 {
			continue
		}
		text, _ := p.Payload["text"].(string)
		out[id] = append(out[id], cleanPoint{
			ID: p.ID, Text: text, ItemID: id,
			EntryIndex: int(payloadInt64(p.Payload, "entry_index")),
			EntryCount: int(payloadInt64(p.Payload, "entry_count")),
		})
	}
}

// payloadInt64 读取 payload 中的整数（qdrant JSON 解码为 float64；个别路径 json.Number）。
func payloadInt64(p map[string]any, key string) int64 {
	switch v := p[key].(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// —— RECLEAN 重清洗（G04/G38/G39/G40/G50）——

// recleanItem RECLEAN 输入条目（originals dump）。
type recleanItem struct {
	ID     int64  `json:"id"`
	Dir    string `json:"dir"`
	TaskID int64  `json:"task_id"`
	Text   string `json:"text"`
}

// recleanDeps RECLEAN 可注入依赖（生产注入真实 client，测试注入假实现）。
type recleanDeps struct {
	Clean        func(ctx context.Context, text string) (string, error)
	Embed        func(ctx context.Context, texts []string) ([][]float32, error)
	Upsert       func(ctx context.Context, collection string, points []qdrant.Point) error
	Delete       func(ctx context.Context, collection string, ids []string) error
	EnsureSparse func(ctx context.Context, collection string) error
	Bump         func(ctx context.Context) error
	Concurrency  int
	Sleep        func(time.Duration) // 429 退避；nil = time.Sleep
}

// recleanFailure 失败清单条目（落盘，可单独重跑）。
type recleanFailure struct {
	ItemID int64  `json:"item_id"`
	Stage  string `json:"stage"`
	Error  string `json:"error"`
}

// runReclean GLM 重清洗模式：RECLEAN=1 + SRC_FILE（originals dump [{id,dir,task_id,text}]）。
// 每条原文 → GLM 清洗（CleanPrompt v2）→ cleanEntries 管线（分条 + 占位过滤 +
// 微型条目过滤，与 worker 落点形态一致）→ embed → upsert。
//
// 关键契约：
//   - 旧 clean 点删除一律「先预收集显式 id、再删（旧 id − 新 id）差集」，绝不用
//     item_id filter 删（会误删刚写的新点，2026-09-07 事故）；
//   - 判空条目（GLM 输出空串/被去重清空）也要删旧点：不落 clean 点且旧点清除；
//   - 幂等短路：条目集合与文本与存量完全一致 → 零 embed/零写/零删/零 bump；
//   - 差集删除失败计入 failed（不许静默）；
//   - dump 覆盖率双向校验（dump item ↔ 库内 distinct item_id），REQUIRE_FULL_DUMP=1
//     时「库内有 item 未被 dump 覆盖」直接拒绝；
//   - 失败清单落盘（RECLEAN_FAILED_FILE，缺省 <SRC_FILE>.failed.json）；
//   - failed>0 返回非 nil error（非零退出）；
//   - 全部完成后若有任何变更 → deps.Bump 递增 kb 版本。
//
// DRY_RUN=1：只读存量点 + 调 GLM 预览，打印每 item 处置（old/new/stale）与清洗后
// 文本，不 embed/不写库/不 bump。
func runReclean(ctx context.Context, path, collection, base, key string,
	deps recleanDeps, dry bool, failedPath string, requireFullDump bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read SRC_FILE: %w", err)
	}
	var items []recleanItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("parse SRC_FILE: %w", err)
	}
	seen := map[int64]bool{}
	uniq := make([]recleanItem, 0, len(items))
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		if it.ID <= 0 {
			return fmt.Errorf("RECLEAN: 输入含非法 item id %d", it.ID)
		}
		if seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		uniq = append(uniq, it)
		ids = append(ids, it.ID)
	}
	conc := deps.Concurrency
	if conc <= 0 {
		conc = 4
	}
	fmt.Printf("RECLEAN mode: %d originals (%d distinct) from %s concurrency=%d (dry=%v)\n",
		len(items), len(uniq), path, conc, dry)

	// 0) 预收集：每个 item 现存全部 clean 点（显式 id + 文本，供幂等比对与差集删除）。
	oldPts, err := collectAllCleanPoints(base, key, collection)
	if err != nil {
		return fmt.Errorf("pre-collect clean points: %w", err)
	}
	oldCount := 0
	for _, v := range oldPts {
		oldCount += len(v)
	}
	fmt.Printf("pre-collected %d clean points across %d items\n", oldCount, len(oldPts))

	// dump 完整性校验（G40）：与库内 distinct item_id 双向比对。
	var missingInDB, notInDump []int64
	for _, id := range ids {
		if len(oldPts[id]) == 0 {
			missingInDB = append(missingInDB, id)
		}
	}
	for id := range oldPts {
		if !seen[id] {
			notInDump = append(notInDump, id)
		}
	}
	sort.Slice(missingInDB, func(i, j int) bool { return missingInDB[i] < missingInDB[j] })
	sort.Slice(notInDump, func(i, j int) bool { return notInDump[i] < notInDump[j] })
	fmt.Printf("dump coverage: dump_items=%d db_items=%d missing_in_db=%d not_in_dump=%d\n",
		len(ids), len(oldPts), len(missingInDB), len(notInDump))
	if len(missingInDB) > 0 {
		fmt.Printf("WARN dump 中 %d 个 item 在库内无 clean 点（将从头重建）：%v\n", len(missingInDB), headInt64(missingInDB, 20))
	}
	if len(notInDump) > 0 {
		fmt.Printf("WARN 库内 %d 个 item 未被 dump 覆盖（不会被重清洗）：%v\n", len(notInDump), headInt64(notInDump, 20))
	}
	if requireFullDump && len(notInDump) > 0 {
		return fmt.Errorf("REQUIRE_FULL_DUMP=1：dump 未覆盖全部库内 item（not_in_dump=%d，前 20 个 %v）",
			len(notInDump), headInt64(notInDump, 20))
	}

	// 1) 并发 GLM 清洗池（串行 458 条需 15-30h；concurrency 路并发）。
	type res struct {
		it      recleanItem
		entries []string
		err     error
		stage   string
	}
	sleep := deps.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	sem := make(chan struct{}, conc)
	results := make(chan res, len(uniq))
	go func() {
		var wg sync.WaitGroup
		for _, it := range uniq {
			wg.Add(1)
			go func(it recleanItem) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				const maxAttempts = 4
				var cleanText string
				var err error
				for attempt := 1; attempt <= maxAttempts; attempt++ {
					cleanText, err = deps.Clean(ctx, it.Text)
					if err == nil {
						break
					}
					if strings.Contains(err.Error(), "429") && attempt < maxAttempts {
						wait := time.Duration(45*attempt) * time.Second
						fmt.Printf("item %d: glm 429 backoff %s (attempt %d/%d)\n", it.ID, wait, attempt, maxAttempts)
						sleep(wait)
						continue
					}
					break
				}
				if err != nil {
					results <- res{it: it, err: err, stage: "glm"}
					return
				}
				results <- res{it: it, entries: cleaner.CleanEntries(cleanText)}
			}(it)
		}
		wg.Wait()
		close(results)
	}()

	recleaned, cleared, unchanged, failed := 0, 0, 0, 0
	upserted, staleDeleted := 0, 0
	sparseEnsured := false
	var failures []recleanFailure
	for r := range results {
		it := r.it
		if r.err != nil {
			fmt.Printf("item %d: %s fail: %v\n", it.ID, r.stage, r.err)
			failures = append(failures, recleanFailure{ItemID: it.ID, Stage: r.stage, Error: r.err.Error()})
			failed++
			continue
		}
		entries := r.entries
		newIDs := make([]string, len(entries))
		for i := range entries {
			newIDs[i] = cleaner.EntryPointID(it.ID, "clean", i)
		}
		old := oldPts[it.ID]
		same := sameEntries(old, newIDs, entries)
		stale := staleIDs(old, newIDs)

		if dry {
			fmt.Printf("item %d: old=%d new=%d stale=%d same=%v (dry)\n", it.ID, len(old), len(entries), len(stale), same)
			for i, e := range entries {
				fmt.Printf("  item %d entry %d (%d 字): %s\n", it.ID, i, len([]rune(e)), e)
			}
			if len(entries) == 0 {
				fmt.Printf("  item %d: cleaned to empty → will delete %d old points\n", it.ID, len(old))
			}
			if !same {
				recleaned++
			}
			continue
		}
		if same {
			unchanged++
			continue
		}
		// 判空条目：不落 clean 点，但旧点必须清除（G04：原实现直接 continue 留下旧点）。
		if len(entries) == 0 {
			if len(stale) > 0 {
				if err := deps.Delete(ctx, collection, stale); err != nil {
					fmt.Printf("item %d: delete %d stale (cleaned empty) fail: %v\n", it.ID, len(stale), err)
					failures = append(failures, recleanFailure{ItemID: it.ID, Stage: "delete-empty", Error: err.Error()})
					failed++
					continue
				}
				staleDeleted += len(stale)
			}
			fmt.Printf("item %d: cleaned to empty → deleted %d old points\n", it.ID, len(stale))
			cleared++
			continue
		}
		vecs, err := deps.Embed(ctx, entries)
		if err != nil {
			fmt.Printf("item %d: embed fail: %v\n", it.ID, err)
			failures = append(failures, recleanFailure{ItemID: it.ID, Stage: "embed", Error: err.Error()})
			failed++
			continue
		}
		if len(vecs) != len(entries) {
			fmt.Printf("item %d: vec/entry mismatch\n", it.ID)
			failures = append(failures, recleanFailure{ItemID: it.ID, Stage: "embed", Error: "vec/entry mismatch"})
			failed++
			continue
		}
		pts := make([]qdrant.Point, len(entries))
		for i, e := range entries {
			idx, vals := bm25.SparseVector(e)
			pts[i] = qdrant.Point{
				ID:     newIDs[i],
				Vector: vecs[i],
				Sparse: map[string]qdrant.SparseVector{
					qdrant.SparseVectorName: {Indices: idx, Values: vals},
				},
				Payload: qdrant.PointPayload{
					Dir: it.Dir + "-clean", PointType: "clean", TaskID: it.TaskID,
					KB: collection, Text: e, ItemID: it.ID,
					EntryIndex: i, EntryCount: len(entries),
				},
			}
		}
		if !sparseEnsured {
			if deps.EnsureSparse != nil {
				if err := deps.EnsureSparse(ctx, collection); err != nil {
					fmt.Printf("item %d: ensure sparse vector fail: %v\n", it.ID, err)
					failures = append(failures, recleanFailure{ItemID: it.ID, Stage: "ensure-sparse", Error: err.Error()})
					failed++
					continue
				}
			}
			sparseEnsured = true
		}
		// 顺序契约：先 upsert 新点后删差集（中断重跑一致、任意时刻该 item 不空窗）。
		if err := deps.Upsert(ctx, collection, pts); err != nil {
			fmt.Printf("item %d: upsert fail: %v\n", it.ID, err)
			failures = append(failures, recleanFailure{ItemID: it.ID, Stage: "upsert", Error: err.Error()})
			failed++
			continue
		}
		if len(stale) > 0 {
			if err := deps.Delete(ctx, collection, stale); err != nil {
				// G40：差集删除失败必须计入 failed，否则终态谎报 failed=0。
				fmt.Printf("item %d: delete %d stale fail: %v\n", it.ID, len(stale), err)
				failures = append(failures, recleanFailure{ItemID: it.ID, Stage: "delete-stale", Error: err.Error()})
				failed++
				continue
			}
			staleDeleted += len(stale)
		}
		upserted += len(entries)
		recleaned++
		if recleaned%10 == 0 {
			fmt.Printf("progress recleaned=%d/%d (upserted=%d failed=%d dry=%v)\n", recleaned, len(uniq), upserted, failed, dry)
		}
	}

	changed := recleaned + cleared
	fmt.Printf("RECLEAN DONE: recleaned=%d cleared=%d unchanged=%d new_points=%d stale_deleted=%d failed=%d (dry=%v)\n",
		recleaned, cleared, unchanged, upserted, staleDeleted, failed, dry)

	if !dry && len(failures) > 0 {
		if failedPath == "" {
			failedPath = path + ".failed.json"
		}
		if b, err := json.MarshalIndent(failures, "", "  "); err != nil {
			fmt.Fprintf(os.Stderr, "marshal failed list: %v\n", err)
		} else if err := os.WriteFile(failedPath, append(b, '\n'), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "write failed list %s: %v\n", failedPath, err)
		} else {
			fmt.Printf("failed list written: %s (%d items)\n", failedPath, len(failures))
		}
	}
	if !dry && changed > 0 && deps.Bump != nil {
		if err := deps.Bump(ctx); err != nil {
			return fmt.Errorf("bump kb version: %w", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("RECLEAN %d items failed（失败 item 旧点按未处理原样保留；失败清单 %s）", failed, failedPath)
	}
	return nil
}

// —— REINDEX_ENTRY entry 字段规范化（G15/G13）——

// entryNormPoint 规范化输入点（id + 现存 entry_index/entry_count）。
type entryNormPoint struct {
	ID  string
	Idx int
	Cnt int
}

// planEntryNormalization 计算某 item 的规范化结果：最终 entry_count、需改写的 payload、
// 需删除的高位残留点显式 id，以及空洞数 holes。
//
// 规则：
//   - 规范值 n = 同 item 内多数派 entry_count（并列取较小值）；无任何正 entry_count 的
//     legacy item 取 n=len(pts)；
//   - idx ≥ n 的点为高位残留（re-clean 拆出条数变少后旧高位点未删，G15 的 487/647 形态），
//     残留判定与显式 id 删除通道保持不变；
//   - 保留点（idx < n）数量恰为 n 且序号不重复：只把 entry_count≠n 的点改写为 n
//     （legacy 单点缺字段 Cnt=0→n），entry_index 不动；
//   - 保留点数量 k ≠ n（空洞：DEDUP 删重复文本点后 idx 出现缺口，如线上 item 530/564/692
//     的 idx=[1,2,4,5]）或序号重复：按 (idx,id) 升序把保留点连续重编号为 0..k-1 并令
//     entry_count=k，不再报错（原实现对空洞直接报错，DEDUP 后 REINDEX 无法收敛）。
//     holes = n−k（>0 即缺失的规范槽位数）供调用方打印与计数；k>n 时 holes=0。
//
// 取舍（G15 边界）：重编号只改写 payload.entry_index/entry_count，**不改点 id**。legacy
// 条目的点 id 本就非确定性（不由 cleaner.EntryPointID 派生），id 与 entry_index 之间没有
// 需要同步维护的不变量；确定性 id 条目（id 编码了 entry_index）不应出现空洞——DEDUP 只删
// 重复文本点、正常路径下确定性条目无重复，若真出现空洞说明 id 序列本身已损坏，仅改 payload
// 会让 id 与 entry_index 不一致，须走重清洗/重建而非本模式。
//
// 变异自检：删除下方 `dup || k != n` 重编号分支（或去掉 holes 计算），空洞用例
// （TestPlanEntryNormalization「空洞重编号」与 TestRunReindexEntry_RenumbersHoles）应失败。
func planEntryNormalization(pts []entryNormPoint) (int, map[string][2]int, []string, int) {
	if len(pts) == 0 {
		return 0, nil, nil, 0
	}
	freq := map[int]int{}
	for _, p := range pts {
		if p.Cnt > 0 {
			freq[p.Cnt]++
		}
	}
	n := 0
	if len(freq) == 0 {
		n = len(pts)
	} else {
		best := -1
		for c, f := range freq {
			if f > best || (f == best && c < n) {
				best, n = f, c
			}
		}
	}
	var keep []entryNormPoint
	var residue []string
	for _, p := range pts {
		if p.Idx >= n {
			residue = append(residue, p.ID)
		} else {
			keep = append(keep, p)
		}
	}
	sort.Strings(residue)
	k := len(keep)
	holes := 0
	if k < n {
		holes = n - k
	}
	dup := false
	seen := map[int]bool{}
	for _, p := range keep {
		if seen[p.Idx] {
			dup = true
			break
		}
		seen[p.Idx] = true
	}
	updates := map[string][2]int{}
	if dup || k != n {
		sort.Slice(keep, func(i, j int) bool {
			if keep[i].Idx != keep[j].Idx {
				return keep[i].Idx < keep[j].Idx
			}
			return keep[i].ID < keep[j].ID
		})
		for i, p := range keep {
			updates[p.ID] = [2]int{i, k}
		}
		return k, updates, residue, holes
	}
	for _, p := range keep {
		if p.Cnt != n {
			updates[p.ID] = [2]int{p.Idx, n}
		}
	}
	return n, updates, residue, 0
}

// runReindexEntry REINDEX_ENTRY=1：全库（或 SRC_FILE 限定的 item 子集）clean 点
// entry 字段规范化——按确定性 id/序号重排 entry_index/entry_count 并删除高位残留点。
// 不改文本、不重算向量、不调 GLM/embed。DRY_RUN=1 只打印处置。
//
// 空洞容错：DEDUP 删除重复文本点后 idx 出现缺口（保留点数量 ≠ canonical entry_count）
// 时不再报错，而是按保留点实际数量 k 连续重编号 entry_index=0..k-1、entry_count=k，
// 打印 `item X: renumbered N points (holes=…)`（dry-run 同样输出）并计入终态
// renumbered_items。重编号只改 payload，不改点 id（见 planEntryNormalization 取舍）。
func runReindexEntry(ctx context.Context, path, collection, base, key string, dry bool,
	bump func(context.Context) error) error {
	var only map[int64]bool
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read SRC_FILE: %w", err)
		}
		var items []recleanItem
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("parse SRC_FILE: %w", err)
		}
		only = map[int64]bool{}
		for _, it := range items {
			if it.ID > 0 {
				only[it.ID] = true
			}
		}
	}
	all, err := collectAllCleanPoints(base, key, collection)
	if err != nil {
		return fmt.Errorf("collect clean points: %w", err)
	}
	ids := make([]int64, 0, len(all))
	for id := range all {
		if only != nil && !only[id] {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	fmt.Printf("REINDEX_ENTRY mode: %d items (from %d in collection) dry=%v\n", len(ids), len(all), dry)

	normalized, updated, deleted, renumbered, unchanged, failed := 0, 0, 0, 0, 0, 0
	legacyIDs := 0
	for _, id := range ids {
		pts := all[id]
		norm := make([]entryNormPoint, len(pts))
		for i, p := range pts {
			norm[i] = entryNormPoint{ID: p.ID, Idx: p.EntryIndex, Cnt: p.EntryCount}
			// 按确定性 id 核对序号（G15「按确定性 id 重排」）：legacy item 级 id 不
			// 匹配 entry 派生 id，只统计不重写（重写需重算向量，超出本模式范围）。
			if p.ID != cleaner.EntryPointID(id, "clean", p.EntryIndex) {
				legacyIDs++
			}
		}
		n, updates, residue, holes := planEntryNormalization(norm)
		if len(updates) == 0 && len(residue) == 0 {
			unchanged++
			continue
		}
		if dry {
			if holes > 0 {
				fmt.Printf("item %d: renumbered %d points (holes=%d)\n", id, n, holes)
				renumbered++
			}
			fmt.Printf("item %d: points=%d entry_count=%d payload_updates=%d residue=%d (dry)\n",
				id, len(pts), n, len(updates), len(residue))
			normalized++
			updated += len(updates)
			deleted += len(residue)
			continue
		}
		ok := true
		for pid, u := range updates {
			if err := rawSetPayload(base, key, collection, []string{pid},
				map[string]any{"entry_index": u[0], "entry_count": u[1]}); err != nil {
				fmt.Printf("item %d point %s: set payload fail: %v\n", id, pid, err)
				failed++
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if len(residue) > 0 {
			if err := rawDeletePoints(base, key, collection, residue); err != nil {
				fmt.Printf("item %d: delete %d residue fail: %v\n", id, len(residue), err)
				failed++
				continue
			}
		}
		if holes > 0 {
			fmt.Printf("item %d: renumbered %d points (holes=%d)\n", id, n, holes)
			renumbered++
		}
		fmt.Printf("item %d: points=%d entry_count=%d payload_updates=%d residue_deleted=%d\n",
			id, len(pts), n, len(updates), len(residue))
		normalized++
		updated += len(updates)
		deleted += len(residue)
	}
	fmt.Printf("REINDEX_ENTRY DONE: items=%d payload_updates=%d residue_deleted=%d renumbered_items=%d unchanged=%d legacy_ids=%d failed=%d (dry=%v)\n",
		normalized, updated, deleted, renumbered, unchanged, legacyIDs, failed, dry)
	if !dry && (updated+deleted) > 0 && bump != nil {
		if err := bump(ctx); err != nil {
			return fmt.Errorf("bump kb version: %w", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("REINDEX_ENTRY %d items failed", failed)
	}
	return nil
}

// —— DEDUP 全库重复 clean 文本去重复核/清理（G04）——

// normalizeDedupText 去空白归一化（比对用，不改写库内文本）。
func normalizeDedupText(s string) string { return strings.Join(strings.Fields(s), " ") }

// runDedup DEDUP=1：全库 clean 点按归一化文本分组，同文本保留
// (item_id, entry_index, point id) 最小者，其余按显式 id 删除。
// DRY_RUN=1 只打印分组处置。删除会让被删 item 出现 entry 空洞，需随后跑
// REINDEX_ENTRY / 重清洗收敛（本模式打印受影响 item）。
func runDedup(ctx context.Context, collection, base, key string, dry bool,
	bump func(context.Context) error) error {
	all, err := collectAllCleanPoints(base, key, collection)
	if err != nil {
		return fmt.Errorf("collect clean points: %w", err)
	}
	groups := map[string][]cleanPoint{}
	for _, pts := range all {
		for _, p := range pts {
			k := normalizeDedupText(p.Text)
			if k == "" {
				continue
			}
			groups[k] = append(groups[k], p)
		}
	}
	dupGroups, dropped := 0, 0
	affected := map[int64]bool{}
	var toDelete []string
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		dupGroups++
		sort.Slice(g, func(i, j int) bool {
			if g[i].ItemID != g[j].ItemID {
				return g[i].ItemID < g[j].ItemID
			}
			if g[i].EntryIndex != g[j].EntryIndex {
				return g[i].EntryIndex < g[j].EntryIndex
			}
			return g[i].ID < g[j].ID
		})
		keep := g[0]
		var drops []string
		for _, p := range g[1:] {
			drops = append(drops, fmt.Sprintf("item=%d idx=%d id=%s", p.ItemID, p.EntryIndex, p.ID))
			toDelete = append(toDelete, p.ID)
			affected[p.ItemID] = true
			dropped++
		}
		fmt.Printf("DEDUP group (%d points, text=%q): keep item=%d idx=%d id=%s; drop %v\n",
			len(g), truncRunes(keep.Text, 60), keep.ItemID, keep.EntryIndex, keep.ID, drops)
	}
	aff := make([]int64, 0, len(affected))
	for id := range affected {
		aff = append(aff, id)
	}
	sort.Slice(aff, func(i, j int) bool { return aff[i] < aff[j] })
	fmt.Printf("DEDUP scan: clean_points=%d items=%d duplicate_groups=%d points_to_drop=%d affected_items=%d (dry=%v)\n",
		countPoints(all), len(all), dupGroups, dropped, len(aff), dry)
	if dropped > 0 {
		fmt.Printf("WARN 删除会让受影响 item 出现 entry 空洞，随后需跑 REINDEX_ENTRY=1 或 RECLEAN 收敛：%v\n", headInt64(aff, 20))
	}
	if dry || dropped == 0 {
		return nil
	}
	if err := rawDeletePoints(base, key, collection, toDelete); err != nil {
		return fmt.Errorf("delete %d duplicate points: %w", len(toDelete), err)
	}
	fmt.Printf("DEDUP DONE: deleted %d duplicate points\n", len(toDelete))
	if bump != nil {
		if err := bump(ctx); err != nil {
			return fmt.Errorf("bump kb version: %w", err)
		}
	}
	return nil
}

func countPoints(m map[int64][]cleanPoint) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// —— SET_SOURCE payload.source_doc 回填（G01）——

// sourceItem SET_SOURCE 输入条目：item_id → 入库文档名。
type sourceItem struct {
	ID        int64  `json:"id"`
	SourceDoc string `json:"source_doc"`
}

// runSetSource SET_SOURCE=1：SRC_FILE=[{id,source_doc}] → 按 item_id 查 clean 点，
// 合并写入 payload.source_doc（不覆盖其他 payload 键），全部完成后 bump kb 版本。
// DRY_RUN=1 只统计。source_doc 为空或 id 非法直接报错（不静默跳过）。
func runSetSource(ctx context.Context, path, collection, base, key string, dry bool,
	bump func(context.Context) error) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read SRC_FILE: %w", err)
	}
	var items []sourceItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("parse SRC_FILE: %w", err)
	}
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(items))
	docOf := map[int64]string{}
	for _, it := range items {
		if it.ID <= 0 {
			return fmt.Errorf("SET_SOURCE: 输入含非法 item id %d", it.ID)
		}
		doc := strings.TrimSpace(it.SourceDoc)
		if doc == "" {
			return fmt.Errorf("SET_SOURCE: item %d 的 source_doc 为空", it.ID)
		}
		if seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		ids = append(ids, it.ID)
		docOf[it.ID] = doc
	}
	fmt.Printf("SET_SOURCE mode: %d items (%d distinct) from %s (dry=%v)\n", len(items), len(ids), path, dry)

	ptsByItem, err := collectCleanPoints(base, key, collection, ids)
	if err != nil {
		return fmt.Errorf("collect clean points: %w", err)
	}
	updatedItems, updatedPoints, missing, failed := 0, 0, 0, 0
	for _, id := range ids {
		pts := ptsByItem[id]
		if len(pts) == 0 {
			fmt.Printf("item %d: no clean point (skip)\n", id)
			missing++
			continue
		}
		if dry {
			fmt.Printf("item %d: %d points ← source_doc=%q (dry)\n", id, len(pts), docOf[id])
			updatedItems++
			updatedPoints += len(pts)
			continue
		}
		pointIDs := make([]string, len(pts))
		for i, p := range pts {
			pointIDs[i] = p.ID
		}
		if err := rawSetPayload(base, key, collection, pointIDs, map[string]any{"source_doc": docOf[id]}); err != nil {
			fmt.Printf("item %d: set source_doc fail: %v\n", id, err)
			failed++
			continue
		}
		updatedItems++
		updatedPoints += len(pts)
	}
	fmt.Printf("SET_SOURCE DONE: items=%d points=%d missing_items=%d failed=%d (dry=%v)\n",
		updatedItems, updatedPoints, missing, failed, dry)
	if !dry && updatedPoints > 0 && bump != nil {
		if err := bump(ctx); err != nil {
			return fmt.Errorf("bump kb version: %w", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("SET_SOURCE %d items failed", failed)
	}
	return nil
}

// runSparseBackfill SPARSE=1 混合检索回填：为存量 clean 点补 bm25 稀疏向量。
//
// 流程：EnsureSparseVector 幂等补 collection 的 bm25 配置（qdrant v1.18+
// Create named vector）→ scroll 全部 point_type=clean 点（只取 id+payload.text，
// 不拉向量）→ 每点 bm25.SparseVector(payload.text) → UpdateSparseVectors 按
// 命名向量部分更新（qdrant "Update vectors"：keep unspecified vectors intact，
// 稠密向量与 payload 原样）。批量 100/次（客户端内置分批），批间 100ms 限速。
//
// 上线顺序：先跑本回填，再放开 AGMEM_HYBRID（查询路径对未配置 bm25 的
// collection 会 4xx）；DRY_RUN=1 只统计不写，供回填前核对规模。
// 注意：worker 在 SparseEnabled 开启后新写的 clean 点自带 bm25，本工具只需
// 跑一次；之后新增条目无需重跑。
func runSparseBackfill(ctx context.Context, collection, base, key string, qc *qdrant.Client, dry bool) {
	if err := qc.EnsureSparseVector(ctx, collection); err != nil {
		panic(fmt.Sprintf("ensure bm25 sparse config: %v（先修 collection 配置再回填）", err))
	}
	fmt.Printf("bm25 sparse config ensured on collection %s\n", collection)

	type pt struct {
		id   string
		text string
	}
	var points []pt
	offset := any(nil)
	total := 0
	for {
		body := map[string]any{
			"limit": 256, "with_payload": true, "with_vector": false,
			"filter": map[string]any{"must": []map[string]any{
				{"key": "point_type", "match": map[string]any{"value": "clean"}}}},
		}
		if offset != nil {
			body["offset"] = offset
		}
		resp, err := rawQdrant("POST", base, key, "/collections/"+collection+"/points/scroll", body)
		if err != nil {
			panic(err)
		}
		for _, p := range resp.Result.Points {
			total++
			text, _ := p.Payload["text"].(string)
			if text == "" {
				continue
			}
			points = append(points, pt{id: p.ID, text: text})
		}
		if resp.Result.NextPageOffset == nil {
			break
		}
		offset = resp.Result.NextPageOffset
	}
	fmt.Printf("scanned %d clean points, %d with text to backfill (dry=%v)\n", total, len(points), dry)

	var updates []qdrant.SparseUpdate
	done, skipped := 0, 0
	for _, p := range points {
		indices, values := bm25.SparseVector(p.text)
		if len(indices) == 0 {
			skipped++ // 全标点/空文本：无可编码词元
			continue
		}
		updates = append(updates, qdrant.SparseUpdate{
			ID:  p.id,
			Vec: qdrant.SparseVector{Indices: indices, Values: values},
		})
		if len(updates) == 100 {
			if !dry {
				if err := qc.UpdateSparseVectors(ctx, collection, updates); err != nil {
					panic(err)
				}
			}
			done += len(updates)
			updates = updates[:0]
			fmt.Printf("progress %d/%d points (dry=%v)\n", done, len(points), dry)
			time.Sleep(100 * time.Millisecond)
		}
	}
	if len(updates) > 0 && !dry {
		if err := qc.UpdateSparseVectors(ctx, collection, updates); err != nil {
			panic(err)
		}
	}
	done += len(updates)
	fmt.Printf("SPARSE DONE: %d points updated, %d skipped (no terms) (dry=%v)\n", done, skipped, dry)
}
