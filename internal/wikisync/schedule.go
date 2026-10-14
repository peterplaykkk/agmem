// schedule.go：调度窗口判定（纯函数，可单测）。
//
// 语义：每 30 分钟一个槽，仅在允许小时集合内执行；每分钟 tick 判定一次，
// 同一 30 分钟槽内只跑一次；跨天正确（23:30 跑完，次日 0:00 还要再跑）。
// 时区固定 Asia/Shanghai（可按 AGMEM_WIKISYNC_TZ 覆盖）。纯函数让边界可测。
//
// 窗口为何排除 9-12 与 14-18：这两段是 deepseek-flash
// 的调用高峰期，wiki 知识提取（清洗）刻意避开，落在非高峰时段跑。
package wikisync

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SlotInterval 调度槽粒度：每 30 分钟一个可执行槽（:00 与 :30）。
const SlotInterval = 30 * time.Minute

// ParseHours 解析 "0-8,12-13,18-23" 形式的小时集合（支持单值与闭区间，逗号分隔）。
// 非法（空段/非数字/越界/区间反序/整体为空）返回 error——配置失误 fail-fast，
// 而不是静默不跑或跑错窗口。
func ParseHours(spec string) (map[int]bool, error) {
	out := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '-'); i >= 0 {
			lo, err1 := strconv.Atoi(strings.TrimSpace(part[:i]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(part[i+1:]))
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("wikisync: 非法小时区间 %q", part)
			}
			if lo < 0 || hi > 23 || lo > hi {
				return nil, fmt.Errorf("wikisync: 小时区间越界/反序 %q", part)
			}
			for h := lo; h <= hi; h++ {
				out[h] = true
			}
			continue
		}
		h, err := strconv.Atoi(part)
		if err != nil || h < 0 || h > 23 {
			return nil, fmt.Errorf("wikisync: 非法小时 %q", part)
		}
		out[h] = true
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("wikisync: 小时集合为空")
	}
	return out, nil
}

func locOrUTC(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}

// AllowedHour 判定 t（按 loc 换算）的小时是否在允许集合内。
func AllowedHour(t time.Time, loc *time.Location, hours map[int]bool) bool {
	return hours[t.In(locOrUTC(loc)).Hour()]
}

// SlotKey 唯一标识一个 (日期, 小时, 30 分钟槽) 窗口。日期是键的一部分，故跨天
// 天然正确：23:30 与次日 00:00 是两个不同键，各自可跑一次。同一小时内的 :00 与
// :30 也是两个不同键——这是「每 30 分钟一次」的实现方式。
//
// 用 Truncate 而非手工判分钟：Asia/Shanghai 是整小时偏移时区（无 DST），
// 截断到 30 分钟边界恒落在本地 :00/:30。
func SlotKey(t time.Time, loc *time.Location) string {
	return t.In(locOrUTC(loc)).Truncate(SlotInterval).Format("2006-01-02T15:04")
}

// ShouldRun 判定本轮是否应执行：t 落在允许小时，且该 30 分钟槽尚未执行过。
// lastKey 为上一次执行的槽键（空串 = 从未执行）。
func ShouldRun(t time.Time, loc *time.Location, hours map[int]bool, lastKey string) bool {
	if !AllowedHour(t, loc, hours) {
		return false
	}
	return SlotKey(t, loc) != lastKey
}

// NextRun 返回严格晚于 after 的下一个「允许小时内的 30 分钟槽边界」（按 loc）。
// 允许集合非空时最多看未来 8 天必然找到；找不到返回零值。用于状态端点的
// 「下次计划时间」。
//
// 口径：从 after 所在槽的下一个槽边界起逐槽推进（步长 30 分钟；跨天/跨月由
// time.Add 正确处理；默认时区 Asia/Shanghai 无 DST，槽边界稳定）。
func NextRun(after time.Time, loc *time.Location, hours map[int]bool) time.Time {
	l := locOrUTC(loc)
	cursor := after.In(l).Truncate(SlotInterval).Add(SlotInterval)
	for i := 0; i < 48*8; i++ {
		if hours[cursor.Hour()] {
			return cursor
		}
		cursor = cursor.Add(SlotInterval)
	}
	return time.Time{}
}
