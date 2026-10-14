package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// QueryID 生成缓存唯一 id：sha256(dir|question)。
// 单库化（2026-09-08）后本地缓存 scope 收敛为 (dir, question) 两维——kb 恒为
// 全局唯一库（cfg.KB）、global 维度退役，均不再参与派生；dir 保留（--dir 过滤）。
// 确定性 id 使同一问题的重写版本可被 LRU 自然覆盖；碰撞（同 id 不同时刻重写）
// 加纳秒时间戳后缀（原始需求 13：每个提问在本地有唯一 id）。
// 兼容注：旧版 id 派生含 kb 维度，存量条目 id 与新条目不同——旧条目携带旧版本
// 锚点（跨 kb 指纹）≠ 新的单 kb 版本，经版本门禁天然失效，并随命中失败的
// 过期清理路径逐步淘汰；不迁移本地缓存。
func QueryID(dir, question string) string {
	sum := sha256.Sum256([]byte(dir + "|" + question))
	return "q-" + hex.EncodeToString(sum[:8])
}

// QueryIDUnique 在 QueryID 基础上保证唯一：调用方用 exists 回调判定碰撞。
func QueryIDUnique(dir, question string, exists func(id string) bool) string {
	id := QueryID(dir, question)
	if !exists(id) {
		return id
	}
	return fmt.Sprintf("%s-%d", id, time.Now().UnixNano())
}
