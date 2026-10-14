package localembed

// 本地 embedding 引擎：bge-base-zh ONNX + onnxruntime（cgo 库运行时加载，不进 git）。
//
// - 模型：默认 BAAI/bge-base-zh（768d），首次启动从 HF 镜像（hf-mirror.com）下载
//   ONNX 权重 + vocab.txt 到 ~/.cache/agmemd/models/；走 HTTP(S)_PROXY 环境变量。
//   默认权重文件 model_quantized.onnx（int8，单文件 <200MB）；可用
//   LOCAL_EMBED_MODEL_URL 换 fp32。
// - EP：AGMEMD_EP = cpu（默认）| coreml（Mac，失败自动回退 cpu 并 warn）
//   | cuda | rocm（stub，未实现）。
// - 输出 768d，L2 归一化。

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

const (
	// EmbedDim bge-base-zh 输出维度。
	EmbedDim = 768

	defaultModelURL = "https://hf-mirror.com/Xenova/bge-base-zh-v1.5/resolve/main/onnx/model_quantized.onnx"
	defaultVocabURL = "https://hf-mirror.com/Xenova/bge-base-zh-v1.5/resolve/main/vocab.txt"
)

// EmbedConfig 本地引擎配置。
type EmbedConfig struct {
	CacheDir string // ~/.cache/agmemd
	ModelURL string // 为空用默认
	VocabURL string // 为空用默认
	ORTLib   string // onnxruntime 共享库路径；为空按平台默认名在 CacheDir/lib 与系统路径找
	EP       string // cpu|coreml|cuda|rocm
	MaxBytes int64  // 模型下载大小上限（防御）
}

// DefaultEmbedConfig 环境变量 → 配置。
func DefaultEmbedConfig(cacheDir string) EmbedConfig {
	return EmbedConfig{
		CacheDir: cacheDir,
		ModelURL: os.Getenv("LOCAL_EMBED_MODEL_URL"),
		VocabURL: os.Getenv("LOCAL_EMBED_VOCAB_URL"),
		ORTLib:   os.Getenv("AGMEMD_ORT_LIB"),
		EP:       strings.ToLower(os.Getenv("AGMEMD_EP")),
		MaxBytes: 200 << 20,
	}
}

// Engine 本地 embedding 引擎。
type Engine struct {
	cfg  EmbedConfig
	mu   sync.Mutex
	sess *ort.DynamicAdvancedSession
	tok  *Tokenizer
	// 输出池化方式
	poolCLS bool
	outDim  int64
}

// NewEngine 创建引擎（不加载；Load 完成后 Ready）。
func NewEngine(cfg EmbedConfig) *Engine {
	if cfg.EP == "" {
		cfg.EP = "cpu"
	}
	return &Engine{cfg: cfg}
}

// Ready 是否已加载可用。
func (e *Engine) Ready() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sess != nil && e.tok != nil
}

// ortLibPath 解析 onnxruntime 共享库路径；找不到返回 ErrORTUnavailable。
func ortLibPath(cfg EmbedConfig) (string, error) {
	if cfg.ORTLib != "" {
		if _, err := os.Stat(cfg.ORTLib); err == nil {
			return cfg.ORTLib, nil
		}
		return "", fmt.Errorf("%w: AGMEMD_ORT_LIB=%s 不存在", ErrORTUnavailable, cfg.ORTLib)
	}
	name := "libonnxruntime.so"
	if runtime.GOOS == "darwin" {
		name = "libonnxruntime.dylib"
	}
	for _, dir := range []string{
		filepath.Join(cfg.CacheDir, "lib"),
		"/opt/homebrew/lib", // macOS brew
		"/usr/local/lib",    // linux/Intel mac 常见
		".",
	} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: 未找到 %s（放到 %s/lib/ 或用 AGMEMD_ORT_LIB 指定；见 README）",
		ErrORTUnavailable, name, cfg.CacheDir)
}

