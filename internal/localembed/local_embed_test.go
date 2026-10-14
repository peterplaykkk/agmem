package localembed

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sre/agmem/internal/cache"
)

// 真实 onnxruntime 推理测试：库/模型缺失时 skip（CI 友好）。
// 跑通方式见 README（brew install onnxruntime + 首次启动下载模型）。

func ortLibAvailable(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("AGMEMD_ORT_LIB"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		t.Skipf("AGMEMD_ORT_LIB=%s 不存在，跳过 onnxruntime 测试", p)
	}
	name := "libonnxruntime.dylib"
	home, _ := os.UserHomeDir()
	for _, dir := range []string{
		filepath.Join(home, ".cache", "agmemd", "lib"),
		"/opt/homebrew/lib",
		"/usr/local/lib",
	} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("onnxruntime 库未找到（brew install onnxruntime 或设置 AGMEMD_ORT_LIB），跳过")
	return ""
}

func TestEngine_SemanticSimilarity(t *testing.T) {
	lib := ortLibAvailable(t)
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cache", "agmemd")
	cfg := EmbedConfig{
		CacheDir: dir,
		ORTLib:   lib,
		EP:       "cpu",
		MaxBytes: 200 << 20,
	}
	e := NewEngine(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := e.Load(ctx); err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "download") {
			t.Skipf("模型未就绪（首次启动会下载）： %v", err)
		}
		t.Fatalf("Load: %v", err)
	}
	vec, err := e.Embed("qdrant collection 维度是多少")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != EmbedDim {
		t.Fatalf("want %d dims, got %d", EmbedDim, len(vec))
	}
	var norm float64
	for _, f := range vec {
		norm += float64(f) * float64(f)
	}
	if abs(norm-1) > 1e-3 {
		t.Fatalf("vector not L2 normalized: norm=%f", norm)
	}

	vec2, err := e.Embed("qdrant 的 collection 是多少维")
	if err != nil {
		t.Fatalf("Embed 2: %v", err)
	}
	vecUnrelated, err := e.Embed("今天晚饭吃什么好呢")
	if err != nil {
		t.Fatalf("Embed unrelated: %v", err)
	}
	vecParaphrase, _ := e.Embed("怎么给k8s节点加污点")
	vecTaint, _ := e.Embed("k8s 节点如何打 taint")
	simClose := cache.Cosine(vec, vec2)
	simFar := cache.Cosine(vec, vecUnrelated)
	simTaint := cache.Cosine(vecTaint, vecParaphrase)
	t.Logf("强近义对相似度 = %.4f；弱近义对(taint) = %.4f；无关对相似度 = %.4f",
		simClose, simTaint, simFar)
	if simClose <= 0.8 {
		t.Errorf("强近义句相似度应 >0.8，实际 %.4f", simClose)
	}
	if simFar >= 0.5 {
		t.Errorf("无关句相似度应 <0.5，实际 %.4f", simFar)
	}
	// 已知边界（实测 2026-09-04，bge-base-zh-v1.5 int8，Mac CPU）：
	// 弱近义短问句对约 0.71（<0.8 阈值 → 走云端，安全兜底）。
	// 阈值可用 AGMEMD_SIM_THRESHOLD 调整；此断言防止引擎劣化无感知。
	if simTaint < 0.5 {
		t.Errorf("弱近义句相似度应 >0.5，实际 %.4f", simTaint)
	}
}

func TestEngine_NotReady(t *testing.T) {
	e := NewEngine(EmbedConfig{CacheDir: t.TempDir()})
	if _, err := e.Embed("x"); err != ErrModelUnavailable {
		t.Fatalf("want ErrModelUnavailable, got %v", err)
	}
}

func TestORTLibPath_Missing(t *testing.T) {
	// 本机若已装 onnxruntime（brew），默认搜索路径能找到 → 跳过负向断言
	if _, err := ortLibPath(EmbedConfig{CacheDir: t.TempDir()}); err == nil {
		t.Skip("本机 onnxruntime 库存在（默认搜索路径可命中），跳过 missing 断言")
	}
	_, err := ortLibPath(EmbedConfig{CacheDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "onnxruntime") {
		t.Fatalf("want onnxruntime unavailable error, got %v", err)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// l2normalize 随本地引擎迁入本包（原 cache.TestL2Normalize）。
func TestL2Normalize(t *testing.T) {
	v := []float32{3, 4}
	l2normalize(v)
	if abs(cache.Cosine(v, v)-1) > 1e-6 {
		t.Fatalf("not normalized: %v", v)
	}
}

// Embed 只跑有效 token 长度（attn mask 非 0 计数），不再恒定 512 步；此纯函数
// 是耗时优化与正确性的关键点，边界须覆盖。
func TestActualSeqLen(t *testing.T) {
	cases := []struct {
		name string
		mask []int64
		want int
	}{
		{"全padding兜底为1", []int64{0, 0, 0}, 1},
		{"空切片兜底为1", []int64{}, 1},
		{"含CLS与SEP", []int64{1, 1, 1, 0, 0}, 3},
		{"满长512", func() []int64 {
			m := make([]int64, 512)
			for i := range m {
				m[i] = 1
			}
			return m
		}(), 512},
		{"单token", []int64{1}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := actualSeqLen(c.mask); got != c.want {
				t.Fatalf("actualSeqLen=%d, want %d", got, c.want)
			}
		})
	}
}
