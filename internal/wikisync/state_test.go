package wikisync

// state 表往返（sqlite 进程内，免 MySQL）：建表幂等、插入/更新 upsert、item_ids
// JSON 编解码、删除。DDL 保持 MySQL/sqlite 双兼容故可用 sqlite 直接覆盖。

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newStateStore(t *testing.T) *StateStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1) // :memory: 每连接一库，钉单连接保证同库
	t.Cleanup(func() { db.Close() })
	s := &StateStore{DB: db, Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema 幂等失败: %v", err)
	}
	return s
}

func TestStateStoreRoundTrip(t *testing.T) {
	s := newStateStore(t)
	ctx := context.Background()

	if got, err := s.LoadAll(ctx); err != nil || len(got) != 0 {
		t.Fatalf("空表 LoadAll=%v err=%v", got, err)
	}

	st := State{DocToken: "doc-1", Title: "k8s 集群", Dir: "ops", ObjEditTime: 100, TaskID: 7, ItemIDs: []int64{11, 12},
		NodeToken: "nt1", ParentNodeToken: "p0", Depth: 2, NodeURL: "https://example.feishu.cn/wiki/nt1"}
	if err := s.Save(ctx, st); err != nil {
		t.Fatalf("Save insert: %v", err)
	}
	all, err := s.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	got, ok := all["doc-1"]
	if !ok {
		t.Fatalf("doc-1 缺失: %v", all)
	}
	if got.Title != "k8s 集群" || got.Dir != "ops" || got.ObjEditTime != 100 || got.TaskID != 7 {
		t.Fatalf("往返字段错误: %+v", got)
	}
	if len(got.ItemIDs) != 2 || got.ItemIDs[0] != 11 || got.ItemIDs[1] != 12 {
		t.Fatalf("item_ids 往返错误: %v", got.ItemIDs)
	}
	if got.NodeToken != "nt1" || got.ParentNodeToken != "p0" || got.Depth != 2 ||
		got.NodeURL != "https://example.feishu.cn/wiki/nt1" {
		t.Fatalf("树结构字段往返错误: %+v", got)
	}

	// 更新（upsert 的 update 分支）：不得因 MySQL/sqlite 差异失败，也不得产生第二行。
	st.Title = "k8s 集群 v2"
	st.ObjEditTime = 200
	st.ItemIDs = []int64{13}
	if err := s.Save(ctx, st); err != nil {
		t.Fatalf("Save update: %v", err)
	}
	all, _ = s.LoadAll(ctx)
	if len(all) != 1 {
		t.Fatalf("更新应仍为 1 行: %v", all)
	}
	got = all["doc-1"]
	if got.Title != "k8s 集群 v2" || got.ObjEditTime != 200 || len(got.ItemIDs) != 1 || got.ItemIDs[0] != 13 {
		t.Fatalf("更新结果错误: %+v", got)
	}

	if err := s.Delete(ctx, "doc-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if all, _ = s.LoadAll(ctx); len(all) != 0 {
		t.Fatalf("删除后应为空: %v", all)
	}
}

// nil ItemIDs 落库为 []（不落 NULL、不 panic），读回为 nil（语义等价）。
func TestStateStoreNilItemIDs(t *testing.T) {
	s := newStateStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, State{DocToken: "d", Title: "t", Dir: "x", ObjEditTime: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	all, err := s.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if get := all["d"]; get.ItemIDs != nil {
		t.Fatalf("nil item_ids 应为空: %v", get.ItemIDs)
	}
}

// oldStateTableSQL 迁移前（2026-09-10 扩列前）的旧表结构，用于验证「旧库补列」幂等。
const oldStateTableSQL = `CREATE TABLE wiki_sync_state (
  doc_token     VARCHAR(190) NOT NULL,
  title         VARCHAR(255) NOT NULL DEFAULT '',
  dir           VARCHAR(255) NOT NULL DEFAULT '',
  obj_edit_time BIGINT NOT NULL DEFAULT 0,
  task_id       BIGINT NOT NULL DEFAULT 0,
  item_ids      TEXT,
  updated_at    BIGINT NOT NULL,
  PRIMARY KEY (doc_token)
)`

// 扩列迁移幂等：对「已存在的旧表」EnsureSchema 补 node_token/parent_node_token/depth/
// node_url 四列；重复调用不报错（第二次 ALTER 命中 duplicate column 被忽略）；已有行
// 保留且新列取默认值；新列可写可读。
func TestEnsureSchemaMigratesExistingTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, oldStateTableSQL); err != nil {
		t.Fatalf("建旧表: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO wiki_sync_state (doc_token, title, dir, obj_edit_time, task_id, item_ids, updated_at) VALUES ('d1','t','ops',111,5,'[7]',1)`); err != nil {
		t.Fatalf("插旧行: %v", err)
	}

	s := &StateStore{DB: db, Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema 补列失败: %v", err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema 应幂等（重复 ALTER 命中 duplicate column 被忽略）: %v", err)
	}

	all, err := s.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	old, ok := all["d1"]
	if !ok {
		t.Fatalf("旧行丢失: %v", all)
	}
	if old.ObjEditTime != 111 || old.TaskID != 5 || len(old.ItemIDs) != 1 || old.ItemIDs[0] != 7 {
		t.Fatalf("旧行内容被迁移改动: %+v", old)
	}
	if old.NodeToken != "" || old.ParentNodeToken != "" || old.Depth != 0 || old.NodeURL != "" {
		t.Fatalf("旧行新列应为默认空值: %+v", old)
	}

	// 新列可写可读。
	if err := s.Save(ctx, State{DocToken: "d2", Title: "x", Dir: "net", ObjEditTime: 1,
		NodeToken: "nt2", ParentNodeToken: "nt1", Depth: 3, NodeURL: "u2"}); err != nil {
		t.Fatalf("Save 新列: %v", err)
	}
	all, _ = s.LoadAll(ctx)
	got := all["d2"]
	if got.NodeToken != "nt2" || got.ParentNodeToken != "nt1" || got.Depth != 3 || got.NodeURL != "u2" {
		t.Fatalf("迁移后新列写入错误: %+v", got)
	}
}

