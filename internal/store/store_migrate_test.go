package store

import (
	"strings"
	"testing"
)

// 回归：迁移文件中「注释行在前 + 其后 DDL」的块不得被整块跳过。
// 2026-09-06 事故：001_schema.sql 文件头注释导致 knowledge_bases 建表
// 被静默丢弃，后续 FK 建表报 errno 1824（Failed to open the referenced
// table 'knowledge_bases'）。
func TestSplitStatementsKeepsDDLAfterComment(t *testing.T) {
	sqlText := `-- 文件头注释
-- 第二行注释

CREATE TABLE IF NOT EXISTS knowledge_bases (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY
) ENGINE=InnoDB;

-- 行间注释：清洗产生的镜像目录不落 directories 表。

CREATE TABLE IF NOT EXISTS directories (
  kb_id BIGINT UNSIGNED NOT NULL,
  CONSTRAINT fk_dir_kb FOREIGN KEY (kb_id) REFERENCES knowledge_bases(id)
) ENGINE=InnoDB;
`
	stmts := splitStatements(sqlText)
	if len(stmts) != 2 {
		t.Fatalf("want 2 statements, got %d: %q", len(stmts), stmts)
	}
	if !strings.Contains(stmts[0], "CREATE TABLE IF NOT EXISTS knowledge_bases") {
		t.Errorf("stmt[0] lost knowledge_bases DDL: %q", stmts[0])
	}
	if !strings.Contains(stmts[1], "CREATE TABLE IF NOT EXISTS directories") {
		t.Errorf("stmt[1] lost directories DDL: %q", stmts[1])
	}
	if strings.Contains(strings.Join(stmts, "\n"), "--") {
		t.Errorf("comment lines must be stripped: %q", stmts)
	}
}

// 无注释纯 DDL 与空语句过滤行为不变。
func TestSplitStatementsBasic(t *testing.T) {
	stmts := splitStatements("CREATE TABLE a (id INT);\n\nCREATE TABLE b (id INT);")
	if len(stmts) != 2 {
		t.Fatalf("want 2 statements, got %d: %q", len(stmts), stmts)
	}
	if got := splitStatements("-- only comment\n"); len(got) != 0 {
		t.Fatalf("want 0 statements, got %q", got)
	}
}
