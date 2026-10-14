package cache

// 本地缓存存储：SQLite（modernc.org/sqlite，纯 Go 无 cgo）。
// LRU：总 bytes 超上限按 last_hit_at 淘汰（UC-6，默认 500MB）。

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Entry 缓存条目（对应 queries 表）。
type Entry struct {
	ID string // 唯一 id（TEXT PK，sha256(dir|question)，见 ids.go）
	// KB / Global 单库化（2026-09-08）后降级为溯源字段：KB=写入时的 cfg.KB，
	// Global=请求 deprecated 字段回显；两者不再参与命中判定（scope 收敛为
	// (dir, question) 两维，Nearest 仅按 dir 过滤），保留仅为旧 schema 兼容。
	KB          string
	Dir         string // 查询时 scope：dir 精确值；""=全库
	Global      bool   // deprecated 溯源字段（见上）
	Question    string
	QuestionVec []byte // float32 768d 序列化
	ResultJSON  string
	KBVersion   uint64
	CreatedAt   time.Time
	LastHitAt   time.Time
	Bytes       int64
	Similarity  float64 `json:"-"` // 查询时填充（不落库）
}

// Store SQLite 缓存。
type Store struct {
	DB *sql.DB
}

// DefaultMaxBytes 本地缓存 LRU 上限默认值（500MB，原始需求 6）。agmemd 与
// agmemserv 进程内缓存共用同一默认口径。
const DefaultMaxBytes = 500 << 20

// OpenCache 打开（或创建）缓存库。
func OpenCache(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cache: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "cache.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("cache: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // sqlite 单写者，避免 busy
	const schema = `
CREATE TABLE IF NOT EXISTS queries (
  id            TEXT PRIMARY KEY,
  kb            TEXT NOT NULL,
  dir           TEXT NOT NULL DEFAULT '',
  global        INTEGER NOT NULL DEFAULT 0,
  question_text TEXT NOT NULL,
  question_vec  BLOB,
  result_json   TEXT NOT NULL,
  kb_version    INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  last_hit_at   INTEGER NOT NULL,
  bytes         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_queries_kb ON queries(kb);
CREATE INDEX IF NOT EXISTS idx_queries_hit ON queries(last_hit_at);
`
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("cache: schema: %w", err)
	}
	if err := migrateAddGlobal(db); err != nil {
		return nil, fmt.Errorf("cache: schema migrate: %w", err)
	}
	return &Store{DB: db}, nil
}

