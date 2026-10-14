package cleaner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// TestCleanPromptEntryConstraint 分条批 tripwire：CleanPrompt 的分条软约束、
// 分条硬上限 MaxEntryLen、CLI --max-len 默认值三方统一 L=600（2026-09-07 拍板）。
// 任何一处改动导致本测试红 → 必须同步其余两处。
// v2（2026-09-07 可执行知识批）：断言随 prompt 重写更新，三方绑定语义不变；
// 另锁 v2 关键要素（一条一自足主题/编号步骤/空行分隔/空串兜底）——空串兜底是
// worker「全文重复」语义与占位过滤管线的依据，空行分隔是 SplitEntries 段界依据。
// v3（2026-09-08 用户拍板）：另锁 v3 三要素——主题句结论（禁止元信息开头，
// eval 失败用例根因）、来源行（来源：<标题>，可核实）、不猜测（无法
// 核实时保留原文表述）。
// v3.1（2026-09-08 用户拍板）：另锁第 4 要素「适用：」行——行首「适用：」、逗号
// 分隔、含口语问法与英文术语、5-10 个、且为该条最后一行（位于「来源：」之后）；
// 并锁长度预算：600 字含「来源：」「适用：」两行，无「不计入/豁免」措辞，
// 超长由 SplitEntries（MaxEntryLen 硬切）兜底而非 prompt 承诺保留两行。
func TestCleanPromptEntryConstraint(t *testing.T) {
	if MaxEntryLen != 600 {
		t.Fatalf("MaxEntryLen=%d：改动必须同步 CleanPrompt 字面 600 与 cmd/agmem --max-len 默认值（统一 L=600 决策）", MaxEntryLen)
	}
	for _, want := range []string{
		"一个完整自足的主题", // 一条一主题（可执行知识 v2 核心要求）
		"编号步骤",      // 步骤类保持编号列表（不回退为描述性碎片）
		"不编造",       // 不省略不编造（含信息不足时不编造缺失步骤）
		"不超过 600 字", // 长度硬约束（与 MaxEntryLen 一致，含来源/适用两行）
		"条目间用空行分隔",  // 分条边界（SplitEntries 空行段界依赖）
		"只输出空字符串",   // 全文重复语义保留（占位过滤兜底依据）
		"主题句",       // v3-1：每条第一行必须是主题句（禁止「实例配置核查」式元信息开头）
		"来源：",       // v3-2：每条末尾「来源：<原文档标题>」行（首行 # 标题，没有则省略）
		"不猜测",       // v3-3：无法核实时保留原文表述，不猜测
		"适用：",       // v3.1：行首「适用：」行（检索召回的查询侧说法）
		"逗号分隔",      // v3.1：适用项以逗号分隔
		"口语化问法",     // v3.1：含口语化问法
		"英文术语",      // v3.1：含英文术语
		"5-10 个",    // v3.1：适用项数量 5-10
		"最后一行",      // v3.1：适用行为该条最后一行（来源在前、适用在后）
	} {
		if !strings.Contains(CleanPrompt, want) {
			t.Errorf("CleanPrompt 缺少约束 %q\nprompt:\n%s", want, CleanPrompt)
		}
	}
	// v3.1 长度预算断言：600 是含「来源：」「适用：」两行的单条硬预算，
	// 无「不计入/豁免」措辞；超长由 SplitEntries 硬切兜底。
	if !strings.Contains(CleanPrompt, "含「来源：」「适用：」两行") {
		t.Errorf("CleanPrompt 未把「来源：」「适用：」两行计入 600 字预算\nprompt:\n%s", CleanPrompt)
	}
	for _, forbidden := range []string{"不计入", "豁免"} {
		if strings.Contains(CleanPrompt, forbidden) {
			t.Errorf("v3.1：适用行不得豁免 600 预算，CleanPrompt 出现 %q\nprompt:\n%s", forbidden, CleanPrompt)
		}
	}
}

// —— G01：来源行必出（prompt v4）——

