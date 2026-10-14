-- 004: 写操作审计表（anti-tamper）。此前写操作只在进程日志里留一行
-- log.Printf("api: audit action=...")，删日志即无痕；本表把每条写操作持久化，
-- 且成功路径的审计行与业务写在**同一个 MySQL 事务**内提交（审计写失败即业务回滚，
-- 见 internal/store/audit.go 的 withAuditedTx）。
--
-- append-only 语义：本表只 INSERT，不提供 UPDATE/DELETE 端点；运行时账号对它的
-- 写权限即普通 DML（由部署时收权后的 INSERT 覆盖）。
CREATE TABLE IF NOT EXISTS audit_log (
  id         BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  ts         TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  actor_sub  VARCHAR(190) NOT NULL DEFAULT '',
  actor_role VARCHAR(32)  NOT NULL DEFAULT '',
  action     VARCHAR(64)  NOT NULL,
  kb         VARCHAR(128) NOT NULL DEFAULT '',
  object_id  VARCHAR(64)  NOT NULL DEFAULT '',
  result     VARCHAR(16)  NOT NULL,
  source_ip  VARCHAR(64)  NOT NULL DEFAULT '',
  detail     VARCHAR(512) NOT NULL DEFAULT '',
  KEY idx_audit_ts (ts),
  KEY idx_audit_kb_ts (kb, ts),
  KEY idx_audit_actor_ts (actor_sub, ts)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
