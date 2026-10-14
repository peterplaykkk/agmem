package bm25

// bm25 稀疏向量编码测试（表驱动：正常/错误/边界）。
// FNV-1a 期望值用独立实现（Python）交叉计算后钉死，防哈希实现漂移。

import (
	"math"
	"reflect"
	"testing"
)

func TestTerms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		// 正常路径
		{name: "中文三字滑窗2gram", in: "怎么推代码", want: []string{"怎么", "么推", "推代", "代码"}},
		{name: "中英混合", in: "怎么推代码 gitlab", want: []string{"怎么", "么推", "推代", "代码", "gitlab"}},
		{name: "纯ASCII整词", in: "gitlab", want: []string{"gitlab"}},
		{name: "ASCII大小写归一", in: "GitLab Runner", want: []string{"gitlab", "runner"}},
		{name: "ASCII字母数字混排", in: "pub/agmem:v0.3", want: []string{"pub", "agmem", "v0", "3"}},
		{name: "CJK与ASCII无空格边界", in: "docker部署", want: []string{"docker", "部署"}},
		// 边界路径
		{name: "单字CJK自成词元", in: "码", want: []string{"码"}},
		{name: "空串", in: "", want: nil},
		{name: "纯空白", in: "  \n\t", want: nil},
		{name: "纯标点", in: "。，！？;;", want: nil},
		{name: "标点隔开CJK各自成run", in: "部署。部署，部署", want: []string{"部署", "部署", "部署"}},
		{name: "emoji作分隔不产词元", in: "ok🚀上线", want: []string{"ok", "上线"}},
		{name: "奇偶混合run", in: "五笔输入法", want: []string{"五笔", "笔输", "输入", "入法"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Terms(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Terms(%q) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}

func TestSparseVector(t *testing.T) {
	t.Run("中文2gram命中已知FNV向量", func(t *testing.T) {
		// FNV-1a 32bit 独立实现交叉计算（Python）：推代=2696409119 代码=4244225397
		idx, vals := SparseVector("推代码")
		if !reflect.DeepEqual(idx, []uint32{2696409119, 4244225397}) {
			t.Errorf("indices = %v, want [2696409119 4244225397]（升序 FNV-1a）", idx)
		}
		for i, v := range vals {
			if v != 1.0 {
				t.Errorf("values[%d] = %v, want 1.0（tf=1 饱和变换恒等）", i, v)
			}
		}
	})

	t.Run("重复term按TF饱和聚合", func(t *testing.T) {
		// 部署。部署，部署 → 部署×3；tf=3 → 3*2.2/4.2 ≈ 1.5714286
		idx, vals := SparseVector("部署。部署，部署")
		if len(idx) != 1 || idx[0] != 2706464831 {
			t.Fatalf("indices = %v, want [2706464831]", idx)
		}
		if math.Abs(float64(vals[0])-1.5714285714) > 1e-6 {
			t.Errorf("value = %v, want 3*2.2/4.2=1.5714286", vals[0])
		}
	})

	t.Run("大小写归一后聚合", func(t *testing.T) {
		idx, vals := SparseVector("GitLab gitlab")
		if len(idx) != 1 || idx[0] != 3878867978 {
			t.Errorf("indices = %v, want 单词元 [3878867978]", idx)
		}
		// tf=2 → 2*(k1+1)/(2+k1) = 4.4/3.2 = 1.375
		if math.Abs(float64(vals[0])-1.375) > 1e-6 {
			t.Errorf("value = %v, want tf=2 → 4.4/3.2=1.375", vals[0])
		}
	})

	t.Run("确定性：同输入两次编码一致", func(t *testing.T) {
		in := "混合检索 hybrid RRF 融合 2026"
		i1, v1 := SparseVector(in)
		i2, v2 := SparseVector(in)
		if !reflect.DeepEqual(i1, i2) || !reflect.DeepEqual(v1, v2) {
			t.Errorf("编码不确定：\n%v %v\n%v %v", i1, v1, i2, v2)
		}
	})

	t.Run("空输入返回空", func(t *testing.T) {
		for _, in := range []string{"", "  ", "。。；"} {
			idx, vals := SparseVector(in)
			if len(idx) != 0 || len(vals) != 0 {
				t.Errorf("SparseVector(%q) = %v %v, want 空", in, idx, vals)
			}
		}
	})

	t.Run("不变量：indices升序唯一且values对齐非零", func(t *testing.T) {
		// qdrant SparseVector 契约（OpenAPI）："Indices must be unique"。
		in := "agmem 混合检索 sparse bm25 稀疏向量 混合检索 agmem 回填 sparse"
		idx, vals := SparseVector(in)
		if len(idx) == 0 || len(idx) != len(vals) {
			t.Fatalf("indices/values 长度失配: %d/%d", len(idx), len(vals))
		}
		for i := 1; i < len(idx); i++ {
			if idx[i-1] >= idx[i] {
				t.Fatalf("indices 未严格升序: %v", idx)
			}
		}
		for i, v := range vals {
			if v <= 0 || v > K1+1 {
				t.Errorf("values[%d]=%v 越界 (0, k1+1=%v)", i, v, K1+1)
			}
		}
	})
}