// TestCleanPromptSourceMandatory 锁 v4 关键约束：来源行必出、值取 {{SOURCE}}
// （入库文档名）、旧条件式来源规则（首行 # 标题 / 没有则省略）已移除。
func TestCleanPromptSourceMandatory(t *testing.T) {
	for _, want := range []string{
		sourcePlaceholder, // {{SOURCE}} 占位符（渲染时替换为入库文档名）
		"必须写一行「来源：",       // 来源行必出
		"不得省略",            // 不得省略
		"不得用原文标题替换",       // 值取入库文档名而非标题
		"IPv4",            // G02 锚点保真强化
		"sha256/digest",   // G02 锚点保真强化
	} {
		if !strings.Contains(CleanPrompt, want) {
			t.Errorf("CleanPrompt 缺少约束 %q\nprompt:\n%s", want, CleanPrompt)
		}
	}
	for _, forbidden := range []string{"若原文首行是", "没有则省略该行"} {
		if strings.Contains(CleanPrompt, forbidden) {
			t.Errorf("v4：旧条件式来源规则未移除 %q\nprompt:\n%s", forbidden, CleanPrompt)
		}
	}
	if got := RenderCleanPrompt("k8s-deploy.md"); !strings.Contains(got, "来源：k8s-deploy.md") || strings.Contains(got, sourcePlaceholder) {
		t.Errorf("RenderCleanPrompt 未替换占位符:\n%s", got)
	}
	if got := RenderCleanPrompt("  "); !strings.Contains(got, "来源："+unnamedSource) {
		t.Errorf("空来源名应回落 %s:\n%s", unnamedSource, got)
	}
	if StrictAnchorPrefix != "必须原样保留以下锚点：" {
		t.Errorf("StrictAnchorPrefix = %q", StrictAnchorPrefix)
	}
}

// —— G11：模型降级链 / think 降级 / 探测 ——

// TestGLMClean_ModelFallbackOn404：primary 404 → 切备模型重试，降级事件结构化记录。
func TestGLMClean_ModelFallbackOn404(t *testing.T) {
	var mu sync.Mutex
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		models = append(models, req.Model)
		mu.Unlock()
		if req.Model == "primary-model" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"清洗结果"}}]}`))
	}))
	defer srv.Close()
	var events []DegradeEvent
	g := &GLMClient{
		BaseURL: srv.URL, Model: "primary-model", Thinking: "medium", Token: "t",
		HTTP: srv.Client(), Models: []string{"primary-model", "fallback-model"},
		OnDegrade: func(ev DegradeEvent) { events = append(events, ev) },
	}
	out, err := g.Clean(context.Background(), "原文")
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if out != "清洗结果" {
		t.Errorf("out = %q, want 清洗结果", out)
	}
	if !reflect.DeepEqual(models, []string{"primary-model", "fallback-model"}) {
		t.Errorf("请求模型序列 = %v, want [primary-model fallback-model]", models)
	}
	if len(events) != 1 || events[0].Kind != "model_fallback" ||
		events[0].From != "primary-model" || events[0].To != "fallback-model" {
		t.Errorf("降级事件 = %+v, want model_fallback primary→fallback", events)
	}
}

// TestGLMClean_ThinkDowngradeOn400：400「think 不被接受」→ 静默去掉 think 重试。
func TestGLMClean_ThinkDowngradeOn400(t *testing.T) {
	var mu sync.Mutex
	var thinks []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Think *struct {
				Mode string `json:"mode"`
			} `json:"think"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mode := ""
		if req.Think != nil {
			mode = req.Think.Mode
		}
		mu.Lock()
		thinks = append(thinks, mode)
		mu.Unlock()
		if req.Think != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"think mode not supported"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	var events []DegradeEvent
	g := &GLMClient{
		BaseURL: srv.URL, Model: "m", Thinking: "medium", Token: "t",
		HTTP: srv.Client(), Models: []string{"m"},
		OnDegrade: func(ev DegradeEvent) { events = append(events, ev) },
	}
	out, err := g.Clean(context.Background(), "原文")
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if out != "ok" {
		t.Errorf("out = %q, want ok", out)
	}
	if !reflect.DeepEqual(thinks, []string{"medium", ""}) {
		t.Errorf("think 序列 = %v, want [medium \"\"]（第二次静默降级默认档）", thinks)
	}
	if len(events) != 1 || events[0].Kind != "think_downgrade" {
		t.Errorf("降级事件 = %+v, want think_downgrade", events)
	}
}

