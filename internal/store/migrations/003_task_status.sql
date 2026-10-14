-- 003: 任务级状态扩展（finding 1：条目全终态后任务状态推进）
ALTER TABLE tasks MODIFY status ENUM('pending','embedded','cleaning','cleaned','failed','completed','completed_with_errors') NOT NULL DEFAULT 'pending';
