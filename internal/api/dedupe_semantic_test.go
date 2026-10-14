package api

import (
	"testing"

	"sre/agmem/internal/qdrant"
)

// 语义折叠：用构造向量验证「同义改写折叠、互补细节不折叠」。
func TestCollapseSemanticDuplicates(t *testing.T) {
	// 三维单位向量便于手算：a 与 b 夹角小（高相似），c 正交（不相似）
	a := []float32{1, 0, 0}
	b := []float32{0.98, 0.199, 0} // cos(a,b) ≈ 0.98
	c := []float32{0, 0, 1}        // cos(a,c)=0
	hits := []qdrant.SearchHit{
		{Score: 0.9, Payload: qdrant.PointPayload{Text: "第一条"}},
		{Score: 0.8, Payload: qdrant.PointPayload{Text: "同义改写"}},
		{Score: 0.7, Payload: qdrant.PointPayload{Text: "互补内容"}},
	}
	kept, dropped := collapseSemanticDuplicates(hits, [][]float32{a, b, c}, 0.88)
	if len(kept) != 2 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d, want 2/1", len(kept), dropped)
	}
	// 折叠时保留**正文更长**的那条（同组内更详尽的版本），位置仍是原排序位置。
	// "同义改写"(4字) 比 "第一条"(3字) 长 → 替换掉先出现的那条。
	if kept[0].Payload.Text != "同义改写" || kept[1].Payload.Text != "互补内容" {
		t.Fatalf("保留项错误: %s / %s", kept[0].Payload.Text, kept[1].Payload.Text)
	}
	// 更长的那条若先出现，则保持不动（不会被更短的替换）
	kept2, _ := collapseSemanticDuplicates([]qdrant.SearchHit{
		{Score: 0.9, Payload: qdrant.PointPayload{Text: "更详尽的第一条说明"}},
		{Score: 0.8, Payload: qdrant.PointPayload{Text: "短版"}},
	}, [][]float32{a, b}, 0.88)
	if kept2[0].Payload.Text != "更详尽的第一条说明" {
		t.Fatalf("长版在前时不应被替换: %s", kept2[0].Payload.Text)
	}
}

// 阈值边界与降级：阈值 0 关闭；向量条数不符时原样返回（调用方错误不误折）。
func TestCollapseSemanticDuplicates_Guards(t *testing.T) {
	hits := []qdrant.SearchHit{
		{Score: 0.9, Payload: qdrant.PointPayload{Text: "a"}},
		{Score: 0.8, Payload: qdrant.PointPayload{Text: "b"}},
	}
	if kept, d := collapseSemanticDuplicates(hits, [][]float32{{1, 0}, {1, 0}}, 0); len(kept) != 2 || d != 0 {
		t.Fatal("阈值 0 应关闭语义折叠")
	}
	if kept, d := collapseSemanticDuplicates(hits, [][]float32{{1, 0}}, 0.88); len(kept) != 2 || d != 0 {
		t.Fatal("向量条数不符时应原样返回")
	}
	if kept, d := collapseSemanticDuplicates(hits, nil, 0.88); len(kept) != 2 || d != 0 {
		t.Fatal("无向量时应原样返回")
	}
	// 零向量不得导致除零 panic，且不与任何东西相似
	if kept, d := collapseSemanticDuplicates(
		[]qdrant.SearchHit{{Payload: qdrant.PointPayload{Text: "x"}}, {Payload: qdrant.PointPayload{Text: "y"}}},
		[][]float32{{0, 0}, {1, 0}}, 0.88); len(kept) != 2 || d != 0 {
		t.Fatal("零向量不应折叠")
	}
}

// 与字面折叠叠加时，两层的折叠数相加（filter.deduped 语义）。
func TestDedupeLayersAddUp(t *testing.T) {
	base := "统一出口代理走 proxy.example.com:3128，仅白名单域名可用。"
	lit, litDropped := collapseNearDuplicates([]qdrant.SearchHit{
		{Score: 0.9, Payload: qdrant.PointPayload{Text: base}},
		{Score: 0.8, Payload: qdrant.PointPayload{Text: base}}, // 字面重复
		{Score: 0.7, Payload: qdrant.PointPayload{Text: "同义改写的另一条。"}},
	})
	if litDropped != 1 {
		t.Fatalf("字面层应折 1 条, got %d", litDropped)
	}
	_, semDropped := collapseSemanticDuplicates(lit,
		[][]float32{{1, 0, 0}, {0.99, 0.14, 0}}, 0.88)
	if litDropped+semDropped != 2 {
		t.Fatalf("两层合计应为 2, got %d", litDropped+semDropped)
	}
}

// 三条同组、依次更长：最终必须留下最长的那条。
// 这条用例专门抓一个真实 bug——替换后若仍拿「原始下标对应的那条」比长度，
// 第二次替换不会生效，最短的会侥幸留下（线上 580 字段落被 353 字段住）。
func TestCollapseSemanticDuplicates_KeepsLongestAcrossChain(t *testing.T) {
	a := []float32{1, 0}
	// 三者两两余弦都 ≥ 0.9（同组）
	b := []float32{0.95, 0.31}
	c := []float32{0.98, 0.199}
	hits := []qdrant.SearchHit{
		{Score: 0.9, Payload: qdrant.PointPayload{Text: "短"}},                     // 1 字
		{Score: 0.8, Payload: qdrant.PointPayload{Text: "中等长度的第二条"}},              // 8 字
		{Score: 0.7, Payload: qdrant.PointPayload{Text: "最长的一条，包含前面所有要点并补充了细节。"}}, // 22 字
	}
	kept, dropped := collapseSemanticDuplicates(hits, [][]float32{a, b, c}, 0.6)
	if len(kept) != 1 || dropped != 2 {
		t.Fatalf("kept=%d dropped=%d want 1/2", len(kept), dropped)
	}
	want := hits[2].Payload.Text
	if kept[0].Payload.Text != want {
		t.Fatalf("应保留最长的那条, got %q (want %q)", kept[0].Payload.Text, want)
	}
}
