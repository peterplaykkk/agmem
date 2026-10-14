package cleaner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// GLMClient 调 aigateway（OpenAI 兼容 /v1/chat/completions）做规范化清洗。
// model 默认 glm-5.3-flash；thinking 档位可配（medium 对应中等思考，透传给上游）。
//
// 降级链（v5 G11，plan 第 5 条）：primary → 同家族 → 千帆文本模型；think.mode
// 不被上游接受时静默降级默认档（去掉 think 参数重试）。降级事件结构化记录
// （日志 + 可选 OnDegrade 钩子），不阻塞管线。
type GLMClient struct {
	BaseURL  string // https://ai-gateway.example.com/v1
	Model    string
	Thinking string // medium | high | off ...（可配参数）
	Token    string
	HTTP     *http.Client
	// Models 降级链（首位为 primary）；空则由 Model 派生 DefaultModelChain。
	Models []string
	// Probe 开启时调用 GET /models 探测可用模型并据此过滤降级链（探测失败/为空
	// 则退回完整链，不阻塞）。默认由 NewGLMClient 按 AGMEM_GLM_PROBE 决定（默认开）。
	Probe bool
	// OnDegrade 降级事件钩子（可选）；未设置时仅结构化日志。
	OnDegrade func(DegradeEvent)
	// OnUsage 每次成功清洗请求的 usage 回调（可选，S3 观测）。前缀 hash/体积由
	// 调用方（worker）拼接，本回调只透出网关 usage。
	OnUsage func(CleanUsage)

	probeMu  sync.Mutex
	probeSet map[string]bool
	probedAt time.Time
	probeTTL time.Duration
}

// NewGLMClient 创建清洗客户端。
func NewGLMClient(baseURL, model, thinking, token string) *GLMClient {
	g := &GLMClient{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Model:    model,
		Thinking: thinking,
		Token:    token,
		HTTP:     &http.Client{Timeout: glmTimeout()},
		Models:   DefaultModelChain(model),
		Probe:    os.Getenv("AGMEM_GLM_PROBE") != "off",
		probeTTL: 10 * time.Minute,
	}
	g.OnDegrade = g.logDegrade
	return g
}

// DefaultModelChain 构造降级链：primary → 同家族（去 -flash/-air/... 后缀）→
// 千帆文本模型。可用 AGMEM_GLM_FALLBACK_MODELS（逗号分隔）覆盖尾部候选。
func DefaultModelChain(primary string) []string {
	var chain []string
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m == "" {
			return
		}
		for _, c := range chain {
			if c == m {
				return
			}
		}
		chain = append(chain, m)
	}
	add(primary)
	add(sameFamily(primary))
	env := strings.TrimSpace(os.Getenv("AGMEM_GLM_FALLBACK_MODELS"))
	if env != "" {
		for _, m := range strings.Split(env, ",") {
			add(m)
		}
		return chain
	}
	// 默认尾部：同家族显式兜底 + 千帆文本模型（探测不可用时才会真正请求到）。
	add("glm-5.3")
	add("ernie-4.5-turbo-128k")
	return chain
}

// sameFamily 派生同家族模型名：glm-5.3-flash → glm-5.3。无已知轻量后缀返回 ""。
func sameFamily(m string) string {
	for _, suf := range []string{"-flash", "-air", "-plus", "-turbo", "-mini", "-lite"} {
		if strings.HasSuffix(m, suf) {
			return strings.TrimSuffix(m, suf)
		}
	}
	return ""
}