// migrateAddGlobal 旧缓存库升级：global 列（P2-1 scope 隔离）在 v0 库中不存在。
// SQLite 无 ADD COLUMN IF NOT EXISTS，先查 table_info 再补列（幂等）。
func migrateAddGlobal(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(queries)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasGlobal := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == "global" {
			hasGlobal = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if hasGlobal {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE queries ADD COLUMN global INTEGER NOT NULL DEFAULT 0`)
	return err
}

// Put 写入条目（id 冲突时覆盖——同问题的最新结果替换旧结果）。
func (s *Store) Put(e Entry) error {
	now := time.Now().Unix()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Unix(now, 0)
	}
	e.LastHitAt = e.CreatedAt
	if e.Bytes == 0 {
		e.Bytes = entryBytes(e)
	}
	_, err := s.DB.Exec(`INSERT INTO queries
		(id, kb, dir, global, question_text, question_vec, result_json, kb_version, created_at, last_hit_at, bytes)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  result_json=excluded.result_json, kb_version=excluded.kb_version,
		  question_vec=excluded.question_vec, dir=excluded.dir, global=excluded.global,
		  created_at=excluded.created_at, last_hit_at=excluded.last_hit_at, bytes=excluded.bytes`,
		e.ID, e.KB, e.Dir, boolInt(e.Global), e.Question, e.QuestionVec, e.ResultJSON, e.KBVersion,
		e.CreatedAt.Unix(), e.LastHitAt.Unix(), e.Bytes)
	if err != nil {
		return fmt.Errorf("cache: put %s: %w", e.ID, err)
	}
	return nil
}

// entryBytes 条目占用字节数（含向量与结果文本）。
func entryBytes(e Entry) int64 {
	return int64(len(e.ResultJSON) + len(e.QuestionVec) + len(e.Question) + 64)
}

// Get 按 id 取条目。
func (s *Store) Get(id string) (*Entry, error) {
	e, err := s.scanOne(s.DB.QueryRow(
		`SELECT id, kb, dir, global, question_text, question_vec, result_json, kb_version, created_at, last_hit_at, bytes
		 FROM queries WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, ErrCacheNotFound
	}
	return e, err
}

// Touch 更新 last_hit_at（LRU 访问）。
func (s *Store) Touch(id string) error {
	_, err := s.DB.Exec("UPDATE queries SET last_hit_at=? WHERE id=?", time.Now().Unix(), id)
	return err
}

func (s *Store) scanOne(row *sql.Row) (*Entry, error) {
	var e Entry
	var created, hit, global int64
	var qvec []byte
	if err := row.Scan(&e.ID, &e.KB, &e.Dir, &global, &e.Question, &qvec, &e.ResultJSON,
		&e.KBVersion, &created, &hit, &e.Bytes); err != nil {
		return nil, err
	}
	e.Global = global != 0
	e.QuestionVec = qvec
	e.CreatedAt = time.Unix(created, 0)
	e.LastHitAt = time.Unix(hit, 0)
	return &e, nil
}

// Nearest 在 scope 内找与 vec 余弦相似度最高的历史问题。
// 单库化（2026-09-08）后 scope = dir 一维：dir 精确匹配（""=全库查询）——
// kb 恒为全局唯一库、global 维度退役，两列仅为旧 schema 兼容保留、不再过滤。
// id 仍为 sha256(dir|question)（ids.go），不变。
// 返回 (entry, similarity)；scope 内无记录返回 ErrCacheNotFound。
func (s *Store) Nearest(dir string, vec []float32) (*Entry, float64, error) {
	rows, err := s.DB.Query(
		`SELECT id, kb, dir, global, question_text, question_vec, result_json, kb_version, created_at, last_hit_at, bytes
		 FROM queries WHERE dir=? AND question_vec IS NOT NULL`, dir)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var best *Entry
	bestSim := math.Inf(-1)
	for rows.Next() {
		var e Entry
		var created, hit, global int64
		var qvec []byte
		if err := rows.Scan(&e.ID, &e.KB, &e.Dir, &global, &e.Question, &qvec, &e.ResultJSON,
			&e.KBVersion, &created, &hit, &e.Bytes); err != nil {
			return nil, 0, err
		}
		e.Global = global != 0
		e.QuestionVec = qvec
		e.CreatedAt = time.Unix(created, 0)
		e.LastHitAt = time.Unix(hit, 0)
		qv := DecodeVec(qvec)
		sim := Cosine(vec, qv)
		if sim > bestSim {
			bestSim = sim
			cp := e // 拷贝避免取址冲突
			best = &cp
		}
	}
	if best == nil {
		return nil, 0, ErrCacheNotFound
	}
	return best, bestSim, rows.Err()
}

// TotalBytes 当前缓存总占用。
func (s *Store) TotalBytes() (int64, error) {
	var v int64
	err := s.DB.QueryRow("SELECT COALESCE(SUM(bytes),0) FROM queries").Scan(&v)
	return v, err
}

// EvictOver 超出 maxBytes 时按 last_hit_at 从旧到新淘汰，返回释放字节数。
func (s *Store) EvictOver(maxBytes int64) (int64, error) {
	var freed int64
	for {
		total, err := s.TotalBytes()
		if err != nil {
			return freed, err
		}
		if total <= maxBytes {
			return freed, nil
		}
		var victimID string
		var victimBytes int64
		err = s.DB.QueryRow(
			`SELECT id, bytes FROM queries ORDER BY last_hit_at ASC, created_at ASC LIMIT 1`).
			Scan(&victimID, &victimBytes)
		if err == sql.ErrNoRows {
			return freed, nil // 空表兜底
		}
		if err != nil {
			return freed, err
		}
		if _, err := s.DB.Exec("DELETE FROM queries WHERE id=?", victimID); err != nil {
			return freed, err
		}
		freed += victimBytes
	}
}

// Clear 按 id 删除，返回释放字节数。
func (s *Store) Clear(id string) (int64, error) {
	e, err := s.Get(id)
	if err == ErrCacheNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if _, err := s.DB.Exec("DELETE FROM queries WHERE id=?", id); err != nil {
		return 0, err
	}
	return e.Bytes, nil
}

// ClearAll 清空，返回释放字节数。
func (s *Store) ClearAll() (int64, error) {
	freed, err := s.TotalBytes()
	if err != nil {
		return 0, err
	}
	if _, err := s.DB.Exec("DELETE FROM queries"); err != nil {
		return 0, err
	}
	return freed, nil
}

// —— 向量序列化 ——

// EncodeVec float32 → BLOB（小端）。
func EncodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		bits := math.Float32bits(f)
		b[i*4] = byte(bits)
		b[i*4+1] = byte(bits >> 8)
		b[i*4+2] = byte(bits >> 16)
		b[i*4+3] = byte(bits >> 24)
	}
	return b
}

// DecodeVec BLOB → float32（长度不合法返回 nil）。
func DecodeVec(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		bits := uint32(b[i*4]) | uint32(b[i*4+1])<<8 | uint32(b[i*4+2])<<16 | uint32(b[i*4+3])<<24
		out[i] = math.Float32frombits(bits)
	}
	return out
}

// Cosine 余弦相似度（输入假定已 L2 归一化时等价于点积；此处做完整计算防未归一化输入）。
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// SimilarityThreshold 默认命中阈值（原始需求 15：置信度 >0.8 才直接返回本地结果）。
// 实测（2026-09-04 bge-base-zh-v1.5 int8 Mac CPU）：强近义对 ~0.92，弱近义短问句对 ~0.71，
// 无关对 ~0.19。阈值可用环境变量 AGMEMD_SIM_THRESHOLD 覆盖（agmemd 启动时注入）。
const SimilarityThreshold = 0.8

// simThreshold 当前生效阈值（进程级，agmemd 启动时 SetSimThreshold 覆盖）。
var simThreshold = SimilarityThreshold

// SetSimThreshold 覆盖命中阈值（仅 agmemd 启动时调用一次）。
func SetSimThreshold(v float64) { simThreshold = v }

// CurrentSimThreshold 当前生效阈值。
func CurrentSimThreshold() float64 { return simThreshold }

// ShouldHitLocal 命中判定：相似度严格大于阈值且 kb_version 一致。
func ShouldHitLocal(sim float64, localVersion, remoteVersion uint64) bool {
	return sim > simThreshold && localVersion == remoteVersion
}

// SanitizeID 防路径注入（日志/输出场景）。
func SanitizeID(id string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 0x20 {
			return '-'
		}
		return r
	}, id)
}

// boolInt bool → SQLite INTEGER。
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