// EnsureModel 下载并校验模型与 vocab（已存在则跳过）。
func (e *Engine) EnsureModel(ctx context.Context) error {
	modelPath, vocabPath, err := e.modelPaths()
	if err != nil {
		return err
	}
	modelURL := e.cfg.ModelURL
	if modelURL == "" {
		modelURL = defaultModelURL
	}
	vocabURL := e.cfg.VocabURL
	if vocabURL == "" {
		vocabURL = defaultVocabURL
	}
	if err := downloadFile(ctx, modelURL, modelPath, e.cfg.MaxBytes); err != nil {
		return fmt.Errorf("download model: %w", err)
	}
	if err := downloadFile(ctx, vocabURL, vocabPath, 8<<20); err != nil {
		return fmt.Errorf("download vocab: %w", err)
	}
	// 校验：vocab 必须含特殊 token
	vb, err := os.ReadFile(vocabPath)
	if err != nil {
		return err
	}
	for _, tok := range []string{"[CLS]", "[SEP]", "[UNK]", "[PAD]"} {
		if !strings.Contains(string(vb), tok+"\n") && !strings.Contains(string(vb), tok+"\r") {
			return fmt.Errorf("vocab 校验失败：缺少 %s", tok)
		}
	}
	// 模型文件非空即通过深度校验（真正校验在 Load 建 session 时）
	st, err := os.Stat(modelPath)
	if err != nil {
		return err
	}
	if st.Size() < 1<<20 {
		return fmt.Errorf("模型文件过小（%d bytes），疑似下载失败", st.Size())
	}
	return nil
}

func (e *Engine) modelPaths() (string, string, error) {
	dir := filepath.Join(e.cfg.CacheDir, "models")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "model.onnx"), filepath.Join(dir, "vocab.txt"), nil
}

// Load 初始化 onnxruntime + session + tokenizer。
func (e *Engine) Load(ctx context.Context) error {
	if err := e.EnsureModel(ctx); err != nil {
		return err
	}
	modelPath, vocabPath, _ := e.modelPaths()
	libPath, err := ortLibPath(e.cfg)
	if err != nil {
		return err
	}
	vb, err := os.ReadFile(vocabPath)
	if err != nil {
		return err
	}
	tok, err := NewTokenizer(string(vb))
	if err != nil {
		return err
	}
	// EP 选择：cpu 默认；coreml best-effort；cuda/rocm stub。
	// 初始化 onnxruntime 运行时（SetSharedLibraryPath 必须在最先）。
	ensureEnv := func() error {
		if ort.IsInitialized() {
			return nil
		}
		ort.SetSharedLibraryPath(libPath)
		return ort.InitializeEnvironment()
	}
	if err := ensureEnv(); err != nil {
		return fmt.Errorf("init onnxruntime (%s): %w", libPath, err)
	}
	// 模型输入输出探测（需环境已初始化）
	inputs, outputs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return fmt.Errorf("probe model: %w", err)
	}
	var inNames, outNames []string
	for _, i := range inputs {
		inNames = append(inNames, i.Name)
	}
	for _, o := range outputs {
		outNames = append(outNames, o.Name)
	}
	if len(outNames) == 0 {
		return fmt.Errorf("model has no outputs")
	}
	// 池化：优先 pooler_output，否则取 last_hidden_state 的 [CLS]
	e.poolCLS = true
	e.outDim = outputs[0].Dimensions[2]
	for _, o := range outputs {
		if o.Name == "pooler_output" {
			e.poolCLS = false
			e.outDim = o.Dimensions[1]
		}
		if o.Name == "last_hidden_state" {
			e.outDim = o.Dimensions[2]
		}
	}

	buildSession := func(withEP bool) (*ort.DynamicAdvancedSession, error) {
		var opts *ort.SessionOptions
		if withEP {
			opts, err = ort.NewSessionOptions()
			if err != nil {
				return nil, err
			}
			defer opts.Destroy()
			switch e.cfg.EP {
			case "coreml":
				// flags=0：默认（CPU fallback 允许）
				if err := opts.AppendExecutionProviderCoreML(0); err != nil {
					return nil, fmt.Errorf("coreml EP: %w", err)
				}
			case "cuda":
				return nil, fmt.Errorf("EP %q: stub 未实现（待评审，见 README）", e.cfg.EP)
			case "rocm":
				return nil, fmt.Errorf("EP %q: stub 未实现（待评审，见 README）", e.cfg.EP)
			}
		}
		return ort.NewDynamicAdvancedSession(modelPath, inNames, outNames, opts)
	}
	sess, err := buildSession(true)
	if err != nil {
		if e.cfg.EP == "coreml" {
			log.Printf("warn: coreml EP 初始化失败，回退 cpu: %v", err)
			sess, err = buildSession(false)
		}
		if err != nil {
			return err
		}
	}
	e.mu.Lock()
	e.sess = sess
	e.tok = tok
	e.mu.Unlock()
	return nil
}