// glmTimeout 返回 GLM 清洗 HTTP 超时，经 AGMEM_GLM_TIMEOUT 配置（Go duration
// 字符串，如 300s、5m）；缺省或非法值回落 300s。GLM 中档思考清洗 4000 字分片
// 实测耗时 2-4 分钟，旧默认 120s 必超时（Client.Timeout exceeded while
// awaiting headers）。
func glmTimeout() time.Duration {
	const def = 300 * time.Second
	v := os.Getenv("AGMEM_GLM_TIMEOUT")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// CleanPrompt 清洗 prompt 模板（原始需求 9：删重复、整理为记忆条目/执行步骤）。
// 模板内 {{SOURCE}} 为「入库文档名」占位符，由 RenderCleanPrompt 渲染。
//
// v2（2026-09-07 用户反馈）：清洗产出的描述性碎片对 ask 无用，改为「可执行知识」
// 导向——一条一自足主题、结论先行、编号步骤、完整保留命令/参数/URL/路径/代码块、
// 不编造。
// v3（2026-09-08，按 eval 失败用例整改）：
//   - v3-1 主题句结论：每条第一行必须是「<做什么>的方法/步骤」式主题句，禁止以
//     「实例配置核查」「验收结论」等元信息开头——eval 检索命中后正文是元描述而非
//     答案，是该失败用例根因；
//   - v3-2 来源可核实：每条末尾加一行「来源：<原文档标题>」；
//   - v3-3 可核实性：保留原文中的具体数值/端口/路径/命令原样；无法核实时保留
//     原文表述，不猜测。
//
// v3.1（2026-09-08 用户拍板，检索召回整改，第 4 要素）：
//   - 每条最后一行输出「适用：」行，列出该条知识可能被问到的常见说法（含口语化
//     问法与英文术语），逗号分隔 5-10 个；
//   - 「适用：」行与「来源：」行并存（来源在前、适用为最后一行）；600 字预算含
//     两行，超长压缩正文、不删两行，仍由 SplitEntries 硬切兜底。
//
// v4（v5 审计 G01，2026-09-09）：来源行改为**必出**，值取「入库文档名」
// （source_name，由调用方传入并渲染进 {{SOURCE}}）——旧规则只在「原文首行是
// # 标题」时输出来源行、且取标题而非入库文档名，导致全库 57.6% clean 点无来源。
// 另强化锚点保真（G02）：IPv4/端口/路径/版本号/域名/sha 等必须逐字符照抄。
//
// 输出格式不变：仍是 markdown 正文（无解释外层），分条软约束「600 字」与分条
// 硬上限 MaxEntryLen、CLI --max-len 默认值三方统一 L=600，
// 改动须同步（tripwire TestCleanPromptEntryConstraint）。
//
// CleanRulesL0 是不变前缀（见 prefix.go / docs/prefix-caching.md）的 L0 段：**规则
// 文本一字未改**，只是从历史 CleanPrompt 常量搬到这里作为可复用常量。规则里保留
// 字面 {{SOURCE}} 占位符不变——若在 L0 里渲染成具体文档名，前缀会随文档逐请求
// 变化，缓存命中率归零。L0 必须逐字节稳定；实际文档名由 L3（PrefixUserBlock）
// 传给模型。
const CleanRulesL0 = `你是知识库清洗助手。把下面的原始文本整理为「可执行知识条目」：
1. 一条 = 一个完整自足的主题：第一行必须是主题句——「<做什么>的方法/步骤」式结论句，禁止以「实例配置核查」「验收结论」等元信息开头；随后给出可复现的编号步骤，完整保留原文中的命令、参数、URL、路径、代码块，不省略不编造；
2. 有命令的操作类知识必须给出完整命令序列；纯架构/概念知识凝练为要点列表（主题句先行）；
3. 删除寒暄、重复、过时、无信息量内容与「本文档介绍」式元描述；一事一条，不合并主题；
4. 单条（含「来源：」「适用：」两行）不超过 600 字，超长先压缩正文、不得删这两行；条目间用空行分隔；
5. 必须原样保留原文中的具体数值与标识——IPv4/IPv6、端口号、文件路径、版本号、域名、sha256/digest、命令与参数——逐字符照抄，不得省略、改写、补全或截断；无法核实的信息保留原文表述，不猜测、不改写数字；
6. 每条正文末尾必须写一行「来源：{{SOURCE}}」——原样照抄该值，不得改写、不得省略、不得用原文标题替换；该行位于「适用：」行之前；
7. 每条最后一行必须是「适用：」行，列出该条知识可能被问到的常见说法（含口语化问法与英文术语），逗号分隔，5-10 个，例：适用：怎么推代码, 提交代码, git push, 配置 git 凭据, 克隆仓库；
8. 原文信息不足以致步骤无法复现时，保留原文全部可用信息，不编造缺失步骤；
9. 若原文重复、过时或无任何可保留的价值，只输出空字符串。
只输出规范化后的正文，不要解释。`

// CleanPrompt 历史常量名：等于 L0 契约全文（含 {{SOURCE}} 占位符）。保留旧名是
// 为了不改 RenderCleanPrompt 及既有测试/调用点；字面量与历史逐字节一致。
const CleanPrompt = CleanRulesL0

// sourcePlaceholder 是 CleanPrompt 中入库文档名的占位符。
const sourcePlaceholder = "{{SOURCE}}"

// unnamedSource 无入库文档名且原文无可回退标题时的来源值（G01 兜底）。
const unnamedSource = "未命名文档"

// RenderCleanPrompt 渲染清洗 prompt：把 {{SOURCE}} 替换为入库文档名。
// sourceName 为空时回落 unnamedSource（来源行仍必出）。
func RenderCleanPrompt(sourceName string) string {
	s := strings.TrimSpace(sourceName)
	if s == "" {
		s = unnamedSource
	}
	return strings.ReplaceAll(CleanPrompt, sourcePlaceholder, s)
}

// StrictAnchorPrefix 严格重清 prompt 的锚点约束前缀（G02：清洗后锚点校验失败时
// 用本约束重清一次）。字面固定，测试断言存在。
const StrictAnchorPrefix = "必须原样保留以下锚点："

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Think    *thinkParam   `json:"think,omitempty"`
}

