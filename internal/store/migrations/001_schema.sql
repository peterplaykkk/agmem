-- agmem 元数据 schema（v1 骨架）
-- 以 go:embed 内嵌，服务启动时按需执行（幂等：IF NOT EXISTS）。

CREATE TABLE IF NOT EXISTS knowledge_bases (
  id          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  name        VARCHAR(128) NOT NULL UNIQUE,
  owner_sub   VARCHAR(190) NOT NULL DEFAULT '',
  created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS directories (
  id          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  kb_id       BIGINT UNSIGNED NOT NULL,
  name        VARCHAR(128) NOT NULL,
  created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uq_dir (kb_id, name),
  CONSTRAINT fk_dir_kb FOREIGN KEY (kb_id) REFERENCES knowledge_bases(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 清洗产生的镜像目录 `<dir>-clean` 不落 directories 表，
-- 由 payload point_type=clean 表达（由清洗管线产出）。

CREATE TABLE IF NOT EXISTS documents (
  id          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  kb_id       BIGINT UNSIGNED NOT NULL,
  dir_id      BIGINT UNSIGNED NOT NULL,
  task_id     BIGINT UNSIGNED NOT NULL,
  source_name VARCHAR(255) NOT NULL DEFAULT '',
  created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  KEY idx_doc_task (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS tasks (
  id          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  kb_id       BIGINT UNSIGNED NOT NULL,
  dir_id      BIGINT UNSIGNED NOT NULL,
  status      ENUM('pending','embedded','cleaning','cleaned','failed') NOT NULL DEFAULT 'pending',
  created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  KEY idx_task_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS task_items (
  id              BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  task_id         BIGINT UNSIGNED NOT NULL,
  kb_id           BIGINT UNSIGNED NOT NULL,
  dir_id          BIGINT UNSIGNED NOT NULL,
  status          ENUM('pending','embedded','cleaning','cleaned','failed') NOT NULL DEFAULT 'pending',
  original_text   MEDIUMTEXT NOT NULL,
  clean_text      MEDIUMTEXT NULL,
  error           TEXT NULL,
  point_id        CHAR(36) NULL,
  clean_point_id  CHAR(36) NULL,
  created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  KEY idx_item_task (task_id),
  KEY idx_item_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS kb_versions (
  kb_id       BIGINT UNSIGNED PRIMARY KEY,
  version     BIGINT UNSIGNED NOT NULL DEFAULT 0,
  updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
