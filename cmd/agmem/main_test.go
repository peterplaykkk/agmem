package main

// UC-2 输出格式（renderAsk 纯函数）+ UC-4 help 可执行性门禁 UT。
//
// 门禁语义：从 usage 常量提取全部以 "agmem " 开头的示例行，经 shell 词法切分后
// 调 CLI 命令入口函数真实执行（mock agmemserv API + mock agmemd），断言 err==nil。
// 未来 help 示例与实现漂移（flag 改名/端点变更/参数语义变化）→ 本文件必红。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"sre/agmem/scripts"
)

// —— UC-2：renderAsk 纯函数 ——

func sampleAskResult() askResult {
	return askResult{
		CacheHit:   false,
		ID:         "q-deadbeefdeadbeef",
		Similarity: 0,
		KBVersion:  7,
		Result: json.RawMessage(`{"kb":"sre","version":7,"hits":[` +
			`{"id":"p-1","score":0.83,"payload":{"kb":"sre","dir":"云-clean","text":"统一出口由 代理网关 承载"}},` +
			`{"id":"p-2","score":0.61,"payload":{"dir":"ai-cluster-clean","text":"第二条正文"}}]}`),
	}
}

func TestRenderAsk(t *testing.T) {
	t.Run("text_多条目仅正文换行分段_末尾边界声明", func(t *testing.T) {
		// 2026-09-07 用户对齐：多条答案换行分段——贴近 GLM 入库自然语言，
		// 零分隔线/零标签/零元数据；来源仅 --debug 可见。
		// 2026-09-08 输出边界批：hits 非空时末尾固定追加一行边界声明。
		want := "统一出口由 代理网关 承载\n\n第二条正文\n" + askBoundaryNote
		if got := renderAsk(sampleAskResult(), modeText, 0); got != want {
			t.Fatalf("got:\n%s\nwant:\n%s", got, want)
		}
	})
	t.Run("text_禁含易变字段", func(t *testing.T) {
		got := renderAsk(sampleAskResult(), modeText, 0)
		for _, bad := range []string{"score", "cache-hit", "kb_version", "q-deadbeef", "sim=", "version",
			"[1]", "[2]", "sre/云-clean", "ai-cluster-clean"} {
			if strings.Contains(got, bad) {
				t.Errorf("default text must not contain %q, got:\n%s", bad, got)
			}
		}
	})
	t.Run("text_默认无来源前缀", func(t *testing.T) {
		// 专项断言：默认输出不暴露分库设计（kb/dir 来源标注仅 --debug/--json 可见），
		// 正文本身须完整保留。
		got := renderAsk(sampleAskResult(), modeText, 0)
		for _, bad := range []string{"sre", "云-clean", "ai-cluster", "----\n["} {
			if strings.Contains(got, bad) {
				t.Errorf("default text must not leak source label %q, got:\n%s", bad, got)
			}
		}
		if !strings.Contains(got, "统一出口由 代理网关 承载") || !strings.Contains(got, "第二条正文") {
			t.Fatalf("entry bodies must be kept verbatim, got:\n%s", got)
		}
		// 边界声明行必须在（2026-09-08 输出边界批）
		if !strings.HasSuffix(got, "\n"+askBoundaryNote) {
			t.Fatalf("default text must end with boundary note, got:\n%s", got)
		}
	})
	t.Run("text_空结果输出无可用知识_无边界行", func(t *testing.T) {
		res := askResult{Result: json.RawMessage(`{"kb":"sre","hits":[]}`)}
		if got := renderAsk(res, modeText, 0); got != "无可用知识" {
			t.Fatalf("got %q", got)
		}
		// 全被阈值过滤即 hits:[]，同路径；空结果不追加边界行
		res2 := askResult{Result: json.RawMessage(`{"global":true,"kbs":["sre"],"hits":[]}`)}
		if got := renderAsk(res2, modeText, 0); got != "无可用知识" ||
			strings.Contains(got, askBoundaryNote) {
			t.Fatalf("empty result must not carry boundary note, got %q", got)
		}
	})
	t.Run("text_非JSON原文兜底", func(t *testing.T) {
		raw := `not-json{`
		if got := renderAsk(askResult{Result: json.RawMessage(raw)}, modeText, 0); got != raw {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("text_error形状按无知识处理", func(t *testing.T) {
		// {"error":...} 可解析为 0 命中 → 无可用知识（daemon 错误本走非 200，此处仅防御）
		if got := renderAsk(askResult{Result: json.RawMessage(`{"error":"boom"}`)}, modeText, 0); got != "无可用知识" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("text_单条目无分隔线_末尾边界声明", func(t *testing.T) {
		res := askResult{Result: json.RawMessage(
			`{"kb":"sre","hits":[{"id":"p-1","score":0.9,"payload":{"dir":"云-clean","text":"正文"}}]}`)}
		want := "正文\n" + askBoundaryNote
		if got := renderAsk(res, modeText, 0); got != want {
			t.Fatalf("got:\n%s", got)
		}
	})
	t.Run("debug_诊断行与score后缀_末尾边界声明", func(t *testing.T) {
		got := renderAsk(sampleAskResult(), modeDebug, 0)
		if !strings.HasPrefix(got, "# id=q-deadbeefdeadbeef cache-hit=false version=7\n") {
			t.Fatalf("missing debug header, got:\n%s", got)
		}
		if !strings.Contains(got, "[1] sre/云-clean (score=0.83)") {
			t.Fatalf("missing score suffix, got:\n%s", got)
		}
		if !strings.Contains(got, "[2] sre/ai-cluster-clean (score=0.61)") {
			t.Fatalf("missing 2nd score suffix, got:\n%s", got)
		}
		if !strings.HasSuffix(got, "\n"+askBoundaryNote) {
			t.Fatalf("debug text must end with boundary note, got:\n%s", got)
		}
	})
	t.Run("debug_命中时含sim", func(t *testing.T) {
		res := sampleAskResult()
		res.CacheHit = true
		res.Similarity = 0.9123
		got := renderAsk(res, modeDebug, 0)
		if !strings.Contains(got, "# id=q-deadbeefdeadbeef cache-hit=true version=7 sim=0.9123") {
			t.Fatalf("missing sim in debug header, got:\n%s", got)
		}
	})
	t.Run("debug_空结果也先诊断行_无边界行", func(t *testing.T) {
		res := askResult{ID: "q-1", KBVersion: 3, Result: json.RawMessage(`{"hits":[]}`)}
		got := renderAsk(res, modeDebug, 0)
		want := "# id=q-1 cache-hit=false version=3\n无可用知识"
		if got != want {
			t.Fatalf("got:\n%s\nwant:\n%s", got, want)
		}
	})
	t.Run("json_完整结构含score_不追加边界行", func(t *testing.T) {
		got := renderAsk(sampleAskResult(), modeJSON, 0)
		var out map[string]any
		if err := json.Unmarshal([]byte(got), &out); err != nil {
			t.Fatalf("not valid json: %v", err)
		}
		for _, key := range []string{"cache_hit", "id", "kb_version", "result"} {
			if _, ok := out[key]; !ok {
				t.Errorf("json missing key %q", key)
			}
		}
		if !strings.Contains(got, `"score": 0.83`) {
			t.Errorf("json must keep score, got:\n%s", got)
		}
		// 边界声明仅面向文本消费方；--json 是结构化契约，不得混入
		if strings.Contains(got, askBoundaryNote) {
			t.Errorf("json output must NOT contain boundary note, got:\n%s", got)
		}
	})
}

// G06：--debug 输出阈值过滤诊断行（mode/threshold/rule/kept/dropped/reason）；
// 默认 text 不显示（缓存友好），--json 完整透传服务端 filter 段。
func TestRenderAsk_FilterDiag(t *testing.T) {
	raw := `{"kb":"sre","hits":[],"filter":{"mode":"hybrid","threshold":0.5,` +
		`"rule":"dense_score>=threshold || sparse_strong","kept":0,"dropped":2,` +
		`"dropped_reason":"dense_score<threshold 且非 sparse 强命中"}}`
	res := askResult{ID: "q-1", KBVersion: 7, Result: json.RawMessage(raw)}

	got := renderAsk(res, modeDebug, 0)
	if !strings.Contains(got, "# filter: mode=hybrid threshold=0.50 rule=dense_score>=threshold || sparse_strong kept=0 dropped=2 reason=") {
		t.Fatalf("debug must render filter diag, got:\n%s", got)
	}
	if !strings.Contains(got, "无可用知识") {
		t.Fatalf("empty hits must still print 无可用知识, got:\n%s", got)
	}
	if strings.Contains(renderAsk(res, modeText, 0), "filter:") {
		t.Fatal("default text must not leak filter diagnostics")
	}
}

// G06：--json 透传 hybrid 分数证据与 filter 段（dense_score/sparse_rank/sparse_score）。
func TestRenderAsk_JSONHybridEvidence(t *testing.T) {
	raw := `{"kb":"sre","hits":[{"id":"p-1","score":0.0769,"dense_score":0.62,` +
		`"sparse_rank":0,"sparse_score":9.9,"payload":{"dir":"k8s-clean","text":"正文"}}],` +
		`"filter":{"mode":"hybrid","threshold":0.5,"rule":"x","kept":1,"dropped":0}}`
	got := renderAsk(askResult{Result: json.RawMessage(raw)}, modeJSON, 0)
	for _, key := range []string{"dense_score", "sparse_rank", "sparse_score", "filter"} {
		if !strings.Contains(got, key) {
			t.Fatalf("json must expose %q, got:\n%s", key, got)
		}
	}
}

// —— 展示截断（分条批 2026-09-07：text/debug 截断省展示 token，--json 永远全文）——

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"未超长原样", "统一出口", 600, "统一出口"},
		{"精确边界不截断", strings.Repeat("字", 600), 600, strings.Repeat("字", 600)},
		{"超长截断带尾标", strings.Repeat("字", 650), 600, strings.Repeat("字", 600) + "…(截断,650字)"},
		{"n=0不截断", strings.Repeat("字", 700), 0, strings.Repeat("字", 700)},
		{"负数不截断", "abc", -1, "abc"},
		{"按rune计不劈字", "αβγδ", 2, "αβ…(截断,4字)"},
	}
	for _, c := range cases {
		if got := truncateRunes(c.in, c.n); got != c.want {
			t.Errorf("%s: truncateRunes(n=%d) = %.30s..., want %.30s...", c.name, c.n, got, c.want)
		}
	}
}

func TestRenderAskTruncation(t *testing.T) {
	long := strings.Repeat("长", 800)
	res := askResult{Result: json.RawMessage(
		`{"kb":"sre","hits":[{"id":"p-1","score":0.9,"payload":{"dir":"云-clean","text":"` + long + `"}}]}`)}
	t.Run("text截断带尾标", func(t *testing.T) {
		got := renderAsk(res, modeText, 600)
		want := strings.Repeat("长", 600) + "…(截断,800字)\n" + askBoundaryNote
		if got != want {
			t.Fatalf("got head=%q tail=%q", got[:30], got[len(got)-40:])
		}
	})
	t.Run("maxlen0不截断", func(t *testing.T) {
		got := renderAsk(res, modeText, 0)
		if !strings.Contains(got, long) {
			t.Fatal("maxLen=0 must keep full text")
		}
	})
	t.Run("json永远全文", func(t *testing.T) {
		got := renderAsk(res, modeJSON, 600)
		if !strings.Contains(got, long) {
			t.Fatal("json must never truncate")
		}
	})
	t.Run("debug同text截断", func(t *testing.T) {
		got := renderAsk(res, modeDebug, 600)
		if !strings.Contains(got, "…(截断,800字)") {
			t.Fatalf("debug must truncate like text, tail=%q", got[len(got)-30:])
		}
	})
	t.Run("短文本不受影响", func(t *testing.T) {
		short := askResult{Result: json.RawMessage(
			`{"kb":"sre","hits":[{"id":"p-1","score":0.9,"payload":{"dir":"云-clean","text":"短正文"}}]}`)}
		if got := renderAsk(short, modeText, 600); !strings.Contains(got, "短正文") || strings.Contains(got, "截断") {
			t.Fatalf("短文本不应截断: %q", got)
		}
	})
}

func TestCmdAskMaxLenFlag(t *testing.T) {
	t.Run("默认max_len截断展示", func(t *testing.T) {
		_ = setupCLIMocks(t)
		out := captureStdout(t, func() error { return cmdAsk([]string{"--kb", "sre", "统一出口代理地址"}) })
		// mock 正文远小于默认上限 600 → 不截
		if strings.Contains(out, "截断") {
			t.Fatalf("短正文默认不截断: %s", out)
		}
	})
	t.Run("显式max_len生效且json全文", func(t *testing.T) {
		_ = setupCLIMocks(t)
		out := captureStdout(t, func() error {
			return cmdAsk([]string{"--kb", "sre", "--max-len", "5", "统一出口代理地址"})
		})
		if !strings.Contains(out, "统一出口由…(截断,13字)") {
			t.Fatalf("--max-len 5 未生效: %s", out)
		}
		outJSON := captureStdout(t, func() error {
			return cmdAsk([]string{"--kb", "sre", "--max-len", "5", "--json", "统一出口代理地址"})
		})
		if strings.Contains(outJSON, "截断") {
			t.Fatalf("--json 不得截断: %s", outJSON)
		}
		var res askResult
		if err := json.Unmarshal([]byte(outJSON), &res); err != nil {
			t.Fatalf("json broken: %v", err)
		}
	})
	t.Run("max_len0不截断", func(t *testing.T) {
		_ = setupCLIMocks(t)
		out := captureStdout(t, func() error {
			return cmdAsk([]string{"--kb", "sre", "--max-len", "0", "统一出口代理地址"})
		})
		if strings.Contains(out, "截断") {
			t.Fatalf("--max-len 0 必须全文: %s", out)
		}
	})
	t.Run("max_len不透传上游", func(t *testing.T) {
		ctl := setupCLIMocks(t)
		_ = captureStdout(t, func() error {
			return cmdAsk([]string{"--kb", "sre", "--max-len", "5", "q"})
		})
		if _, ok := ctl.askBody()["max_len"]; ok {
			t.Fatalf("截断是纯展示层行为，不得进入上游请求体: %v", ctl.askBody())
		}
	})
}

// askHitLabel payload.kb 缺失回落响应级 kb；空段省略。
func TestAskHitLabel(t *testing.T) {
	cases := []struct {
		respKB, hitKB, dir, want string
	}{
		{"sre", "sre", "云-clean", "sre/云-clean"},
		{"sre", "", "云-clean", "sre/云-clean"}, // 老点位 payload 无 kb → 响应级回落
		{"", "ops", "k8s-clean", "ops/k8s-clean"},
		{"sre", "sre", "", "sre"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		if got := askHitLabel(c.respKB, c.hitKB, c.dir); got != c.want {
			t.Errorf("askHitLabel(%q,%q,%q)=%q want %q", c.respKB, c.hitKB, c.dir, got, c.want)
		}
	}
}

// —— shell 词法切分（示例行 → argv）——

func TestSplitShellWords(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{name: "引号内空白不切分", in: `ask "统一出口 代理地址" --k 3`, want: []string{"ask", "统一出口 代理地址", "--k", "3"}},
		{name: "单引号", in: `push -t '单引 号'`, want: []string{"push", "-t", "单引 号"}},
		{name: "双引号转义", in: `ask "a\"b"`, want: []string{"ask", `a"b`}},
		{name: "多余空白", in: "  ask   x  ", want: []string{"ask", "x"}},
		{name: "空串", in: "", want: nil},
		{name: "未闭合双引号报错", in: `ask "abc`, wantErr: true},
		{name: "未闭合单引号报错", in: `ask 'abc`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := splitShellWords(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("split: %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
				t.Fatalf("got %#v want %#v", got, c.want)
			}
		})
	}
}

// —— UC-4：help 示例提取 ——

// extractExampleLines 提取以 "agmem " 开头且第二 token 为已知子命令的示例行。
// 过滤规则：含 "<" 的占位行跳过；非子命令开头的说明性行（如标题 "agmem — ..."）跳过。
// 命令清单即 dispatch 清单——新增子命令未接入 dispatch 时门禁必红。
var knownCommands = map[string]bool{"push": true, "ask": true, "status": true, "kb": true, "clear": true,
	"forget": true, "retry": true, "completion": true, "--version": true}

func extractExampleLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "agmem ") || strings.Contains(l, "<") {
			continue
		}
		rest := strings.TrimPrefix(l, "agmem ")
		cmd, _, _ := strings.Cut(rest, " ")
		if !knownCommands[cmd] {
			continue
		}
		out = append(out, l)
	}
	return out
}

