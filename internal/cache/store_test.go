package cache

import (
	"database/sql"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenCache(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	t.Cleanup(func() { s.DB.Close() })
	return s
}

func vecOf(vals ...float32) []byte { return EncodeVec(vals) }

func TestPutGet(t *testing.T) {
	s := testStore(t)
	e := Entry{ID: "q-1", KB: "sre", Question: "问题", QuestionVec: vecOf(1, 0), ResultJSON: `{"hits":1}`}
	if err := s.Put(e); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get("q-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.KB != "sre" || got.ResultJSON != `{"hits":1}` {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if len(DecodeVec(got.QuestionVec)) != 2 {
		t.Fatalf("vec roundtrip: %v", got.QuestionVec)
	}
}

func TestGet_NotFound(t *testing.T) {
	s := testStore(t)
	if _, err := s.Get("nope"); err != ErrCacheNotFound {
		t.Fatalf("want ErrCacheNotFound, got %v", err)
	}
}

func TestNearest_PicksBest(t *testing.T) {
	s := testStore(t)
	_ = s.Put(Entry{ID: "a", KB: "sre", Dir: "mysql", Question: "a", QuestionVec: vecOf(1, 0), ResultJSON: "A"})
	_ = s.Put(Entry{ID: "b", KB: "sre", Dir: "mysql", Question: "b", QuestionVec: vecOf(0, 1), ResultJSON: "B"})
	_ = s.Put(Entry{ID: "c", KB: "sre", Dir: "k8s", Question: "c", QuestionVec: vecOf(0.9, 0.1), ResultJSON: "C"})
	e, sim, err := s.Nearest("mysql", []float32{0.95, 0.05})
	if err != nil {
		t.Fatalf("Nearest: %v", err)
	}
	if e.ID != "a" {
		t.Fatalf("want a, got %s", e.ID)
	}
	if sim <= 0.9 {
		t.Fatalf("want sim>0.9, got %f", sim)
	}
	// dir 隔离：k8s 内不受影响
	if e2, _, err := s.Nearest("k8s", []float32{0, 1}); err == nil && e2.ID == "a" {
		t.Fatalf("dir isolation broken")
	}
}

func TestNearest_Empty(t *testing.T) {
	s := testStore(t)
	if _, _, err := s.Nearest("", []float32{1, 0}); err != ErrCacheNotFound {
		t.Fatalf("want ErrCacheNotFound, got %v", err)
	}
}

// —— scope 隔离（单库版）：dir 精确匹配 ——
// 同一向量预置为某 dir 的历史问题后，跨 dir 查询不得命中（范围泄漏）。
// 单库化（2026-09-08）后 kb/global 维度退役，不再参与隔离。

func TestNearest_ScopeIsolation(t *testing.T) {
	cases := []struct {
		name string
		// 预置条目 scope
		putDir string
		// 查询 scope
		qDir string
		want bool // 是否允许命中
	}{
		{name: "全库条目 vs dir查询-不命中", putDir: "", qDir: "k8s", want: false},
		{name: "全库条目 vs 全库查询-命中", putDir: "", qDir: "", want: true},
		{name: "dir条目 vs 全库查询-不命中", putDir: "k8s", qDir: "", want: false},
		{name: "dir条目 vs 他dir查询-不命中", putDir: "mysql", qDir: "k8s", want: false},
		{name: "dir条目 vs 同dir命中", putDir: "k8s", qDir: "k8s", want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := testStore(t)
			if err := s.Put(Entry{
				ID: "q-seed", KB: "sre", Dir: c.putDir,
				Question: "历史问题", QuestionVec: vecOf(1, 0),
				ResultJSON: `{"hits":7}`, KBVersion: 4,
			}); err != nil {
				t.Fatalf("Put: %v", err)
			}
			e, sim, err := s.Nearest(c.qDir, []float32{0.99, 0.01})
			if c.want {
				if err != nil {
					t.Fatalf("want hit, got err=%v", err)
				}
				if e.ID != "q-seed" || e.Dir != c.putDir {
					t.Fatalf("hit mismatch: id=%s dir=%q sim=%f", e.ID, e.Dir, sim)
				}
				if sim <= 0.9 {
					t.Fatalf("want sim>0.9, got %f", sim)
				}
				return
			}
			if err != ErrCacheNotFound {
				t.Fatalf("cross-scope must not hit (want ErrCacheNotFound), got e=%+v sim=%f err=%v", e, sim, err)
			}
		})
	}
}

