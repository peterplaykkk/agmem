// state.go：wiki 同步状态表（agmem MySQL，本包幂等建表）。
//
// 为什么不放进 internal/store 的 migrations：wiki 同步是 agmemserv 进程内的一个
// 可选后台任务，state 表生命周期与本任务同域；放本包自管（CREATE TABLE IF NOT
// EXISTS）可避免与既有迁移（001/002/003）耦合——同步功能回退/删除时不必动
// store 迁移，也不改 store 包的任何既有语义。
//
// SQL 保持 MySQL 与 sqlite 双兼容（VARCHAR/BIGINT/TEXT/DATETIME，无 ENGINE/字符集
// 子句、无 ON DUPLICATE KEY），使单测可用进程内 sqlite 覆盖 state 往返而不依赖 MySQL。
package wikisync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// createStateTableSQL 状态表 DDL（幂等）。updated_at 存 unix 秒（BIGINT），
// 避开 MySQL TIMESTAMP 隐式默认与 sqlite 时间类型的可移植性坑。
//
// node_token / parent_node_token / depth / node_url 为 2026-09-10 新增的树结构列：
// 保存该文档在 wiki 树中的节点标识、父节点标识、深度（根 = 0）与可点击的 wiki 链接，
// 供清洗前缀的 L2 父目录上溯（StateStore.TreeNodeMap）与状态端点核对。保留既有平铺
// 的 dir 列（= 父节点标题归一后的目录名），不改变「按 obj_edit_time 判变更」的语义。
const createStateTableSQL = `CREATE TABLE IF NOT EXISTS wiki_sync_state (
  doc_token         VARCHAR(190) NOT NULL,
  title             VARCHAR(255) NOT NULL DEFAULT '',
  dir               VARCHAR(255) NOT NULL DEFAULT '',
  obj_edit_time     BIGINT NOT NULL DEFAULT 0,
  task_id           BIGINT NOT NULL DEFAULT 0,
  item_ids          TEXT,
  updated_at        BIGINT NOT NULL,
  node_token        VARCHAR(190) NOT NULL DEFAULT '',
  parent_node_token VARCHAR(190) NOT NULL DEFAULT '',
  depth             INT NOT NULL DEFAULT 0,
  node_url          VARCHAR(512) NOT NULL DEFAULT '',
  PRIMARY KEY (doc_token)
)`

// ensureColumnSQL 给**已存在**的表补新增列（幂等迁移）。MySQL 8 的
// `ALTER TABLE ... ADD COLUMN` 不支持 `IF NOT EXISTS`，而 `information_schema` 查询在
// sqlite（单测）上不可用——故采用「直接 ADD COLUMN，遇 duplicate column 错误即忽略」：
// 该错误在 MySQL（1060 "Duplicate column name"）与 sqlite（"duplicate column name"）
// 的消息里都含 "duplicate column"，用子串匹配统一识别，保持双库可测。
// 已有行的新列取各自 DEFAULT（空串 / 0），下次全树扫描会回填真值。
var ensureColumnSQL = []string{
	`ALTER TABLE wiki_sync_state ADD COLUMN node_token VARCHAR(190) NOT NULL DEFAULT ''`,
	`ALTER TABLE wiki_sync_state ADD COLUMN parent_node_token VARCHAR(190) NOT NULL DEFAULT ''`,
	`ALTER TABLE wiki_sync_state ADD COLUMN depth INT NOT NULL DEFAULT 0`,
	`ALTER TABLE wiki_sync_state ADD COLUMN node_url VARCHAR(512) NOT NULL DEFAULT ''`,
}

// cleanDirSuffix 清洗镜像目录名后缀（口径来源：cleaner.cleanDirName 的 `<dir>-clean`）。
// TreeNodeMap 的 key/value 都用该口径，便于直接喂给 cleaner.PrefixBuilder.ParentDirFunc。
const cleanDirSuffix = "-clean"

// State 单文档同步状态：doc_token（= 飞书 obj_token，文档稳定身份，跨移动不变）。
//
// NodeToken/ParentNodeToken/Depth/NodeURL 为 2026-09-10 新增的树结构字段（见上 DDL 注释）。
type State struct {
	DocToken    string  `json:"doc_token"`
	Title       string  `json:"title"`
	Dir         string  `json:"dir"`
	ObjEditTime int64   `json:"obj_edit_time"`
	TaskID      int64   `json:"task_id"`
	ItemIDs     []int64 `json:"item_ids"`
	UpdatedAt   int64   `json:"updated_at"`
	// NodeToken 该文档在 wiki 树中的 node token（URL 组成部分；obj_token 是文档 id）。
	NodeToken string `json:"node_token"`
	// ParentNodeToken 父节点的 node token（顶层节点的父为空间根，记空串）。
	ParentNodeToken string `json:"parent_node_token"`
	// Depth 该节点深度：根层节点 = 0，其子 = 1，依此类推。
	Depth int `json:"depth"`
	// NodeURL 该节点的 wiki 链接：https://<tenant>.feishu.cn/wiki/<node_token>。
	NodeURL string `json:"node_url"`
}