func TestExtractExampleLines(t *testing.T) {
	text := "说明:\n  agmem ask \"q\"\n用法: agmem <command> [flags]\n子命令帮助: agmem <command> --help\n  AGMEM_URL env\nagmem — 智能记忆系统 CLI\n"
	got := extractExampleLines(text)
	if len(got) != 1 || got[0] != `agmem ask "q"` {
		t.Fatalf("got %#v", got)
	}
	// 标题行（agmem — 描述）与未知命令行不得混入
	if got := extractExampleLines("agmem — CLI\nagmem bogus --x"); len(got) != 0 {
		t.Fatalf("non-example lines leaked: %#v", got)
	}
	// 每个常量都至少能提取到示例（防止常量重构后 UT 静默扫空）
	for _, c := range []struct{ name, text string }{
		{"usage", usage}, {"usagePush", usagePush}, {"usageAsk", usageAsk},
		{"usageStatus", usageStatus}, {"usageKB", usageKB}, {"usageClear", usageClear},
		{"usageKBCreate", usageKBCreate}, {"usageKBDir", usageKBDir},
		{"usageForget", usageForget}, {"usageRetry", usageRetry},
		{"usageCompletion", usageCompletion},
	} {
		if n := len(extractExampleLines(c.text)); n == 0 {
			t.Errorf("%s contains no example lines", c.name)
		}
	}
}