// Store.Put 的 ON CONFLICT 契约：同 id 直写覆盖后新结果生效。
// （守护进程经 QueryIDUnique 碰撞分叉，见 cachectl 测试。）
func TestPut_OverwriteReplacesResult(t *testing.T) {
	s := testStore(t)
	id := QueryID("", "APISIX 上游健康检查怎么做")
	put := func(result string) {
		if err := s.Put(Entry{
			ID: id, KB: "sre", Dir: "",
			Question: "APISIX 上游健康检查怎么做", QuestionVec: vecOf(1, 0),
			ResultJSON: result, KBVersion: 2,
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	put(`{"hits":1}`)
	e, _, err := s.Nearest("", []float32{1, 0})
	if err != nil || e.ResultJSON != `{"hits":1}` {
		t.Fatalf("first result expected: e=%+v err=%v", e, err)
	}
	put(`{"hits":99}`) // 同 id 覆盖
	if e, _, err = s.Nearest("", []float32{1, 0}); err != nil || e.ResultJSON != `{"hits":99}` {
		t.Fatalf("overwritten result expected: e=%+v err=%v", e, err)
	}
}

// —— 阈值边界（>0.8 严格大于）——

func TestShouldHitLocal_Boundary(t *testing.T) {
	cases := []struct {
		sim    float64
		local  uint64
		remote uint64
		want   bool
	}{
		{0.81, 1, 1, true},
		{0.8, 1, 1, false}, // 严格大于：0.8 不命中
		{0.799999, 1, 1, false},
		{0.99, 1, 2, false}, // 版本不一致不命中
		{0.99, 0, 0, true},
		{0.5, 5, 5, false},
		{math.Nextafter(0.8, 1.0), 1, 1, true}, // 恰好比 0.8 大一点
	}
	for _, c := range cases {
		if got := ShouldHitLocal(c.sim, c.local, c.remote); got != c.want {
			t.Errorf("ShouldHitLocal(%.6f,%d,%d)=%v want %v", c.sim, c.local, c.remote, got, c.want)
		}
	}
}

// —— LRU 淘汰 ——

func TestEvictOver_LRU(t *testing.T) {
	s := testStore(t)
	mk := func(id string, size int) Entry {
		return Entry{
			ID: id, KB: "sre", Question: id,
			ResultJSON: strings.Repeat("x", size),
		}
	}
	_ = s.Put(mk("old1", 100)) // 最早
	_ = s.Put(mk("old2", 100))
	_ = s.Put(mk("new1", 100))
	// touch new1 → old1 最旧
	if err := s.Touch("new1"); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	// 每条 bytes = 100+len(id)+64 ≈ 168，共 ~504；限 400 → 只淘汰最旧一条
	freed, err := s.EvictOver(400)
	if err != nil {
		t.Fatalf("EvictOver: %v", err)
	}
	if freed <= 0 {
		t.Fatalf("want freed>0, got %d", freed)
	}
	if _, err := s.Get("old1"); err != ErrCacheNotFound {
		t.Fatalf("oldest should be evicted, got err=%v", err)
	}
	for _, id := range []string{"old2", "new1"} {
		if _, err := s.Get(id); err != nil {
			t.Fatalf("%s should survive: %v", id, err)
		}
	}
	if total, _ := s.TotalBytes(); total > 400 {
		t.Fatalf("total %d still over limit", total)
	}
}

func TestEvictOver_UnderLimit(t *testing.T) {
	s := testStore(t)
	_ = s.Put(Entry{ID: "a", KB: "sre", Question: "a", ResultJSON: "small"})
	freed, err := s.EvictOver(1 << 20)
	if err != nil || freed != 0 {
		t.Fatalf("want 0 freed, got %d, err=%v", freed, err)
	}
}

func TestClear(t *testing.T) {
	s := testStore(t)
	_ = s.Put(Entry{ID: "a", KB: "sre", Question: "a", ResultJSON: strings.Repeat("y", 50)})
	freed, err := s.Clear("a")
	if err != nil || freed <= 0 {
		t.Fatalf("Clear: freed=%d err=%v", freed, err)
	}
	freed2, err := s.Clear("a") // 幂等
	if err != nil || freed2 != 0 {
		t.Fatalf("Clear idempotent: freed=%d err=%v", freed2, err)
	}
	_ = s.Put(Entry{ID: "b", KB: "sre", Question: "b", ResultJSON: "z"})
	_ = s.Put(Entry{ID: "c", KB: "other", Question: "c", ResultJSON: "z"})
	freedAll, err := s.ClearAll()
	if err != nil || freedAll <= 0 {
		t.Fatalf("ClearAll: %d %v", freedAll, err)
	}
	if total, _ := s.TotalBytes(); total != 0 {
		t.Fatalf("want empty after ClearAll, total=%d", total)
	}
}

// —— 向量编解码 ——

func TestVecRoundtrip(t *testing.T) {
	v := []float32{0.5, -1.25, 3.75, 0}
	got := DecodeVec(EncodeVec(v))
	if len(got) != len(v) {
		t.Fatalf("len mismatch")
	}
	for i := range v {
		if got[i] != v[i] {
			t.Fatalf("vec[%d]: %f != %f", i, got[i], v[i])
		}
	}
	if DecodeVec([]byte{1, 2, 3}) != nil {
		t.Fatal("bad length must yield nil")
	}
}

// —— 余弦与归一化 ——

func TestCosine(t *testing.T) {
	cases := []struct {
		a, b []float32
		want float64
	}{
		{[]float32{1, 0}, []float32{1, 0}, 1},
		{[]float32{1, 0}, []float32{0, 1}, 0},
		{[]float32{1, 0}, []float32{-1, 0}, -1},
		{[]float32{}, []float32{1}, 0},
		{[]float32{1, 2}, []float32{1, 2}, 1}, // 未归一化同向=1
		{[]float32{0, 0}, []float32{1, 1}, 0}, // 零向量防除零
	}
	for i, c := range cases {
		if got := Cosine(c.a, c.b); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("case %d: Cosine=%f want %f", i, got, c.want)
		}
	}
}

// —— 唯一 id（单库版：(dir, question) 两维派生）——

func TestQueryID(t *testing.T) {
	id1 := QueryID("mysql", "binlog 怎么配")
	id2 := QueryID("mysql", "binlog 怎么配")
	if id1 != id2 {
		t.Fatal("same input must give same id")
	}
	if id1 == QueryID("mysql", "另一问题") {
		t.Fatal("different question must differ")
	}
	if id1 == QueryID("k8s", "binlog 怎么配") {
		t.Fatal("different dir must differ")
	}
	// kb 维度已退役：不再作为派生入参（编译期签名即保证），空 dir 与带 dir 必不同
	if id1 == QueryID("", "binlog 怎么配") {
		t.Fatal("dir dimension must participate in id derivation")
	}
	if !strings.HasPrefix(id1, "q-") {
		t.Fatalf("want q- prefix, got %s", id1)
	}
}

func TestQueryIDUnique_Collision(t *testing.T) {
	calls := 0
	id := QueryIDUnique("d", "q", func(id string) bool {
		calls++
		return true // 恒碰撞
	})
	if calls != 1 {
		t.Fatalf("want 1 exists check, got %d", calls)
	}
	if !strings.HasPrefix(id, QueryID("d", "q")+"-") {
		t.Fatalf("collision must add timestamp suffix, got %s", id)
	}
	if id == QueryID("d", "q") {
		t.Fatal("collided id must differ from base")
	}
}

func TestQueryIDUnique_NoCollision(t *testing.T) {
	id := QueryIDUnique("d", "q", func(string) bool { return false })
	if id != QueryID("d", "q") {
		t.Fatalf("no collision should keep base id, got %s", id)
	}
}

func TestSanitizeID(t *testing.T) {
	if got := SanitizeID("a/b\\c\nd"); got != "a-b-c-d" {
		t.Fatalf("got %q", got)
	}
}

// —— 旧缓存库升级（migrateAddGlobal）：v0 schema 无 global 列，OpenCache 须补列且数据保留 ——

func TestOpenCache_MigratesLegacySchema(t *testing.T) {
	dir := t.TempDir()
	legacy := `
CREATE TABLE IF NOT EXISTS queries (
  id            TEXT PRIMARY KEY,
  kb            TEXT NOT NULL,
  dir           TEXT NOT NULL DEFAULT '',
  question_text TEXT NOT NULL,
  question_vec  BLOB,
  result_json   TEXT NOT NULL,
  kb_version    INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  last_hit_at   INTEGER NOT NULL,
  bytes         INTEGER NOT NULL DEFAULT 0
);`
	s0, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "cache.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := s0.Exec(legacy); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := s0.Exec(`INSERT INTO queries
		(id, kb, dir, question_text, question_vec, result_json, kb_version, created_at, last_hit_at, bytes)
		VALUES ('q-legacy','sre','mysql','老问题',?, '{"hits":1}',3,1,1,10)`,
		vecOf(1, 0)); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := s0.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s, err := OpenCache(dir) // 触发 migrateAddGlobal
	if err != nil {
		t.Fatalf("OpenCache migrate: %v", err)
	}
	defer s.DB.Close()
	// 旧数据保留且可读（global 默认 false=kb/dir 级，与旧语义一致）
	e, err := s.Get("q-legacy")
	if err != nil {
		t.Fatalf("legacy row lost: %v", err)
	}
	if e.KB != "sre" || e.Dir != "mysql" || e.Global {
		t.Fatalf("legacy row mismatch: %+v", e)
	}
	// 单库版：Nearest 仅按 dir 匹配，旧 kb/global 列不再参与过滤
	if _, _, err := s.Nearest("mysql", []float32{1, 0}); err != nil {
		t.Fatalf("legacy row must be searchable in same dir scope: %v", err)
	}
	// 新列可写
	if err := s.Put(Entry{ID: "q-new", KB: "sre", Global: true, Question: "x",
		QuestionVec: vecOf(0, 1), ResultJSON: "y"}); err != nil {
		t.Fatalf("put after migrate: %v", err)
	}
	if _, _, err := s.Nearest("", []float32{0, 1}); err != nil {
		t.Fatalf("whole-kb scope after migrate: %v", err)
	}
}