// Embed 单句 → 768d L2 归一化向量。
func (e *Engine) Embed(text string) ([]float32, error) {
	e.mu.Lock()
	sess, tok := e.sess, e.tok
	poolCLS, outDim := e.poolCLS, e.outDim
	e.mu.Unlock()
	if sess == nil || tok == nil {
		return nil, ErrModelUnavailable
	}
	in := tok.Encode(text)
	// 只跑实际 token 长度：Encode 为定形张量恒定 padding 到 maxLen（512）。attention
	// mask 已把 padding 位标 0，模型对 masked 位置的注意力恒为 0，故把三张量裁到
	// 有效长度后 [CLS] 表示不变（语义等价），而单次前向从 512 步降到实际步数
	// （短问句约 10 步）。实测（buildhost, bge-base-zh int8, cpu）：本地命中耗时
	// 由 ~0.21s 降到 ~0.017s，是达成预检索 <0.1s 的关键（否则 embedding 单项即 ~0.2s）。
	n := actualSeqLen(in.AttentionMask)
	inIDs, err := ort.NewTensor(ort.NewShape(1, int64(n)), in.IDs[:n])
	if err != nil {
		return nil, err
	}
	defer inIDs.Destroy()
	attn, err := ort.NewTensor(ort.NewShape(1, int64(n)), in.AttentionMask[:n])
	if err != nil {
		return nil, err
	}
	defer attn.Destroy()
	ttype, err := ort.NewTensor(ort.NewShape(1, int64(n)), in.TokenTypeIDs[:n])
	if err != nil {
		return nil, err
	}
	defer ttype.Destroy()

	var outShape ort.Shape
	if poolCLS {
		outShape = ort.NewShape(1, int64(n), outDim)
	} else {
		outShape = ort.NewShape(1, outDim)
	}
	out, err := ort.NewEmptyTensor[float32](outShape)
	if err != nil {
		return nil, err
	}
	defer out.Destroy()
	inVals := []ort.Value{inIDs, attn, ttype}
	outVals := []ort.Value{out}
	if err := sess.Run(inVals, outVals); err != nil {
		return nil, fmt.Errorf("onnx run: %w", err)
	}
	var vec []float32
	if poolCLS {
		vec = append([]float32{}, out.GetData()[:outDim]...) // [CLS] 位置=序列第一个 token
	} else {
		vec = append([]float32{}, out.GetData()...)
	}
	l2normalize(vec)
	return vec, nil
}

// actualSeqLen 有效序列长度 = attention mask 中非 0（即 1）的个数，含 [CLS]/[SEP]。
// Encode 产出的 mask 前段连续为 1、padding 段为 0，故计数即真实 token 长度；
// 兜底至少 1（空输入时仍含 [CLS]/[SEP]，不会出现 0）。
func actualSeqLen(mask []int64) int {
	n := 0
	for _, m := range mask {
		if m != 0 {
			n++
		}
	}
	if n == 0 {
		return 1
	}
	return n
}

func l2normalize(v []float32) {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		return
	}
	n := float32(1.0 / math.Sqrt(sum))
	for i := range v {
		v[i] *= n
	}
}

// downloadFile 带进度与大小上限的下载（走 HTTP(S)_PROXY 环境变量，默认 transport）。
func downloadFile(ctx context.Context, url, dest string, maxBytes int64) error {
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		return nil // 已存在
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w（模型下载走 HTTP(S)_PROXY 环境变量）", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	total := resp.ContentLength
	var written int64
	buf := make([]byte, 128<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return werr
			}
			written += int64(n)
			if maxBytes > 0 && written > maxBytes {
				f.Close()
				os.Remove(tmp)
				return fmt.Errorf("download exceeds %d bytes", maxBytes)
			}
			if total > 0 {
				log.Printf("download %s: %d/%d MB (%.0f%%)", filepath.Base(dest),
					written>>20, total>>20, 100*float64(written)/float64(total))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}
