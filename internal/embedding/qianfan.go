// Package embedding：embeddings 客户端（qwen3-embedding-0.6b，1024d）。
//
// 主通道：aigateway（OpenAI 兼容 POST {AIGATEWAY_BASE_URL}/embeddings，Bearer AIGATEWAY_TOKEN）。
// fallback：千帆直连 POST https://qianfan.baidubce.com/v2/embeddings（Bearer QIANFAN_API_KEY）。
// 主通道失败（网络/429/5xx）自动切换 fallback；两边都失败才返回错误。
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"
)

const (
	// Dim qwen3-embedding-0.6b 输出维度。
	Dim = 1024
)

// Client embedding 客户端（主通道 aigateway + fallback 千帆直连）。
type Client struct {
	// 主通道（OpenAI 兼容）：POST {AIGatewayURL}/embeddings
	AIGatewayURL string // https://ai-gateway.example.com/v1
	AIGatewayKey string
	// fallback（千帆 v2）：POST {BaseURL}/v2/embeddings
	BaseURL string // https://qianfan.baidubce.com
	APIKey  string
	Model   string // qwen3-embedding-0.6b
	Batch   int
	Retries int // 429/5xx 网络错误重试次数
	HTTP    *http.Client
	// QueryInstruction 查询侧指令前缀（Qwen3-Embedding 非对称检索约定：查询侧加
	// Instruct/Query 前缀，文档/写入侧不加）。空串 = 禁用（EmbedQuery 直通 Embed）。
	// 由服务端装配处注入（config.QueryInstruction）；默认值见 config.DefaultQueryInstruction。
	QueryInstruction string
}

// New 创建客户端：aigateway 主通道（token 非空时启用），千帆 fallback（key 非空时启用）。
func New(aiGWURL, aiGWKey, qfURL, qfKey, model string, batch, retries int) *Client {
	if qfURL == "" {
		qfURL = "https://qianfan.baidubce.com"
	}
	if model == "" {
		model = "qwen3-embedding-0.6b"
	}
	if batch <= 0 {
		batch = 16
	}
	if retries <= 0 {
		retries = 3
	}
	return &Client{
		AIGatewayURL: aiGWURL,
		AIGatewayKey: aiGWKey,
		BaseURL:      qfURL,
		APIKey:       qfKey,
		Model:        model,
		Batch:        batch,
		Retries:      retries,
		HTTP:         &http.Client{Timeout: 30 * time.Second},
	}
}

// NewQianfan 兼容旧签名：仅千帆直连（测试用）。
func NewQianfan(baseURL, apiKey, model string, batch, retries int) *Client {
	return New("", "", baseURL, apiKey, model, batch, retries)
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ErrRateLimited / ErrUnauthorized：调用方按类处理（429 退避重试，401 不重试）。
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("embedding: status %d: %s", e.StatusCode, e.Body)
}

// IsRateLimited 判断是否 429。
func IsRateLimited(err error) bool {
	he, ok := err.(*HTTPError)
	return ok && he.StatusCode == http.StatusTooManyRequests
}

// IsUnauthorized 判断是否 401（key 失效，不重试）。
func IsUnauthorized(err error) bool {
	he, ok := err.(*HTTPError)
	return ok && he.StatusCode == http.StatusUnauthorized
}

// BatchSize 按客户端配置切批。
func (c *Client) BatchSize() int { return c.Batch }

// EmbedBatch 对一批文本取向量，保持输入顺序。
// 主通道 aigateway 一次尝试；失败（含 401/429/网络）降级千帆直连（带退避重试）。
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > c.Batch {
		return nil, fmt.Errorf("embedding: batch size %d exceeds limit %d", len(texts), c.Batch)
	}
	if c.AIGatewayKey != "" {
		body, err := json.Marshal(embedRequest{Model: c.Model, Input: texts})
		if err != nil {
			return nil, err
		}
		out, err := c.postAt(ctx, c.AIGatewayURL+"/embeddings", c.AIGatewayKey, body)
		if err == nil {
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// 主通道失败 → 降级 fallback（日志留给调用方/worker 层打印可改；此处静默降级）
	}
	if c.APIKey == "" {
		if c.AIGatewayKey == "" {
			return nil, fmt.Errorf("embedding: 无可用通道（AIGATEWAY_TOKEN 与 QIANFAN_API_KEY 均为空）")
		}
		return nil, fmt.Errorf("embedding: aigateway 主通道失败且未配置千帆 fallback")
	}
	body, err := json.Marshal(embedRequest{Model: c.Model, Input: texts})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt); err != nil {
				return nil, err
			}
		}
		out, err := c.postAt(ctx, c.BaseURL+"/v2/embeddings", c.APIKey, body)
		if err == nil {
			return out, nil
		}
		lastErr = err
		// 401 是 key 问题，重试无意义；429 与网络/5xx 退避后重试
		if IsUnauthorized(err) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// Embed 分批并发嵌入任意数量文本，返回顺序与输入一致。
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	var batches [][]string
	for i := 0; i < len(texts); i += c.Batch {
		end := i + c.Batch
		if end > len(texts) {
			end = len(texts)
		}
		batches = append(batches, texts[i:end])
	}
	out := make([][]float32, len(texts))
	type result struct {
		start int
		vecs  [][]float32
		err   error
	}
	ch := make(chan result, len(batches))
	for bi, b := range batches {
		go func(bi int, b []string) {
			vecs, err := c.EmbedBatch(ctx, b)
			ch <- result{start: bi * c.Batch, vecs: vecs, err: err}
		}(bi, b)
	}
	var firstErr error
	for range batches {
		r := <-ch
		if r.err != nil && firstErr == nil {
			firstErr = r.err
			continue
		}
		copy(out[r.start:], r.vecs)
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// EmbedQuery 查询侧嵌入：QueryInstruction 非空时给每个文本前置指令前缀后调
// Embed；空指令 = 禁用，直通 Embed（与存量纯稠密行为一致）。
//
// 非对称检索语义（Qwen3-Embedding）：指令只加在查询侧，文档/写入侧一律走
// Embed 不加前缀——两侧都加会破坏训练时的不对称约定。故本方法是查询路径唯一
// 入口，worker/ingest 继续用 Embed。
func (c *Client) EmbedQuery(ctx context.Context, texts []string) ([][]float32, error) {
	if c.QueryInstruction == "" {
		return c.Embed(ctx, texts)
	}
	prefixed := make([]string, len(texts))
	for i, t := range texts {
		prefixed[i] = c.QueryInstruction + t
	}
	return c.Embed(ctx, prefixed)
}

func (c *Client) postAt(ctx context.Context, url, key string, body []byte) ([][]float32, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err // 网络错误 → 可重试
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(b)}
	}
	var er embedResponse
	if err := json.Unmarshal(b, &er); err != nil {
		return nil, fmt.Errorf("embedding: decode: %w", err)
	}
	if len(er.Data) == 0 {
		return nil, fmt.Errorf("embedding: empty data: %s", string(b))
	}
	out := make([][]float32, len(er.Data))
	for _, d := range er.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embedding: index out of range: %d", d.Index)
		}
		if len(d.Embedding) != Dim {
			return nil, fmt.Errorf("embedding: dim mismatch: got %d want %d", len(d.Embedding), Dim)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

// sleepBackoff 指数退避 + 抖动：200ms, 400ms, 800ms...
func sleepBackoff(ctx context.Context, attempt int) error {
	d := time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond
	d += time.Duration(rand.Int63n(int64(d/4 + 1)))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