// —— mock 基础设施 ——

// mockDaemon 假 agmemd：healthz/ask/clear；记录最近一次 ask 请求体。
type mockDaemon struct {
	mu       sync.Mutex
	lastAsk  map[string]any
	askCalls int
}

func (d *mockDaemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "engine": map[string]bool{"ready": true}})
		case "/api/ask":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			d.mu.Lock()
			d.lastAsk = body
			d.askCalls++
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cache_hit": false, "id": "q-deadbeefdeadbeef", "kb_version": 7,
				"result": map[string]any{
					"kb": body["kb"], "global": body["global"], "version": 7,
					"hits": []map[string]any{
						{"id": "p-1", "score": 0.83, "payload": map[string]any{"kb": "sre", "dir": "云-clean", "text": "统一出口由 代理网关 承载"}},
						{"id": "p-2", "score": 0.61, "payload": map[string]any{"dir": "ai-cluster-clean", "text": "第二条正文"}},
					},
				},
			})
		case "/api/clear":
			_ = json.NewEncoder(w).Encode(map[string]any{"freed_bytes": 123})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (d *mockDaemon) askBody() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastAsk
}

// mockAgmemAPI 假 agmemserv：push/status/kb 全端点，形状与 internal/api 契约一致。
func mockAgmemAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/kb/{kb}/docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"task_id":1,"kb":"sre","dir":"mysql","items":2,"status":"pending","hint":"hint"}`))
	})
	mux.HandleFunc("GET /api/kb/{kb}/tasks/{task_id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":42,"status":"cleaning","items":[{"id":1,"status":"cleaned","original_text":"原始文本"}]}`))
	})
	mux.HandleFunc("GET /api/kb", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"kbs":[{"id":1,"name":"sre","role":"machine","dir_count":2,"item_count":10,"version":7,"last_task_id":1,"last_task_status":"cleaning"}]}`))
	})
	mux.HandleFunc("POST /api/kb", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1,"name":"sre"}`))
	})
	mux.HandleFunc("POST /api/kb/{kb}/dir", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":2,"kb":"sre","dir":"mysql"}`))
	})
	mux.HandleFunc("GET /api/kb/{kb}/dirs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"kb":"sre","dirs":[{"id":2,"name":"mysql"}]}`))
	})
	// UC-011 生命周期端点（forget/retry 的 help 示例可执行性门禁用）
	mux.HandleFunc("DELETE /api/kb/{kb}/items/{item_id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"kb":"sre","item_id":42,"deleted_points":3,"version":8}`))
	})
	mux.HandleFunc("POST /api/kb/{kb}/items/{item_id}/retry", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"kb":"sre","item_id":42,"status":"pending","version":8}`))
	})
	return mux
}

// setupCLIMocks 起 mock agmemserv + mock agmemd 并注入环境变量。
// agmemd 以 httptest 随机端口承载（AGMEMD_ADDR 指向），CLI 侧不涉及本地
// embedding（healthz 探测通过即走守护 HTTP），无需 onnxruntime。
func setupCLIMocks(t *testing.T) *mockDaemon {
	t.Helper()
	apiSrv := httptest.NewServer(mockAgmemAPI())
	t.Cleanup(apiSrv.Close)
	t.Setenv("AGMEM_URL", apiSrv.URL)
	t.Setenv("AGMEM_TOKEN", "test-token")

	ctl := &mockDaemon{}
	ctlSrv := httptest.NewServer(ctl.handler())
	t.Cleanup(ctlSrv.Close)
	t.Setenv("AGMEMD_ADDR", strings.TrimPrefix(ctlSrv.URL, "http://"))
	t.Setenv("AGMEMD_DIR", t.TempDir()) // 防误触真实守护写 ~/
	return ctl
}

// captureStdout 捕获命令入口的标准输出。
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	errCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		errCh <- string(b)
	}()
	ferr := fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = old
	out := <-errCh
	if ferr != nil {
		t.Fatalf("cmd err: %v", ferr)
	}
	return out
}

// captureStderr 捕获标准错误（flag 包 usage 走 fs.Output()=stderr）。
func captureStderr(t *testing.T, fn func() error) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	errCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		errCh <- string(b)
	}()
	ferr := fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stderr = old
	out := <-errCh
	if ferr != nil {
		t.Fatalf("cmd err: %v", ferr)
	}
	return out
}

// runExampleLine 切分示例行并经命令入口函数真实执行（与 main() 同一 dispatch 语义）。
func runExampleLine(t *testing.T, line string) error {
	t.Helper()
	tok, err := splitShellWords(line)
	if err != nil {
		t.Fatalf("词法切分失败 %q: %v", line, err)
	}
	if len(tok) < 2 || tok[0] != "agmem" {
		t.Fatalf("非可执行示例行 %q → %v", line, tok)
	}
	args := tok[2:]
	switch tok[1] {
	case "push":
		return cmdPush(args)
	case "ask":
		return cmdAsk(args)
	case "status":
		return cmdStatus(args)
	case "kb":
		return cmdKB(args)
	case "forget":
		return cmdForget(args)
	case "retry":
		return cmdRetry(args)
	case "clear":
		return cmdClear(args)
	case "completion":
		return cmdCompletion(args)
	case "--version":
		return cmdVersion(args)
	default:
		t.Fatalf("未知示例命令 %q（dispatch 与 main() 漂移?）", tok[1])
		return nil
	}
}

// allUsageTexts 全部 usage 常量（新增子命令 usage 常量须同步登记，见 TestUsageConstsRegistered）。
func allUsageTexts() []string {
	return []string{usage, usagePush, usageAsk, usageStatus, usageKB, usageClear,
		usageKBCreate, usageKBDir, usageForget, usageRetry, usageCompletion}
}

// 核心门禁：help 中每条示例都必须真实可执行（err==nil）。
// 漂移场景 → 必红：flag 改名（parse 失败）、端点变更（mock 404 → client 报错）、
// 语义破坏（校验失败）。
func TestHelpExamplesExecutable(t *testing.T) {
	setupCLIMocks(t)
	t.Chdir(t.TempDir())
	if err := os.WriteFile("notes.txt", []byte("第一段\n\n第二段\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	total := 0
	for _, text := range allUsageTexts() {
		for _, line := range extractExampleLines(text) {
			total++
			line := line
			t.Run(fmt.Sprintf("ex%02d", total), func(t *testing.T) {
				t.Logf("exec: %s", line)
				if err := runExampleLine(t, line); err != nil {
					t.Errorf("help 示例与实现漂移（不可执行）: %v", err)
				}
			})
		}
	}
	if total < 10 {
		t.Fatalf("extracted only %d examples — extraction or help content regressed", total)
	}
	t.Logf("共提取并执行 %d 条示例，全部 exit 0", total)
}

// usage 常量登记守卫：防止新 usage 常量加入后漏进可执行性门禁。
func TestUsageConstsRegistered(t *testing.T) {
	// allUsageTexts 覆盖数 ≥ 已知常量数；新增常量时同步登记并让该断言失败提示
	if len(allUsageTexts()) != 11 {
		t.Fatalf("usage consts changed: update allUsageTexts to register new usage strings")
	}
}

// 子命令 --help：打印用法并 exit 0（err==nil；flag.ExitOnError 语义为 exit 2，已修）。
func TestSubcommandHelpExitZero(t *testing.T) {
	cmds := []struct {
		name string
		fn   func([]string) error
	}{
		{"push", cmdPush}, {"ask", cmdAsk}, {"status", cmdStatus}, {"clear", cmdClear},
		{"forget", cmdForget}, {"retry", cmdRetry},
	}
	for _, c := range cmds {
		for _, flag := range []string{"--help", "-h"} {
			if err := c.fn([]string{flag}); err != nil {
				t.Errorf("agmem %s %s: want nil (exit 0), got %v", c.name, flag, err)
			}
		}
	}
	// kb 路由层与 kb 子命令
	for _, args := range [][]string{{"--help"}, {"-h"}, {"create", "--help"}, {"dir", "-h"}} {
		if err := cmdKB(args); err != nil {
			t.Errorf("agmem kb %v: want nil (exit 0), got %v", args, err)
		}
	}
	// help 输出确实含用法与用例（flag 包 usage 走 stderr，非空打印）
	out := captureStderr(t, func() error { return cmdAsk([]string{"--help"}) })
	if !strings.Contains(out, "用例") || !strings.Contains(out, "agmem ask") {
		t.Fatalf("ask --help output missing usage examples:\n%s", out)
	}
}

// 单库版 CLI 默认（UC-3 旧「默认全局 fan-out」语义已随单库化退役）：
// ask 无 --kb → wire 回填 AGMEM_KB（默认 sre），global 不再强制 true；
// 默认文本输出带边界声明行、无 score/version 等易变字段。
func TestCmdAskDefaults(t *testing.T) {
	t.Run("无flag_单库默认_wire回填kb_sre", func(t *testing.T) {
		ctl := setupCLIMocks(t)
		out := captureStdout(t, func() error { return cmdAsk([]string{"统一出口代理地址"}) })
		if ctl.askBody()["kb"] != "sre" || ctl.askBody()["global"] != false {
			t.Fatalf("upstream must receive kb=sre (AGMEM_KB default) global=false, got %v", ctl.askBody())
		}
		// 2026-09-07 来源前缀下线：默认文本只剩正文，无 "[N] sre/云-clean" 标签
		if !strings.Contains(out, "统一出口由 代理网关 承载") || !strings.Contains(out, "第二条正文") {
			t.Fatalf("missing rendered hit bodies, got:\n%s", out)
		}
		// 2026-09-08 输出边界批：末尾固定边界声明行
		if !strings.Contains(out, askBoundaryNote) {
			t.Fatalf("default text output must carry boundary note, got:\n%s", out)
		}
		if strings.Contains(out, "sre/云-clean") || strings.Contains(out, "[1]") ||
			strings.Contains(out, "[2]") {
			t.Fatalf("default text must not leak source labels, got:\n%s", out)
		}
		if strings.Contains(out, "score") || strings.Contains(out, "cache-hit") ||
			strings.Contains(out, "kb_version") || strings.Contains(out, "q-deadbeef") {
			t.Fatalf("default text must not leak volatile fields, got:\n%s", out)
		}
	})
	t.Run("AGMEM_KB覆盖wire回填值", func(t *testing.T) {
		ctl := setupCLIMocks(t)
		t.Setenv("AGMEM_KB", "custom-kb")
		_ = captureStdout(t, func() error { return cmdAsk([]string{"q"}) })
		if ctl.askBody()["kb"] != "custom-kb" {
			t.Fatalf("AGMEM_KB must override wire kb field, got %v", ctl.askBody())
		}
	})
	t.Run("显式kb保留接受_仅回填wire", func(t *testing.T) {
		// --kb 已废弃：仍接受（旧脚本兼容），值仅回填 wire 字段（服务端忽略）
		ctl := setupCLIMocks(t)
		_ = captureStdout(t, func() error { return cmdAsk([]string{"--kb", "legacy-name", "qdrant 维度"}) })
		if ctl.askBody()["kb"] != "legacy-name" || ctl.askBody()["global"] != false {
			t.Fatalf("deprecated --kb must be accepted and echoed on wire, got %v", ctl.askBody())
		}
	})
	t.Run("显式global保留接受_不再强制", func(t *testing.T) {
		ctl := setupCLIMocks(t)
		_ = captureStdout(t, func() error { return cmdAsk([]string{"--global", "qdrant 维度"}) })
		if ctl.askBody()["global"] != true || ctl.askBody()["kb"] != "sre" {
			t.Fatalf("deprecated --global must be accepted, got %v", ctl.askBody())
		}
	})
	t.Run("debug_含诊断行与score_末尾边界声明", func(t *testing.T) {
		_ = setupCLIMocks(t)
		out := captureStdout(t, func() error {
			return cmdAsk([]string{"--debug", "统一出口代理地址"})
		})
		if !strings.HasPrefix(out, "# id=q-deadbeefdeadbeef cache-hit=false version=7\n") {
			t.Fatalf("missing debug header, got:\n%s", out)
		}
		if !strings.Contains(out, "(score=0.83)") {
			t.Fatalf("missing score suffix, got:\n%s", out)
		}
		if !strings.Contains(out, askBoundaryNote) {
			t.Fatalf("debug output must carry boundary note, got:\n%s", out)
		}
	})
	t.Run("json_完整结构_无边界声明", func(t *testing.T) {
		_ = setupCLIMocks(t)
		out := captureStdout(t, func() error {
			return cmdAsk([]string{"--json", "统一出口代理地址"})
		})
		var res askResult
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("not valid askResult json: %v", err)
		}
		if res.ID != "q-deadbeefdeadbeef" || res.KBVersion != 7 {
			t.Fatalf("json shape drifted: %+v", res)
		}
		// --json 结构化契约：不追加边界声明行（消费方自行决定 UI 呈现）
		if strings.Contains(out, askBoundaryNote) {
			t.Fatalf("json output must NOT carry boundary note, got:\n%s", out)
		}
	})
	t.Run("query必填", func(t *testing.T) {
		_ = setupCLIMocks(t)
		if err := cmdAsk(nil); err == nil {
			t.Fatal("query is required")
		}
	})
}

// push 的 -f 示例文件语义回归：空行分段（_UT 与门禁共用 notes.txt 形状）。
func TestSplitParagraphs(t *testing.T) {
	got := splitParagraphs("a\n\nb\nc\n\n\nd")
	want := "a\x00b\nc\x00d"
	if strings.Join(got, "\x00") != want {
		t.Fatalf("got %#v", got)
	}
}

// 兜底：kb 无参数打印用法并报错（exit 1 语义不变）。
func TestKBNoArgsStillErrors(t *testing.T) {
	err := cmdKB(nil)
	if err == nil || !strings.Contains(err.Error(), "create|dir|list|dirs") {
		t.Fatalf("want subcommand-required error, got %v", err)
	}
}

// 环境变量未注入时 push 直接报错（不静默），守卫 newAgmemClient 契约。
func TestPushRequiresEnv(t *testing.T) {
	t.Setenv("AGMEM_URL", "")
	t.Setenv("AGMEM_TOKEN", "")
	err := cmdPush([]string{"--kb", "sre", "--dir", "mysql", "-t", "x"})
	if err == nil || !strings.Contains(err.Error(), "AGMEM_URL") {
		t.Fatalf("want AGMEM_URL error, got %v", err)
	}
}

// —— UC-011：forget / retry 参数解析与 --yes 行为 ——

// setStdin 替换交互输入源（confirm 读取），测试结束恢复。
func setStdin(t *testing.T, s string) {
	t.Helper()
	old := stdinReader
	stdinReader = strings.NewReader(s)
	t.Cleanup(func() { stdinReader = old })
}

func TestCmdForget(t *testing.T) {
	t.Run("yes跳过确认并输出删除点数与版本", func(t *testing.T) {
		_ = setupCLIMocks(t)
		out := captureStdout(t, func() error { return cmdForget([]string{"--id", "42", "--yes"}) })
		if !strings.Contains(out, "item=42") || !strings.Contains(out, "deleted_points=3") ||
			!strings.Contains(out, "version=8") {
			t.Fatalf("forget --yes output drifted: %q", out)
		}
	})
	t.Run("无yes且确认y则执行", func(t *testing.T) {
		_ = setupCLIMocks(t)
		setStdin(t, "y\n")
		out := captureStdout(t, func() error { return cmdForget([]string{"--id", "42"}) })
		if !strings.Contains(out, "已删除") {
			t.Fatalf("confirmed forget must proceed: %q", out)
		}
	})
	t.Run("无yes且拒绝则不调用API", func(t *testing.T) {
		// 独立 server：任何请求都判失败，证明取消后未触达服务端
		hit := false
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			hit = true
		}))
		defer srv.Close()
		t.Setenv("AGMEM_URL", srv.URL)
		t.Setenv("AGMEM_TOKEN", "test-token")
		setStdin(t, "n\n")
		out := captureStdout(t, func() error { return cmdForget([]string{"--id", "42"}) })
		if hit {
			t.Fatal("cancelled forget must not call the API")
		}
		if !strings.Contains(out, "已取消") {
			t.Fatalf("want cancel notice, got %q", out)
		}
	})
	t.Run("EOF视为取消", func(t *testing.T) {
		_ = setupCLIMocks(t)
		setStdin(t, "")
		out := captureStdout(t, func() error { return cmdForget([]string{"--id", "42"}) })
		if !strings.Contains(out, "已取消") {
			t.Fatalf("EOF must cancel, got %q", out)
		}
	})
	t.Run("缺少id报错", func(t *testing.T) {
		_ = setupCLIMocks(t)
		if err := cmdForget(nil); err == nil || !strings.Contains(err.Error(), "--id") {
			t.Fatalf("want --id required error, got %v", err)
		}
	})
}

func TestCmdRetry(t *testing.T) {
	t.Run("输出新状态与版本", func(t *testing.T) {
		_ = setupCLIMocks(t)
		out := captureStdout(t, func() error { return cmdRetry([]string{"--id", "42"}) })
		if !strings.Contains(out, "item=42") || !strings.Contains(out, "status=pending") ||
			!strings.Contains(out, "version=8") {
			t.Fatalf("retry output drifted: %q", out)
		}
	})
	t.Run("缺少id报错", func(t *testing.T) {
		_ = setupCLIMocks(t)
		if err := cmdRetry(nil); err == nil || !strings.Contains(err.Error(), "--id") {
			t.Fatalf("want --id required error, got %v", err)
		}
	})
}

// —— completion：内嵌 bash 补全脚本（随二进制发布，仓库为唯一事实源）——

func TestCmdCompletion(t *testing.T) {
	t.Run("bash输出与仓库文件字节一致", func(t *testing.T) {
		// go test 的 cwd = 包目录 cmd/agmem → ../../scripts/agmem-completion.bash
		want, err := os.ReadFile(filepath.Join("..", "..", "scripts", "agmem-completion.bash"))
		if err != nil {
			t.Fatalf("读取事实源脚本失败: %v", err)
		}
		got := captureStdout(t, func() error { return cmdCompletion([]string{"bash"}) })
		if got != string(want) {
			t.Fatalf("内嵌输出与 scripts/agmem-completion.bash 不一致: got %d bytes, file %d bytes",
				len(got), len(want))
		}
	})
	t.Run("内嵌内容含forget与retry及其flag", func(t *testing.T) {
		for _, want := range []string{"forget", "retry", "--id", "--yes"} {
			if !strings.Contains(scripts.BashCompletion, want) {
				t.Errorf("内嵌补全缺少 %q", want)
			}
		}
	})
	t.Run("bash3_2兼容_不用nosort", func(t *testing.T) {
		if strings.Contains(scripts.BashCompletion, "nosort") {
			t.Fatal("补全脚本不得使用 compgen -o nosort（bash 3.2 不支持）")
		}
	})
	t.Run("无参列出支持的shell", func(t *testing.T) {
		out := captureStdout(t, func() error { return cmdCompletion(nil) })
		if !strings.Contains(out, "bash") || !strings.Contains(out, "zsh") {
			t.Fatalf("无参输出必须列出支持的 shell, got:\n%s", out)
		}
	})
	t.Run("zsh明确不支持且非零退出", func(t *testing.T) {
		err := cmdCompletion([]string{"zsh"})
		if err == nil || !strings.Contains(err.Error(), "暂不支持") {
			t.Fatalf("zsh 必须报「暂不支持」, got %v", err)
		}
	})
	t.Run("未知shell非零退出", func(t *testing.T) {
		if err := cmdCompletion([]string{"fish"}); err == nil {
			t.Fatal("未知 shell 必须非零退出")
		}
	})
	t.Run("多余参数非零退出", func(t *testing.T) {
		if err := cmdCompletion([]string{"bash", "extra"}); err == nil {
			t.Fatal("多余参数必须非零退出")
		}
	})
	t.Run("help退出0并含示例", func(t *testing.T) {
		for _, f := range []string{"--help", "-h", "help"} {
			out := captureStdout(t, func() error { return cmdCompletion([]string{f}) })
			if !strings.Contains(out, "agmem completion bash") {
				t.Fatalf("completion %s 输出缺少示例:\n%s", f, out)
			}
		}
	})
}

// notes.txt 位置契约（-f ./notes.txt 相对 cwd；门禁用例 chdir 后依赖此文件名）。
func TestNotesFileContract(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
}
