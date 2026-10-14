package store

// knowledge.go：清洗不变前缀的数据源（S1）——把「已清洗入库的知识条目」连同其来源
// 目录名/文档名读出，供 cleaner.PrefixBuilder 构造 L1/L2 前缀（见 docs/prefix-caching.md）。
//
// 另含 SourceNameForItem 与 KnowledgeEntries 共用的「task 内序号 ↔ documents 行」
// 不变式实现（sourceNamesForItems），避免两处各写一套映射。

import (
	"context"
	"fmt"
	"strings"
)

// KnowledgeEntryRow 清洗前缀用的知识条目行（已清洗入库的条目 + 其来源文档名）。
type KnowledgeEntryRow struct {
	ItemID  int64
	Dir     string // 目录名（directories.name，形如 k8s-clean）
	DocName string // documents.source_name
	Text    string // task_items.clean_text
}

// KnowledgeEntries 返回某 kb 全部已清洗条目，按 item id 升序（= 产出时间序）。
//
// 只取 status='cleaned' 且 clean_text 非空的条目：清洗失败/占位条目（clean_text 为
// 空）不构成背景知识，纳入只会污染前缀、打乱 append-only 语义。
func (s *Store) KnowledgeEntries(ctx context.Context, kbID int64) ([]KnowledgeEntryRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT i.id, COALESCE(d.name, ''), i.clean_text
		FROM task_items i
		LEFT JOIN directories d ON d.id = i.dir_id
		WHERE i.kb_id=? AND i.status=?
		  AND i.clean_text IS NOT NULL AND i.clean_text<>''
		ORDER BY i.id`, kbID, StatusCleaned)
	if err != nil {
		return nil, fmt.Errorf("store: knowledge entries kb %d: %w", kbID, err)
	}
	defer rows.Close()

	var out []KnowledgeEntryRow
	var ids []int64
	for rows.Next() {
		var r KnowledgeEntryRow
		if err := rows.Scan(&r.ItemID, &r.Dir, &r.Text); err != nil {
			return nil, fmt.Errorf("store: knowledge entries kb %d: %w", kbID, err)
		}
		out = append(out, r)
		ids = append(ids, r.ItemID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: knowledge entries kb %d: %w", kbID, err)
	}
	names, err := s.sourceNamesForItems(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].DocName = names[out[i].ItemID]
	}
	return out, nil
}

// sourceNameChunk 批量解析文档名时的分片大小：每片查询里 item id 占位符出现 3 次
// （窗口过滤各两次 + 输出过滤一次），1000 * 3 远低于 MySQL 的 65535 占位符上限。
const sourceNameChunk = 1000

// sourceNamesForItems 批量解析 item id → documents.source_name（缺失/无对应行 → 空串）。
//
// 映射依据（现有 schema 无 item↔document 外键）：CreateTaskWithItems 在同一事务、
// 同一循环内逐条插入 task_items 与 documents，重复文本在两侧同步跳过，因此
// 「task 内按 id 升序的第 N 条 task_item」严格对应「task 内按 id 升序的第 N 条
// documents」。本函数用 ROW_NUMBER() OVER (PARTITION BY task_id ORDER BY id) 表达
// 该不变式并做批量关联；SourceNameForItem（单条）与 KnowledgeEntries（整库）共用
// 它，是本不变式的唯一实现。
//
// 关键点：窗口在**整个 task**（不限于传入的 item 子集）上计算，故分片不影响序号；
// 只有输出按传入的 item id 过滤。若后续 documents 增 item_id 列，应改为直接按
// item_id 关联（G01）。
func (s *Store) sourceNamesForItems(ctx context.Context, itemIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(itemIDs))
	if len(itemIDs) == 0 {
		return out, nil
	}
	const query = `
		SELECT ti.item_id, COALESCE(d.source_name, '')
		FROM (
			SELECT i.id AS item_id, i.task_id,
			       ROW_NUMBER() OVER (PARTITION BY i.task_id ORDER BY i.id) AS rn
			FROM task_items i
			WHERE i.task_id IN (SELECT task_id FROM task_items WHERE id IN (%[1]s))
		) ti
		LEFT JOIN (
			SELECT doc.task_id, doc.source_name,
			       ROW_NUMBER() OVER (PARTITION BY doc.task_id ORDER BY doc.id) AS rn
			FROM documents doc
			WHERE doc.task_id IN (SELECT task_id FROM task_items WHERE id IN (%[1]s))
		) d ON d.task_id = ti.task_id AND d.rn = ti.rn
		WHERE ti.item_id IN (%[1]s)`
	for start := 0; start < len(itemIDs); start += sourceNameChunk {
		end := start + sourceNameChunk
		if end > len(itemIDs) {
			end = len(itemIDs)
		}
		part := itemIDs[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
		args := make([]any, 0, len(part)*3)
		for i := 0; i < 3; i++ {
			for _, id := range part {
				args = append(args, id)
			}
		}
		rows, err := s.DB.QueryContext(ctx, fmt.Sprintf(query, placeholders), args...)
		if err != nil {
			return nil, fmt.Errorf("store: source names: %w", err)
		}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: source names: %w", err)
			}
			out[id] = name
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: source names: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// SourceNameForItem 返回条目对应的来源文档名（documents.source_name，供 cleaner
// 写「来源：」行 / payload.source_doc 使用）。条目不存在或无对应 documents 行时
// 返回空串且不报错（调用方按空串降级，不得因此失败整条清洗）。
//
// 映射不变式与批量实现见 sourceNamesForItems（本函数是它的单条薄封装）。
func (s *Store) SourceNameForItem(ctx context.Context, itemID int64) (string, error) {
	names, err := s.sourceNamesForItems(ctx, []int64{itemID})
	if err != nil {
		return "", err
	}
	return names[itemID], nil
}