// TreeNodeMap 父子映射：正常链、根节点无父、父节点缺失降级、旧行（node_token 空）忽略。
func TestTreeNodeMap(t *testing.T) {
	s := newStateStore(t)
	ctx := context.Background()
	seed := func(st State) {
		if err := s.Save(ctx, st); err != nil {
			t.Fatalf("seed %+v: %v", st, err)
		}
	}
	// 树：P(depth0, 空间根) → C(depth1, dir=系统) → D(depth2, dir=网络) → E(depth3, dir=文档D)
	seed(State{DocToken: "pd", NodeToken: "p1", ParentNodeToken: "", Dir: "SRE 知识库", Depth: 0})
	seed(State{DocToken: "cd", NodeToken: "c1", ParentNodeToken: "p1", Dir: "系统", Depth: 1})
	seed(State{DocToken: "dd", NodeToken: "d1", ParentNodeToken: "c1", Dir: "网络", Depth: 2})
	seed(State{DocToken: "ed", NodeToken: "e1", ParentNodeToken: "d1", Dir: "文档D", Depth: 3})
	// 父节点缺失（树中无 node_token=ghost 的行）：降级跳过。
	seed(State{DocToken: "xd", NodeToken: "x1", ParentNodeToken: "ghost", Dir: "孤立", Depth: 1})
	// 旧行（node_token 为空）：无父子信息，整行忽略。
	seed(State{DocToken: "ld", NodeToken: "", Dir: "旧", Depth: 0})

	m, err := s.TreeNodeMap(ctx)
	if err != nil {
		t.Fatalf("TreeNodeMap: %v", err)
	}
	want := map[string]string{
		"系统-clean":  "SRE 知识库-clean",
		"网络-clean":  "系统-clean",
		"文档D-clean": "网络-clean",
	}
	if len(m) != len(want) {
		t.Fatalf("TreeNodeMap 条数=%d want %d: %v", len(m), len(want), m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("TreeNodeMap[%q]=%q want %q（完整 %v）", k, m[k], v, m)
		}
	}
	// 根节点无父 → 不出现在 map（调用方据此停止上溯）。
	if _, ok := m["SRE 知识库-clean"]; ok {
		t.Errorf("根层目录不应有父目录: %v", m)
	}
	if _, ok := m["孤立-clean"]; ok {
		t.Errorf("父节点缺失应降级跳过: %v", m)
	}
}

// TreeStats：nodes 只计已记录 node_token 的行，maxDepth 取这些行的最大 depth。
func TestTreeStats(t *testing.T) {
	s := newStateStore(t)
	ctx := context.Background()
	if n, d, err := s.TreeStats(ctx); err != nil || n != 0 || d != 0 {
		t.Fatalf("空表 TreeStats=(%d,%d) err=%v", n, d, err)
	}
	save := func(st State) {
		if err := s.Save(ctx, st); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	save(State{DocToken: "a", NodeToken: "", Depth: 7}) // 旧行不计
	save(State{DocToken: "b", NodeToken: "n1", Depth: 1})
	save(State{DocToken: "c", NodeToken: "n2", Depth: 3})
	n, d, err := s.TreeStats(ctx)
	if err != nil || n != 2 || d != 3 {
		t.Fatalf("TreeStats=(%d,%d) err=%v want (2,3)", n, d, err)
	}
}
