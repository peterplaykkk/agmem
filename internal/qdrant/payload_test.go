package qdrant

// payload wire 契约（G13 / test/agmem/uc003-clean-quality C1）：
//   - clean 点恒带 entry_index（含 0）与 entry_count；
//   - original 点恒不带 entry_index/entry_count。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPointPayloadEntryFieldsWireContract(t *testing.T) {
	cases := []struct {
		name    string
		payload PointPayload
		wantIdx string // 空 = 断言不得出现
		wantCnt string
	}{
		{"clean idx0 必带 0", PointPayload{PointType: "clean", EntryIndex: 0, EntryCount: 1},
			`"entry_index":0`, `"entry_count":1`},
		{"clean idx3", PointPayload{PointType: "clean", EntryIndex: 3, EntryCount: 5},
			`"entry_index":3`, `"entry_count":5`},
		{"original 不得带", PointPayload{PointType: "original"}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := string(b)
			for _, want := range []string{c.wantIdx, c.wantCnt} {
				if want == "" {
					continue
				}
				if !strings.Contains(got, want) {
					t.Errorf("payload %s 缺 %s", got, want)
				}
			}
			if c.wantIdx == "" {
				if strings.Contains(got, "entry_index") || strings.Contains(got, "entry_count") {
					t.Errorf("original 点不得带 entry 字段，got %s", got)
				}
			}
		})
	}
}

// Upsert 走 Point.MarshalJSON（payload 内嵌），确认契约在真实 wire 路径上生效。
func TestPointMarshalCarriesCleanEntryIndexZero(t *testing.T) {
	b, err := json.Marshal(Point{
		ID:      "p1",
		Vector:  []float32{1, 2},
		Payload: PointPayload{Dir: "k8s-clean", PointType: "clean", ItemID: 7, EntryIndex: 0, EntryCount: 2},
	})
	if err != nil {
		t.Fatalf("marshal point: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"entry_index":0`) || !strings.Contains(got, `"entry_count":2`) {
		t.Errorf("clean 点 wire 缺 entry 字段: %s", got)
	}
}

func TestPointMarshalOmitsOriginalEntryFields(t *testing.T) {
	b, err := json.Marshal(Point{
		ID:      "p2",
		Vector:  []float32{1},
		Payload: PointPayload{Dir: "k8s", PointType: "original", ItemID: 7},
	})
	if err != nil {
		t.Fatalf("marshal point: %v", err)
	}
	if got := string(b); strings.Contains(got, "entry_index") || strings.Contains(got, "entry_count") {
		t.Errorf("original 点 wire 不得带 entry 字段: %s", got)
	}
}