// StateStore wiki_sync_state 表存取。DB 复用 agmem 既有 *sql.DB（store.Store.DB）。
type StateStore struct {
	DB  *sql.DB
	Now func() time.Time
}

func (s *StateStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// EnsureSchema 幂等建表 + 幂等补列（进程启动时调用一次）。
func (s *StateStore) EnsureSchema(ctx context.Context) error {
	if s == nil || s.DB == nil {
		return fmt.Errorf("wikisync: state DB 未配置")
	}
	if _, err := s.DB.ExecContext(ctx, createStateTableSQL); err != nil {
		return fmt.Errorf("wikisync: create wiki_sync_state: %w", err)
	}
	for _, stmt := range ensureColumnSQL {
		if _, err := s.DB.ExecContext(ctx, stmt); err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("wikisync: ensure column (%s): %w", stmt, err)
		}
	}
	return nil
}

// isDuplicateColumn 判定「列已存在」错误：MySQL 1060 与 sqlite 的消息都含
// "duplicate column"（大小写不敏感匹配，见 ensureColumnSQL 注释）。
func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column")
}

// LoadAll 读全表（一轮一次，内存做 diff）。永不为 nil。
func (s *StateStore) LoadAll(ctx context.Context) (map[string]State, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT doc_token, title, dir, obj_edit_time, task_id, item_ids, node_token, parent_node_token, depth, node_url FROM wiki_sync_state")
	if err != nil {
		return nil, fmt.Errorf("wikisync: load state: %w", err)
	}
	defer rows.Close()
	out := map[string]State{}
	for rows.Next() {
		var st State
		var ids sql.NullString
		if err := rows.Scan(&st.DocToken, &st.Title, &st.Dir, &st.ObjEditTime, &st.TaskID, &ids,
			&st.NodeToken, &st.ParentNodeToken, &st.Depth, &st.NodeURL); err != nil {
			return nil, fmt.Errorf("wikisync: scan state: %w", err)
		}
		st.ItemIDs = decodeItemIDs(ids.String)
		out[st.DocToken] = st
	}
	return out, rows.Err()
}

// Save upsert 单文档状态。先 SELECT 判存在再 UPDATE/INSERT：MySQL 对「值未变化的
// UPDATE」affected=0，若用 UPDATE→0 行则 INSERT 会撞主键；且避免 MySQL 专有
// ON DUPLICATE KEY 语法（保 sqlite 可测）。单 writer（单 syncer goroutine）无并发。
func (s *StateStore) Save(ctx context.Context, st State) error {
	ids := st.ItemIDs
	if ids == nil {
		ids = []int64{}
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("wikisync: encode item_ids: %w", err)
	}
	ts := s.now().UTC().Unix()
	var exists int
	if err := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM wiki_sync_state WHERE doc_token=?", st.DocToken).Scan(&exists); err != nil {
		return fmt.Errorf("wikisync: probe state %s: %w", st.DocToken, err)
	}
	if exists > 0 {
		if _, err := s.DB.ExecContext(ctx,
			`UPDATE wiki_sync_state SET title=?, dir=?, obj_edit_time=?, task_id=?, item_ids=?, updated_at=?, node_token=?, parent_node_token=?, depth=?, node_url=? WHERE doc_token=?`,
			st.Title, st.Dir, st.ObjEditTime, st.TaskID, string(blob), ts,
			st.NodeToken, st.ParentNodeToken, st.Depth, st.NodeURL, st.DocToken); err != nil {
			return fmt.Errorf("wikisync: update state %s: %w", st.DocToken, err)
		}
		return nil
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO wiki_sync_state (doc_token, title, dir, obj_edit_time, task_id, item_ids, updated_at, node_token, parent_node_token, depth, node_url) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		st.DocToken, st.Title, st.Dir, st.ObjEditTime, st.TaskID, string(blob), ts,
		st.NodeToken, st.ParentNodeToken, st.Depth, st.NodeURL); err != nil {
		return fmt.Errorf("wikisync: insert state %s: %w", st.DocToken, err)
	}
	return nil
}

// Delete 删除单文档状态（文档从 wiki 树消失时）。
func (s *StateStore) Delete(ctx context.Context, docToken string) error {
	if _, err := s.DB.ExecContext(ctx, "DELETE FROM wiki_sync_state WHERE doc_token=?", docToken); err != nil {
		return fmt.Errorf("wikisync: delete state %s: %w", docToken, err)
	}
	return nil
}