type thinkParam struct {
	Mode string `json:"mode"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CleanUsage 单次清洗请求的 token 用量（S3 观测）：网关 usage 直接透出。
// PromptCacheHitTokens/PromptCacheMissTokens 是 DeepSeek 前缀缓存命中/未命中计数
// （api-docs.deepseek.com/zh-cn/guides/kv_cache），用于验收轮内命中率 >90%。
type CleanUsage struct {
	PromptTokens          int
	PromptCacheHitTokens  int
	PromptCacheMissTokens int
	ReasoningTokens       int
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *usageBlock `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// usageBlock 网关 usage 字段（缺失时 Usage 为 nil，零值透出）。
type usageBlock struct {
	PromptTokens            int `json:"prompt_tokens"`
	PromptCacheHitTokens    int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens   int `json:"prompt_cache_miss_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// cleanUsage 把网关 usage 归一为 CleanUsage（nil → 零值）。
func (u *usageBlock) cleanUsage() CleanUsage {
	if u == nil {
		return CleanUsage{}
	}
	return CleanUsage{
		PromptTokens:          u.PromptTokens,
		PromptCacheHitTokens:  u.PromptCacheHitTokens,
		PromptCacheMissTokens: u.PromptCacheMissTokens,
		ReasoningTokens:       u.CompletionTokensDetails.ReasoningTokens,
	}
}

// statusError 上游非 200 响应（携带状态码与响应体，供降级判定分类）。
type statusError struct {
	Code int
	Body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("glm: status %d: %s", e.Code, truncateStr([]byte(e.Body), 256))
}

// isModelUnavailable 判定「模型不可用/不存在」类错误（404 或 400 且响应体提示
// 模型相关），触发降级链切换。
func isModelUnavailable(err error) bool {
	se, ok := err.(*statusError)
	if !ok {
		return false
	}
	if se.Code == http.StatusNotFound {
		return true
	}
	if se.Code != http.StatusBadRequest {
		return false
	}
	b := strings.ToLower(se.Body)
	for _, k := range []string{"model", "模型", "not found", "unsupported", "不存在"} {
		if strings.Contains(b, k) {
			return true
		}
	}
	return false
}

// isThinkUnsupported 判定「think.mode 不被接受」类 400（触发静默降级默认档）。
func isThinkUnsupported(err error) bool {
	se, ok := err.(*statusError)
	if !ok || se.Code != http.StatusBadRequest {
		return false
	}
	b := strings.ToLower(se.Body)
	for _, k := range []string{"think", "reasoning", "effort", "思考"} {
		if strings.Contains(b, k) {
			return true
		}
	}
	return false
}

// Clean 实现 cleaner.LLM（无来源名，回落 unnamedSource）。
func (g *GLMClient) Clean(ctx context.Context, text string) (string, error) {
	return g.clean(ctx, text, RenderCleanPrompt(""), nil)
}

// CleanWithSource 带入库文档名的清洗（G01：来源行值取 source_name）。
func (g *GLMClient) CleanWithSource(ctx context.Context, text, sourceName string) (string, error) {
	return g.clean(ctx, text, RenderCleanPrompt(sourceName), nil)
}

// CleanWithAnchors 带锚点强约束的严格重清（G02）：prompt 追加「必须原样保留以下
// 锚点：…」，用于清洗后锚点校验失败时重清一次。
func (g *GLMClient) CleanWithAnchors(ctx context.Context, text, sourceName string, anchors []string) (string, error) {
	return g.clean(ctx, text, RenderCleanPrompt(sourceName), anchors)
}

// CleanWithPrefix 带不变前缀的清洗（S2）：system = prefix.Text（L0+L1+L2，逐字节
// 稳定），user = L3（覆盖声明 + 目标 + 正文 + 可选锚点）。返回正文与本次 usage。
// 锚点约束放进 user 段（不放 system），否则锚点逐请求变化会把系统段前缀打散。
//
// targetDoc 是本次唯一目标文档的标识。库内没有 wiki URL（见 prefix.go 文件头
// 「已知偏差」），生产传的是入库文档名。
func (g *GLMClient) CleanWithPrefix(ctx context.Context, prefix Prefix, targetDoc, sourceName, text string, anchors []string) (string, CleanUsage, error) {
	msgs := []chatMessage{
		{Role: "system", Content: prefix.Text},
		{Role: "user", Content: PrefixUserBlock(prefix, targetDoc, sourceName, text, anchors)},
	}
	return g.chat(ctx, msgs)
}

// PrefixUserBlock 渲染 L3「本次目标」user 段。覆盖声明（前缀里的文档名 + 条目数）
// 与唯一目标文档必须同时出现：缺覆盖声明模型会把背景内容重提一遍，缺目标文档
// 模型不知道只针对哪篇文档。覆盖标识用文档名而非 URL（见 prefix.go「已知偏差」）。
func PrefixUserBlock(prefix Prefix, targetDoc, sourceName, text string, anchors []string) string {
	var b strings.Builder
	b.WriteString("【本前缀覆盖的文档】\n")
	lines := prefix.CoverageLines()
	if len(lines) == 0 {
		b.WriteString("（无）\n")
	} else {
		for _, l := range lines {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	b.WriteString("以上仅作背景资料，**不要**从这些文档提取条目。\n")
	b.WriteString("【本次提取目标文档】")
	b.WriteString(strings.TrimSpace(targetDoc))
	b.WriteString("\n只针对该文档的正文输出知识条目；与背景重复的内容不要重复输出。\n")
	b.WriteString("【本次入库文档名】")
	if s := strings.TrimSpace(sourceName); s != "" {
		b.WriteString(s)
	} else {
		b.WriteString(unnamedSource)
	}
	b.WriteString("（即规则中 {{SOURCE}} 的值）\n")
	if len(anchors) > 0 {
		b.WriteString("\n")
		b.WriteString(StrictAnchorPrefix)
		b.WriteString(strings.Join(anchors, "、"))
		b.WriteString("。这些标识必须逐字符原样出现在正文中。\n")
	}
	b.WriteString("【正文】\n")
	b.WriteString(text)
	return b.String()
}

// clean 执行清洗请求：按降级链逐个模型尝试，think 不被接受时静默降级默认档。
func (g *GLMClient) clean(ctx context.Context, text, prompt string, anchors []string) (string, error) {
	out, _, err := g.cleanMsgs(ctx, text, prompt, anchors)
	return out, err
}

// cleanMsgs system=prompt（+可选锚点）、user=text 的旧式两段请求，丢弃 usage。
func (g *GLMClient) cleanMsgs(ctx context.Context, text, prompt string, anchors []string) (string, CleanUsage, error) {
	if len(anchors) > 0 {
		prompt += "\n\n" + StrictAnchorPrefix + strings.Join(anchors, "、") + "。这些标识必须逐字符原样出现在正文中。"
	}
	msgs := []chatMessage{{Role: "system", Content: prompt}, {Role: "user", Content: text}}
	return g.chat(ctx, msgs)
}

// chat 按降级链发送消息，返回最后一次成功请求的正文与 usage。
func (g *GLMClient) chat(ctx context.Context, msgs []chatMessage) (string, CleanUsage, error) {
	chain := g.modelChain(ctx)
	thinking := g.Thinking
	var lastErr error
	for i, m := range chain {
		out, usage, err := g.doChat(ctx, m, thinking, msgs)
		if err == nil {
			if g.OnUsage != nil {
				g.OnUsage(usage)
			}
			return out, usage, nil
		}
		if isThinkUnsupported(err) && thinking != "" {
			g.emit(DegradeEvent{Kind: "think_downgrade", From: thinking, To: "default", Reason: err.Error()})
			thinking = ""
			out, usage, err = g.doChat(ctx, m, thinking, msgs)
			if err == nil {
				if g.OnUsage != nil {
					g.OnUsage(usage)
				}
				return out, usage, nil
			}
		}
		if isModelUnavailable(err) && i+1 < len(chain) {
			g.emit(DegradeEvent{Kind: "model_fallback", From: m, To: chain[i+1], Reason: err.Error()})
			lastErr = err
			continue
		}
		return "", CleanUsage{}, err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("glm: no model in chain succeeded")
	}
	return "", CleanUsage{}, lastErr
}

// modelChain 返回本次请求使用的降级链：Probe 开启时用 /models 探测结果过滤，
// 探测失败或结果为空则退回完整链。
func (g *GLMClient) modelChain(ctx context.Context) []string {
	chain := g.Models
	if len(chain) == 0 {
		chain = DefaultModelChain(g.Model)
	}
	if !g.Probe {
		return chain
	}
	avail := g.availableModels(ctx)
	if len(avail) == 0 {
		return chain
	}
	var filtered []string
	for _, m := range chain {
		if avail[m] {
			filtered = append(filtered, m)
		}
	}
	if len(filtered) == 0 {
		return chain
	}
	return filtered
}

// availableModels 探测 aigateway 可用模型列表（GET /models，OpenAI 兼容）。
// 结果缓存 probeTTL；失败/为空返回 nil（调用方退回完整链）。
func (g *GLMClient) availableModels(ctx context.Context) map[string]bool {
	g.probeMu.Lock()
	defer g.probeMu.Unlock()
	now := time.Now()
	if g.probeSet != nil || (!g.probedAt.IsZero() && now.Sub(g.probedAt) < g.probeTTL) {
		return g.probeSet
	}
	g.probedAt = now
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL+"/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	var mr struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil
	}
	set := make(map[string]bool, len(mr.Data))
	for _, d := range mr.Data {
		if d.ID != "" {
			set[d.ID] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	g.probeSet = set
	return set
}

// doChat 发一次 chat/completions 请求（指定 model 与 thinking 档位），返回正文与
// 网关 usage（缺失时零值）。
func (g *GLMClient) doChat(ctx context.Context, model, thinking string, msgs []chatMessage) (string, CleanUsage, error) {
	reqBody := chatRequest{Model: model, Messages: msgs}
	if thinking != "" {
		reqBody.Think = &thinkParam{Mode: thinking}
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", CleanUsage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", CleanUsage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.Token)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", CleanUsage{}, fmt.Errorf("glm: request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", CleanUsage{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", CleanUsage{}, &statusError{Code: resp.StatusCode, Body: string(body)}
	}
	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", CleanUsage{}, fmt.Errorf("glm: decode: %w", err)
	}
	if cr.Error != nil {
		return "", CleanUsage{}, fmt.Errorf("glm: api error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", CleanUsage{}, fmt.Errorf("glm: no choices")
	}
	return strings.TrimSpace(cr.Choices[0].Message.Content), cr.Usage.cleanUsage(), nil
}

// emit 记录降级事件：结构化日志 + 可选钩子。
func (g *GLMClient) emit(ev DegradeEvent) {
	if g.OnDegrade != nil {
		g.OnDegrade(ev)
		return
	}
	logDegrade(ev)
}

func (g *GLMClient) logDegrade(ev DegradeEvent) { logDegrade(ev) }

func truncateStr(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