// TestGLMClean_ProbeFiltersChain：/models 探测只返回备模型 → 不再请求 primary。
func TestGLMClean_ProbeFiltersChain(t *testing.T) {
	var mu sync.Mutex
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"fallback-model"}]}`))
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		models = append(models, req.Model)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	g := &GLMClient{
		BaseURL: srv.URL, Model: "primary-model", Thinking: "medium", Token: "t",
		HTTP: srv.Client(), Probe: true, Models: []string{"primary-model", "fallback-model"},
	}
	if _, err := g.Clean(context.Background(), "原文"); err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if !reflect.DeepEqual(models, []string{"fallback-model"}) {
		t.Errorf("探测后请求模型 = %v, want [fallback-model]", models)
	}
}

// TestGLMCleanWithAnchors_PromptContainsStrictConstraint：严格重清 prompt 含
// 「必须原样保留以下锚点：<清单>」与来源行。
func TestGLMCleanWithAnchors_PromptContainsStrictConstraint(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			got = req.Messages[0].Content
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	g := &GLMClient{BaseURL: srv.URL, Model: "m", Token: "t", HTTP: srv.Client(), Models: []string{"m"}}
	if _, err := g.CleanWithAnchors(context.Background(), "原文", "src.md", []string{"192.0.2.162", "1194"}); err != nil {
		t.Fatalf("CleanWithAnchors: %v", err)
	}
	for _, want := range []string{StrictAnchorPrefix, "192.0.2.162", "1194", "来源：src.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("严格重清 prompt 缺 %q:\n%s", want, got)
		}
	}
}

// TestDefaultModelChain：primary → 同家族 → 千帆文本模型（env 未覆盖）。
func TestDefaultModelChain(t *testing.T) {
	t.Setenv("AGMEM_GLM_FALLBACK_MODELS", "")
	got := DefaultModelChain("glm-5.3-flash")
	want := []string{"glm-5.3-flash", "glm-5.3", "ernie-4.5-turbo-128k"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DefaultModelChain = %v, want %v", got, want)
	}
	t.Setenv("AGMEM_GLM_FALLBACK_MODELS", "custom-a,custom-b")
	got = DefaultModelChain("glm-5.3-flash")
	want = []string{"glm-5.3-flash", "glm-5.3", "custom-a", "custom-b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DefaultModelChain(env) = %v, want %v", got, want)
	}
}

// —— S2/S3：带前缀清洗的 messages 结构与 usage 透出 ——

// TestGLMCleanWithPrefix_MessagesAndUsage：system=前缀 Text（逐字节），user=L3
// （覆盖声明 + 目标 + 正文 + 锚点）；usage 的 cache hit/miss/reasoning 正确透出。
func TestGLMCleanWithPrefix_MessagesAndUsage(t *testing.T) {
	var systems, users []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if m.Role == "system" {
				systems = append(systems, m.Content)
			} else {
				users = append(users, m.Content)
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"清洗结果"}}],
			"usage":{"prompt_tokens":1000,"prompt_cache_hit_tokens":900,"prompt_cache_miss_tokens":100,
			"completion_tokens_details":{"reasoning_tokens":42}}}`))
	}))
	defer srv.Close()

	b := &PrefixBuilder{}
	prefix := b.Build([]KnowledgeEntry{
		{ItemID: 1, DocName: "a.md", Dir: "a-clean", Text: "背景条目"},
	}, "a-clean")
	var seen []CleanUsage
	g := &GLMClient{
		BaseURL: srv.URL, Model: "m", Token: "t", HTTP: srv.Client(), Models: []string{"m"},
		OnUsage: func(u CleanUsage) { seen = append(seen, u) },
	}
	out, usage, err := g.CleanWithPrefix(context.Background(), prefix, "target.md", "src.md", "本次正文", []string{"1.2.3.4"})
	if err != nil {
		t.Fatalf("CleanWithPrefix: %v", err)
	}
	if out != "清洗结果" {
		t.Errorf("out = %q", out)
	}
	if len(systems) != 1 || systems[0] != prefix.Text {
		t.Fatalf("system 段 != 前缀 Text（前缀必须逐字节进 system）:\n%q", systems[0])
	}
	if len(users) != 1 {
		t.Fatalf("user 段数 = %d", len(users))
	}
	for _, want := range []string{
		"【本前缀覆盖的文档】", "a.md（1 条）", "**不要**从这些文档提取条目",
		"【本次提取目标文档】target.md", "本次正文", StrictAnchorPrefix, "1.2.3.4",
	} {
		if !strings.Contains(users[0], want) {
			t.Errorf("L3 缺 %q:\n%s", want, users[0])
		}
	}
	if strings.Contains(systems[0], "1.2.3.4") {
		t.Errorf("锚点不得进入 system 前缀（会破坏前缀稳定性）")
	}
	want := CleanUsage{PromptTokens: 1000, PromptCacheHitTokens: 900, PromptCacheMissTokens: 100, ReasoningTokens: 42}
	if usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
	if len(seen) != 1 || seen[0] != want {
		t.Errorf("OnUsage = %+v, want 一次 %+v", seen, want)
	}
}

