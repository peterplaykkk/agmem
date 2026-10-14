// feishu.go：飞书开放平台只读客户端（自建应用 tenant_access_token + wiki/docx）。
//
// 只用到三条只读接口（自建应用需具备 wiki/drive 读 scope 且已加入目标空间）：
//
//	POST /open-apis/auth/v3/tenant_access_token/internal  app_id/app_secret → token（~2h）
//	GET  /open-apis/wiki/v2/spaces/{space}/nodes          列节点（parent_node_token 翻页）
//	GET  /open-apis/docx/v1/documents/{obj}/raw_content   docx 纯文本正文
//
// 不引第三方飞书 SDK：三条接口手写即可，仓库 vendor 已锁定，新增依赖收益为负。
package wikisync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL 飞书开放平台公网基地址（测试注入 httptest URL 覆盖）。
const DefaultBaseURL = "https://open.feishu.cn"

// tokenEarlyRefresh 令牌提前刷新窗口：到期前 5 分钟即刷新，避免边界 401。
const tokenEarlyRefresh = 5 * time.Minute

// FeishuClient 飞书 wiki/docx 只读客户端（并发安全）。
type FeishuClient struct {
	BaseURL   string
	AppID     string
	AppSecret string
	HTTP      *http.Client
	// Now 注入时钟（测试用）；nil → time.Now。
	Now func() time.Time

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

func (c *FeishuClient) base() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *FeishuClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *FeishuClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Token 返回可用的 tenant_access_token：命中缓存（未进入提前刷新窗口）直接复用，
// 否则调令牌端点换取并缓存。整个方法持锁——单 syncer goroutine 调用，串行无妨。
func (c *FeishuClient) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.tokenExpiry) {
		return c.token, nil
	}
	body, err := json.Marshal(map[string]string{"app_id": c.AppID, "app_secret": c.AppSecret})
	if err != nil {
		return "", fmt.Errorf("wikisync: token encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base()+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("wikisync: token request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("wikisync: token read: %w", err)
	}
	var out struct {
		Code   int    `json:"code"`
		Msg    string `json:"msg"`
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("wikisync: token decode: %w", err)
	}
	if out.Code != 0 || out.Token == "" {
		return "", fmt.Errorf("wikisync: 令牌换取失败 code=%d msg=%s", out.Code, out.Msg)
	}
	ttl := time.Duration(out.Expire) * time.Second
	if ttl <= tokenEarlyRefresh {
		ttl = 2 * time.Hour // expire 异常时按约定 TTL 兜底，避免每次调用都刷新
	}
	c.token = out.Token
	c.tokenExpiry = c.now().Add(ttl - tokenEarlyRefresh)
	return c.token, nil
}

// flexInt64 解析飞书时间戳字段：同一字段既可能给 JSON 字符串也可能给数字。
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("flexInt64: %q: %w", s, err)
	}
	*f = flexInt64(n)
	return nil
}

// Node wiki 节点（仅保留同步需要的字段）。
type Node struct {
	NodeToken   string    `json:"node_token"`
	ObjToken    string    `json:"obj_token"`
	ObjType     string    `json:"obj_type"`
	Title       string    `json:"title"`
	HasChild    bool      `json:"has_child"`
	ObjEditTime flexInt64 `json:"obj_edit_time"`
}

// envelope 飞书统一响应壳（code/msg 在顶层，业务字段在 data）。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// get 发起带 Bearer 的 GET，校验 code，并把 data 反序列化到 out。
func (c *FeishuClient) get(ctx context.Context, apiPath string, q url.Values, out any) error {
	u := c.base() + apiPath
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	tok, err := c.Token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("wikisync: GET %s: %w", apiPath, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("wikisync: GET %s read: %w", apiPath, err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("wikisync: GET %s decode (http %d): %w", apiPath, resp.StatusCode, err)
	}
	if env.Code != 0 {
		return fmt.Errorf("wikisync: GET %s: feishu code=%d msg=%s", apiPath, env.Code, env.Msg)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("wikisync: GET %s data decode: %w", apiPath, err)
		}
	}
	return nil
}

// ListNodes 列出某父节点下的直接子节点（自动按 page_token 翻页）；parentToken 为空
// 表示列顶层节点。返回已取到的全部节点（翻页中途失败时返回部分结果与错误）。
func (c *FeishuClient) ListNodes(ctx context.Context, spaceID, parentToken string) ([]Node, error) {
	var all []Node
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("page_size", "50")
		if parentToken != "" {
			q.Set("parent_node_token", parentToken)
		}
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		var data struct {
			HasMore   bool   `json:"has_more"`
			PageToken string `json:"page_token"`
			Items     []Node `json:"items"`
		}
		if err := c.get(ctx, "/open-apis/wiki/v2/spaces/"+url.PathEscape(spaceID)+"/nodes", q, &data); err != nil {
			return all, err
		}
		all = append(all, data.Items...)
		if !data.HasMore || data.PageToken == "" {
			break
		}
		pageToken = data.PageToken
	}
	return all, nil
}

// RawContent 取 docx 文档纯文本正文（仅 obj_type=docx 有效，其它类型飞书返回错误）。
func (c *FeishuClient) RawContent(ctx context.Context, objToken string) (string, error) {
	var data struct {
		Content string `json:"content"`
	}
	if err := c.get(ctx, "/open-apis/docx/v1/documents/"+url.PathEscape(objToken)+"/raw_content", nil, &data); err != nil {
		return "", err
	}
	return data.Content, nil
}

// SpaceName 取 wiki 空间名（顶层节点的目录名兜底）。best-effort：失败由调用方降级。
func (c *FeishuClient) SpaceName(ctx context.Context, spaceID string) (string, error) {
	var data struct {
		Name string `json:"name"`
	}
	if err := c.get(ctx, "/open-apis/wiki/v2/spaces/"+url.PathEscape(spaceID), nil, &data); err != nil {
		return "", err
	}
	return data.Name, nil
}