// UpdateTree 只回填单文档的树结构列（node_token/parent_node_token/depth/node_url），
// 不触碰变更判定字段（obj_edit_time/task_id/item_ids），也不改 updated_at（其语义保持
// 「上次入库写入」）。用于把扩列前写入的旧行补上树信息，且不改变「按 obj_edit_time 判
// 变更、只对变更文档入队」的既有语义。
func (s *StateStore) UpdateTree(ctx context.Context, docToken, nodeToken, parentNodeToken string, depth int, nodeURL string) error {
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE wiki_sync_state SET node_token=?, parent_node_token=?, depth=?, node_url=? WHERE doc_token=?`,
		nodeToken, parentNodeToken, depth, nodeURL, docToken); err != nil {
		return fmt.Errorf("wikisync: update tree %s: %w", docToken, err)
	}
	return nil
}

// TreeNodeMap 返回「目录名 → 父目录名」的映射，供清洗前缀的 L2 向上递归
// （cleaner.PrefixBuilder.ParentDirFunc）。链式上溯由 cleaner 侧做，上限 2 层。
//
// 目录名口径：清洗侧目录名 = `<节点 title 归一>`，其 clean 镜像 = `<dir>-clean`
// （cleaner.cleanDirName）。state.dir 存的是未加 -clean 的原始目录名，故本方法对
// key/value 统一追加 "-clean"，可直接喂给 ParentDirFunc（入参即目标目录的 -clean 名）。
//
// 父子推导：state 行（doc X）记录 node_token=X、parent_node_token=父节点 P、dir=D(P)
// （X 归属目录 = 其父节点标题归一）。再用 node_token→dir 反查 P 自身的 dir，即得
// D(P) 的父目录 D(parent(P))。父节点是根（parent_node_token 为空）或非 docx（无 state
// 行、反查缺失）→ 跳过该目录，调用方据此停止上溯。
//
// 已知边界：目录名按 title 归一，同名不同节点会互相覆盖（平铺 directories 表的既有
// 限制）；本方法只用于「补上下文」，覆盖错误只会少给/错给一层背景，不影响清洗正确性。
func (s *StateStore) TreeNodeMap(ctx context.Context) (map[string]string, error) {
	if s == nil || s.DB == nil {
		return nil, fmt.Errorf("wikisync: state DB 未配置")
	}
	rows, err := s.DB.QueryContext(ctx,
		"SELECT node_token, dir, parent_node_token FROM wiki_sync_state")
	if err != nil {
		return nil, fmt.Errorf("wikisync: tree map: %w", err)
	}
	defer rows.Close()

	nodeDir := map[string]string{} // node_token → 该节点自身归属目录（raw，未加 -clean）
	type edge struct{ dir, parentToken string }
	var edges []edge
	for rows.Next() {
		var nodeToken, dir, parentToken string
		if err := rows.Scan(&nodeToken, &dir, &parentToken); err != nil {
			return nil, fmt.Errorf("wikisync: tree map scan: %w", err)
		}
		if nodeToken == "" {
			continue // 旧行未回填 node_token：无父子信息，跳过
		}
		nodeDir[nodeToken] = dir
		edges = append(edges, edge{dir: dir, parentToken: parentToken})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("wikisync: tree map rows: %w", err)
	}

	out := map[string]string{}
	for _, e := range edges {
		if e.parentToken == "" || e.dir == "" {
			continue // 根层节点（父 = 空间根）没有父目录
		}
		parentDir, ok := nodeDir[e.parentToken]
		if !ok || parentDir == "" {
			continue // 父节点非 docx（无 state 行）/ 缺失：降级跳过
		}
		out[e.dir+cleanDirSuffix] = parentDir + cleanDirSuffix
	}
	return out, nil
}

// TreeStats 返回 state 表已记录树信息的规模：nodes = 已记录 node_token 的行数（即已落
// 父子关系的文档数），maxDepth = 这些行的最大 depth（根 = 0）。供 /api/wiki-sync/status
// 与 healthz 的 wiki_sync 段展示，便于核对父子关系与深度。
func (s *StateStore) TreeStats(ctx context.Context) (nodes, maxDepth int, err error) {
	if s == nil || s.DB == nil {
		return 0, 0, fmt.Errorf("wikisync: state DB 未配置")
	}
	row := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(MAX(depth), 0) FROM wiki_sync_state WHERE node_token <> ''")
	if err := row.Scan(&nodes, &maxDepth); err != nil {
		return 0, 0, fmt.Errorf("wikisync: tree stats: %w", err)
	}
	return nodes, maxDepth, nil
}

func decodeItemIDs(raw string) []int64 {
	if raw == "" {
		return nil
	}
	var ids []int64
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	if len(ids) == 0 {
		return nil // "[]" 与 NULL 归一为空（语义等价，便于比较）
	}
	return ids
}