// TestGLMCleanUsageMissing：网关不返回 usage 时零值透出，不 panic。
func TestGLMCleanUsageMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	g := &GLMClient{BaseURL: srv.URL, Model: "m", Token: "t", HTTP: srv.Client(), Models: []string{"m"}}
	out, usage, err := g.CleanWithPrefix(context.Background(), Prefix{Text: "L0"}, "u", "s", "t", nil)
	if err != nil {
		t.Fatalf("CleanWithPrefix: %v", err)
	}
	if out != "ok" || usage != (CleanUsage{}) {
		t.Errorf("out=%q usage=%+v, want ok/零值", out, usage)
	}
}

// TestGLMLegacyPathsUnchanged 回归：既有三方法行为不变（system 仍是渲染规则、
// user 仍是正文），不因引入前缀改动而变。
func TestGLMLegacyPathsUnchanged(t *testing.T) {
	var roles []string
	var system, user string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		roles = roles[:0]
		for _, m := range req.Messages {
			roles = append(roles, m.Role)
			if m.Role == "system" {
				system = m.Content
			} else {
				user = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	g := &GLMClient{BaseURL: srv.URL, Model: "m", Token: "t", HTTP: srv.Client(), Models: []string{"m"}}

	if _, err := g.CleanWithSource(context.Background(), "原文", "doc.md"); err != nil {
		t.Fatalf("CleanWithSource: %v", err)
	}
	if len(roles) != 2 || roles[0] != "system" || roles[1] != "user" {
		t.Fatalf("roles = %v, want [system user]", roles)
	}
	if user != "原文" || !strings.Contains(system, "来源：doc.md") || strings.Contains(system, "【本前缀覆盖的 URL】") {
		t.Fatalf("CleanWithSource 行为改变：system=%q user=%q", system, user)
	}

	if _, err := g.CleanWithAnchors(context.Background(), "原文", "doc.md", []string{"X1"}); err != nil {
		t.Fatalf("CleanWithAnchors: %v", err)
	}
	if !strings.Contains(system, StrictAnchorPrefix+"X1") || user != "原文" {
		t.Fatalf("CleanWithAnchors 行为改变：system=%q user=%q", system, user)
	}
}
