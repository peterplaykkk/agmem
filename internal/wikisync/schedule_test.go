package wikisync

// 调度窗口边界（纯函数，表驱动）：允许小时集合解析、小时判定、同一小时只跑一次、
// 跨天（23→0）、窗口外不跑、下次计划时间。
// 时区用固定 +08:00，避免依赖宿主机 tzdata。

import (
	"testing"
	"time"
)

var cst = time.FixedZone("CST", 8*3600)

func mustHours(t *testing.T, spec string) map[int]bool {
	t.Helper()
	h, err := ParseHours(spec)
	if err != nil {
		t.Fatalf("ParseHours(%q): %v", spec, err)
	}
	return h
}

func TestParseHours(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want map[int]bool
		bad  bool
	}{
		{name: "默认集合", spec: "0-9,12-14,18-23",
			want: map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 12: true, 13: true, 14: true, 18: true, 19: true, 20: true, 21: true, 22: true, 23: true}},
		{name: "单值", spec: "5", want: map[int]bool{5: true}},
		{name: "单值多点", spec: "1,3,5", want: map[int]bool{1: true, 3: true, 5: true}},
		{name: "空白容忍", spec: " 0 - 2 , 7 ", want: map[int]bool{0: true, 1: true, 2: true, 7: true}},
		{name: "边界0-23", spec: "0-23", want: func() map[int]bool {
			m := map[int]bool{}
			for h := 0; h <= 23; h++ {
				m[h] = true
			}
			return m
		}()},
		{name: "越界24", spec: "24", bad: true},
		{name: "区间上界越界", spec: "0-24", bad: true},
		{name: "区间反序", spec: "9-3", bad: true},
		{name: "负值", spec: "-1", bad: true},
		{name: "非数字", spec: "abc", bad: true},
		{name: "空集合", spec: " , , ", bad: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseHours(c.spec)
			if c.bad {
				if err == nil {
					t.Fatalf("ParseHours(%q) 应报错，得到 %v", c.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseHours(%q): %v", c.spec, err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("ParseHours(%q)=%v want %v", c.spec, got, c.want)
			}
			for h := range c.want {
				if !got[h] {
					t.Fatalf("小时 %d 缺失：%v", h, got)
				}
			}
		})
	}
}

func TestAllowedHourBoundaries(t *testing.T) {
	hours := mustHours(t, "0-9,12-14,18-23")
	cases := []struct {
		hour int
		want bool
	}{
		{0, true}, {1, true}, {9, true}, {10, false}, {11, false},
		{12, true}, {13, true}, {14, true}, {15, false}, {17, false},
		{18, true}, {23, true},
	}
	for _, c := range cases {
		at := time.Date(2026, 9, 10, c.hour, 0, 0, 0, cst)
		if got := AllowedHour(at, cst, hours); got != c.want {
			t.Errorf("AllowedHour(hour=%d)=%v want %v", c.hour, got, c.want)
		}
	}
}

func TestShouldRunHalfHourlyOnceAndCrossDay(t *testing.T) {
	hours := mustHours(t, "0-8,12-13,18-23")
	at := func(day, hour, min int) time.Time {
		return time.Date(2026, 9, day, hour, min, 0, 0, cst)
	}
	cases := []struct {
		name    string
		now     time.Time
		lastKey string
		want    bool
	}{
		// 高峰窗口（9-12、14-18）一律不跑，即使从未跑过。
		{"高峰期9点不跑", at(10, 9, 0), "", false},
		{"高峰期11:59不跑", at(10, 11, 59), "2026-09-10T08:30", false},
		{"高峰期14点不跑", at(10, 14, 0), "", false},
		{"高峰期17:30不跑", at(10, 17, 30), "2026-09-10T13:30", false},
		// 非高峰窗口内按 30 分钟槽执行。
		{"0点首次跑", at(10, 0, 5), "", true},
		{"同一槽不重跑", at(10, 0, 29), "2026-09-10T00:00", false},
		{"下一个槽（0:30）跑", at(10, 0, 31), "2026-09-10T00:00", true},
		{"8:30 是本窗口最后一个槽", at(10, 8, 30), "2026-09-10T08:00", true},
		{"8:59 已跑则不重跑", at(10, 8, 59), "2026-09-10T08:30", false},
		{"12:00 峰后第一槽跑", at(10, 12, 0), "2026-09-10T08:30", true},
		{"12:30 跑", at(10, 12, 30), "2026-09-10T12:00", true},
		{"13:30 跑", at(10, 13, 30), "2026-09-10T13:00", true},
		{"18:00 峰后第一槽跑", at(10, 18, 0), "2026-09-10T13:30", true},
		{"23:30 跑", at(10, 23, 59), "2026-09-10T18:00", true},
		{"跨天23:30→次日0:00 仍跑", at(11, 0, 5), "2026-09-10T23:30", true},
		{"跨天0:30 槽已跑则不重跑", at(11, 0, 40), "2026-09-11T00:30", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldRun(c.now, cst, hours, c.lastKey); got != c.want {
				t.Fatalf("ShouldRun(%s, last=%q)=%v want %v", c.now.Format(time.RFC3339), c.lastKey, got, c.want)
			}
		})
	}
}

func TestNextRun(t *testing.T) {
	hours := mustHours(t, "0-8,12-13,18-23")
	at := func(day, hour, min int) time.Time { return time.Date(2026, 9, day, hour, min, 0, 0, cst) }
	cases := []struct {
		name  string
		after time.Time
		want  time.Time
	}{
		{"槽内推进到下一个槽", at(10, 8, 10), at(10, 8, 30)},
		{"8:30 之后进高峰→12:00", at(10, 8, 30), at(10, 12, 0)},
		{"高峰期 9:00→12:00", at(10, 9, 0), at(10, 12, 0)},
		{"12:00→12:30", at(10, 12, 0), at(10, 12, 30)},
		{"13:30 之后进高峰→18:00", at(10, 13, 30), at(10, 18, 0)},
		{"高峰期 15:00→18:00", at(10, 15, 0), at(10, 18, 0)},
		{"23:30 之后→次日 0:00", at(10, 23, 30), at(11, 0, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NextRun(c.after, cst, hours)
			if !got.Equal(c.want) {
				t.Fatalf("NextRun(%s)=%s want %s",
					c.after.Format(time.RFC3339), got.Format(time.RFC3339), c.want.Format(time.RFC3339))
			}
		})
	}
}

// SlotKey 是 (日期,小时,30分钟槽) 键——同槽相同、跨槽/跨天不同。
func TestSlotKey(t *testing.T) {
	a := time.Date(2026, 9, 10, 23, 1, 0, 0, cst)
	b := time.Date(2026, 9, 10, 23, 29, 59, 0, cst)
	c := time.Date(2026, 9, 10, 23, 30, 0, 0, cst)
	d := time.Date(2026, 9, 11, 0, 0, 0, 0, cst)
	if SlotKey(a, cst) != SlotKey(b, cst) {
		t.Fatal("同一 30 分钟槽应同键")
	}
	if SlotKey(a, cst) == SlotKey(c, cst) {
		t.Fatal("跨槽应不同键")
	}
	if SlotKey(a, cst) == SlotKey(d, cst) {
		t.Fatal("跨天应不同键")
	}
}
