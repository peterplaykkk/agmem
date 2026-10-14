-- 002: kb 读者授权（owner=knowledge_bases.owner_sub；reader=本表）
CREATE TABLE IF NOT EXISTS kb_readers (
  kb_id      BIGINT UNSIGNED NOT NULL,
  sub        VARCHAR(190) NOT NULL,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (kb_id, sub),
  CONSTRAINT fk_reader_kb FOREIGN KEY (kb_id) REFERENCES knowledge_bases(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
