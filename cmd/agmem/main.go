// agmem：智能记忆系统 CLI（查询/推送/缓存治理）。
//
// ask/clear 走本地 agmemd 守护（自动拉起）；push/status/kb 调 agmem API（骨架）。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sre/agmem/internal/cleaner"
	"sre/agmem/internal/config"
	"sre/agmem/scripts"
)

const usage = `agmem — 智能记忆系统 CLI

用法: agmem <command> [flags]

命令:
  push     提交一批文本到知识库（异步清洗入库，立刻返回 task id；重复内容自动去重）
  ask      提问（本地 agmemd 缓存校验，未命中则查 agmem 并落缓存）
  status   查看任务进度
  kb       知识库/目录管理
  forget   删除条目（qdrant 点位 + 元数据，不可逆；版本 +1）
  retry    重试失败条目（failed → pending）
  clear    清理本地缓存（--id 单条 / --all 全部）
  completion 输出 shell 补全脚本（bash，内嵌于二进制）
  version  打印构建信息（版本 / git 完整 commit / 构建时间 / 构建参数）

常用示例:
  agmem push --dir mysql -t "binlog 保留 3 天，用 expire_logs_days 控制"
  agmem ask "统一出口代理地址"
  agmem ask --dir k8s --k 10 "node pool 如何配置 delete_option"
  agmem forget --id 42 --yes
  agmem retry --id 42
  agmem completion bash
  agmem --version

单库版（2026-09-08）：产品只有一个知识库；--kb / --global 已废弃
（保留接受，服务端忽略，仅为旧脚本兼容）。
子命令帮助: agmem <command> --help（如 agmem ask --help）

环境变量:
  AGMEM_URL      agmem 地址（例 https://agmem-test.example.com）
  AGMEM_TOKEN    机器 Bearer token
  AGMEM_KB       全局唯一知识库名（默认 sre；仅用于回填 wire 兼容字段）
  AGMEMD_ADDR    本地守护地址（默认 127.0.0.1:8377）
  AGMEMD_DIR     本地缓存目录（默认 ~/.cache/agmemd）

每个命令 -h 查看详细说明与用例。`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "push":
		err = cmdPush(os.Args[2:])
	case "ask":
		err = cmdAsk(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "kb":
		err = cmdKB(os.Args[2:])
	case "forget":
		err = cmdForget(os.Args[2:])
	case "retry":
		err = cmdRetry(os.Args[2:])
	case "clear":
		err = cmdClear(os.Args[2:])
	case "completion":
		err = cmdCompletion(os.Args[2:])
	case "--version", "-v", "version":
		err = cmdVersion(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agmem:", err)
		os.Exit(1)
	}
}

// —— agmemd 本地守护 ——

func ctldBase() string {
	return "http://" + getenv("AGMEMD_ADDR", "127.0.0.1:8377")
}

// ensureDaemon 探测 agmemd；未运行则拉起同目录 agmemd 二进制并等 healthz。
func ensureDaemon() error {
	base := ctldBase()
	if probeHealthz(base) {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位自身失败: %w", err)
	}
	daemon := filepath.Join(filepath.Dir(exe), "agmemd")
	if _, err := os.Stat(daemon); err != nil {
		return fmt.Errorf("未找到 agmemd（%s）：请先构建/安装（见 README）；ask 需要本地守护", daemon)
	}
	cacheDir := getenv("AGMEMD_DIR", filepath.Join(homeDir(), ".cache", "agmemd"))
	_ = os.MkdirAll(cacheDir, 0o700)
	logFile, err := os.OpenFile(filepath.Join(cacheDir, "agmemd.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(daemon)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // 脱离父进程
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 agmemd: %w", err)
	}
	fmt.Fprintf(os.Stderr, "agmem: 已拉起 agmemd (pid=%d)，等待就绪（首次会下载模型，见 %s）...\n",
		cmd.Process.Pid, filepath.Join(cacheDir, "agmemd.log"))
	// 等待 healthz；模型下载最长等 10 分钟
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		if probeHealthz(base) {
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("agmemd 未在 10 分钟内就绪（查看 %s）", filepath.Join(cacheDir, "agmemd.log"))
}

func probeHealthz(base string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	var h struct {
		Status string `json:"status"`
	}
	return resp.StatusCode == 200 && json.Unmarshal(b, &h) == nil && h.Status != ""
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// parseFlags 统一 flagset 解析（ContinueOnError）：
//   - -h/--help：flag 包已打印用法 → handled=true, err=nil（调用方 return nil，进程 exit 0）
//     ——ExitOnError 对未定义 -h 的默认行为是 exit 2，不符合「agmem <cmd> --help 打印用法」预期；
//   - 其余解析错误：handled=true, err≠nil（由 main 统一打印并 exit 1）；
//   - 解析成功：handled=false，调用方继续正常流程。
func parseFlags(fs *flag.FlagSet, args []string) (handled bool, err error) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, nil
		}
		return true, err
	}
	return false, nil
}

// splitShellWords 极简 POSIX 风格分词：空白分隔；单/双引号内空白不切分，
// 双引号内支持 \" \\ 转义。用于 help 示例行 → argv（可执行性 UT，UC-4）。
func splitShellWords(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inToken := false
	i := 0
	flush := func() {
		if inToken {
			out = append(out, cur.String())
			cur.Reset()
			inToken = false
		}
	}
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			flush()
			i++
		case c == '\'':
			inToken = true
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 2
		case c == '"':
			inToken = true
			i++
			closed := false
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					cur.WriteByte(s[i+1])
					i += 2
					continue
				}
				if s[i] == '"' {
					closed = true
					i++
					break
				}
				cur.WriteByte(s[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated double quote")
			}
		default:
			inToken = true
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return out, nil
}

// ctldPost 调 agmemd POST /api/*。
func ctldPost(path string, req any, out any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := http.Post(ctldBase()+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("agmemd 未运行或不可达: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return fmt.Errorf("agmemd: %s", e.Error)
		}
		return fmt.Errorf("agmemd: status %d: %s", resp.StatusCode, truncateBytes(body))
	}
	return json.Unmarshal(body, out)
}

func truncateBytes(b []byte) string {
	if len(b) > 256 {
		return string(b[:256]) + "..."
	}
	return string(b)
}

// —— 命令 ——

const usagePush = `push — 提交文本到知识库（单库，异步，立刻返回 task id）

用例:
  agmem push --dir mysql -t "binlog 保留 3 天，用 expire_logs_days 控制"
  agmem push --dir k8s -f ./notes.txt

说明:
  单库版（2026-09-08）：只有一个知识库，--kb 已废弃（保留接受，服务端忽略）。
  入库去重：与库内已有条目原文一致的文本自动跳过；全部重复时不建任务，
  返回 task_id=0 status=duplicate。
  提交后原始文本先 embedding 入库（可立即查询），随后 GLM-5.3-flash 自动清洗，
  清洗结果落入 <dir>-clean 目录。用 agmem status --task <id> 查看进度。
`

func cmdPush(args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	kb := fs.String("kb", "", "（已废弃，单库版忽略）目标知识库名——仅为旧脚本兼容保留")
	dir := fs.String("dir", "", "目标 1 级目录")
	file := fs.String("f", "", "文本文件路径（- 表示 stdin），每行/每段一条")
	text := fs.String("t", "", "内联单条文本")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usagePush)
		fs.PrintDefaults()
	}
	if done, err := parseFlags(fs, args); done {
		return err
	}
	if *dir == "" || (*file == "" && *text == "") {
		fs.Usage()
		return fmt.Errorf("需要 --dir 且 --f/--t 至少其一")
	}
	// 单库化：--kb 已废弃，wire 上回填全局唯一库名（AGMEM_KB，默认 sre）——
	// 旧版服务端按 URL 定位 kb 需要非空名；新版服务端忽略该字段。
	kbName := *kb
	if kbName == "" {
		kbName = getenv("AGMEM_KB", config.DefaultKB)
	}
	client, err := newAgmemClient()
	if err != nil {
		return err
	}
	var texts []string
	if *text != "" {
		texts = append(texts, *text)
	}
	if *file != "" {
		var data []byte
		if *file == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(*file)
		}
		if err != nil {
			return err
		}
		// 空行分段：每段一条记忆（单段文件即一条）
		for _, para := range splitParagraphs(string(data)) {
			texts = append(texts, para)
		}
	}
	if len(texts) == 0 {
		return fmt.Errorf("没有可提交的文本")
	}
	req := map[string]any{"dir": *dir, "texts": texts}
	var out struct {
		TaskID int64  `json:"task_id"`
		Items  int    `json:"items"`
		Status string `json:"status"`
		Hint   string `json:"hint"`
	}
	if err := client.do(http.MethodPost, "/api/kb/"+kbName+"/docs", req, &out); err != nil {
		return err
	}
	fmt.Printf("task_id=%d items=%d status=%s\n%s\n", out.TaskID, out.Items, out.Status, out.Hint)
	return nil
}

// splitParagraphs 空行分段（连续空白行 = 分隔符）。
func splitParagraphs(s string) []string {
	var out []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return out
}

type askResult struct {
	CacheHit   bool            `json:"cache_hit"`
	ID         string          `json:"id"`
	Similarity float64         `json:"similarity"`
	KBVersion  uint64          `json:"kb_version"`
	Result     json.RawMessage `json:"result"`
}

const usageAsk = `ask — 提问检索记忆（单知识库）

用例:
  agmem ask "统一出口代理地址"
  agmem ask --dir k8s --k 10 "node pool 如何配置 delete_option"
  agmem ask --json "缓存命中边界"
  agmem ask --max-len 200 "长条目只看前 200 字"

说明:
  单库版（2026-09-08）：产品只有一个知识库，检索范围恒为全库，--dir 可收窄到
  单 1 级目录（查询侧自动匹配清洗目录 <dir>-clean，直接写 --dir k8s 即可，
  已带 -clean 后缀同样接受）；--kb / --global 已废弃（保留接受，服务端忽略，
  仅为旧脚本兼容）。
  自动拉起本地 agmemd（首次会下载 bge-base-zh 模型，~100MB）。
  流程：版本校验（/api/version/{kb}）→ 本地历史问题相似度 >0.8 且版本一致 →
  本地秒回；否则查 agmem 并落缓存。本地缓存按 (dir, question) 隔离。

输出:
  默认纯文本仅条目正文（条目间空行分段；不暴露来源 kb/dir/编号，缓存友好：
  无 score/版本/id 等易变字段）；hits 非空时末尾固定追加一行边界声明——
  「（以上为检索到的参考素材，可能过时或不完整，使用前请核实原文）」，
  检索内容仅作参考素材、使用前须核实原文。
  空结果输出「无可用知识」；单条正文超 --max-len 字截断并标注「…(截断,N字)」
  （N=原文字数）；--json 完整 JSON（程序消费，含 score/kb/dir 与 hybrid 的
  dense_score/sparse_rank/sparse_score，永远全文，不截断、不追加边界行——
  结构化消费方自行决定 UI 呈现）；--debug 文本 + 诊断行
  （id/cache-hit/version/sim）+ 阈值过滤诊断行（mode/threshold/rule/kept/
  dropped/reason，说明命中为何被阈值过滤）与每条 [N] kb/dir (score) 标签
  （截断与边界行同默认文本）。
`

// askBoundaryNote 查询输出边界声明（2026-09-08 用户拍板）：检索内容仅是参考
// 素材、可能过时或不完整，消费方（agent/人）使用前必须核实原文。
// 仅默认 text 与 --debug 模式在 hits 非空时追加；--json 不追加（结构化消费方
// 自行决定 UI 呈现）。
const askBoundaryNote = "（以上为检索到的参考素材，可能过时或不完整，使用前请核实原文）"

type askMode int

const (
	modeText  askMode = iota // 默认缓存友好文本
	modeDebug                // 文本 + 诊断行 + score 后缀
	modeJSON                 // 完整 JSON（程序消费）
)

func cmdAsk(args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	kb := fs.String("kb", "", "（已废弃，单库版忽略）知识库名——仅为旧脚本兼容保留")
	dir := fs.String("dir", "", "限定 1 级目录")
	global := fs.Bool("global", false, "（已废弃，单库版忽略）旧版跨库检索开关")
	topK := fs.Int("k", 5, "返回条数")
	// 分条批（2026-09-07）：展示截断默认值 = 分条硬上限（cleaner.MaxEntryLen），
	// 单源保证「GLM 约束 = 分条上限 = 查询截断默认值」三方统一 L=600。
	maxLen := fs.Int("max-len", cleaner.MaxEntryLen, "text/debug 单条正文字数上限，0=不截断（--json 永远全文）")
	asJSON := fs.Bool("json", false, "输出完整 JSON（程序消费）")
	debug := fs.Bool("debug", false, "文本输出 + 诊断行（id/cache-hit/version/相似度/阈值过滤原因）")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageAsk)
		fs.PrintDefaults()
	}
	if done, err := parseFlags(fs, args); done {
		return err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return fmt.Errorf("需要一个问题文本")
	}
	if err := ensureDaemon(); err != nil {
		return err
	}
	query := ""
	for i, a := range fs.Args() {
		if i > 0 {
			query += " "
		}
		query += a
	}
	var out askResult
	// 单库化：不再要求/选择 kb/global 语义；wire 上回填全局唯一库名（AGMEM_KB，
	// 默认 sre）——旧版 agmemd 需要非空 kb 才受理，新版 daemon/服务端一律忽略。
	kbName := *kb
	if kbName == "" {
		kbName = getenv("AGMEM_KB", config.DefaultKB)
	}
	req := map[string]any{"kb": kbName, "dir": *dir, "query": query, "top_k": *topK, "global": *global}
	if err := ctldPost("/api/ask", req, &out); err != nil {
		return err
	}
	mode := modeText
	if *asJSON {
		mode = modeJSON
	} else if *debug {
		mode = modeDebug
	}
	fmt.Println(renderAsk(out, mode, *maxLen))
	return nil
}

// renderAsk ask 结果渲染纯函数（UC-2）：
//
//	modeText   仅条目正文，条目间 "\n\n" 分隔；默认不暴露分库设计——
//	           无 [N] 编号、kb/dir、score、版本、id 等字样（2026-09-07 用户反馈：
//	           来源前缀泄漏内部设计，且对 agent prompt 无消费价值；
//	           保护 agent prompt cache）；全被过滤/无命中 → 「无可用知识」。
//	           hits 非空时末尾固定追加一行 askBoundaryNote（2026-09-08 用户拍板：
//	           检索内容仅作参考素材，使用前须核实原文）
//	modeDebug  首行 "# id=... cache-hit=... version=... sim=..."（sim 仅命中时），
//	           每条恢复 "[N] kb/dir (score=0.62)" 标签（来源仅调试可见）；
//	           边界行同 modeText 追加
//	modeJSON   完整 askResult JSON（含 score/kb/dir，供程序消费）；不追加边界行
//	           （结构化消费方自行决定 UI 呈现）
//	maxLen     text/debug 模式单条正文字数（rune）截断上限，0=不截断；JSON 永远全文
//	           （截断只影响展示层 token：服务端返回全文、本地缓存存全文）
//
// 上游结果非 JSON（解析失败）→ 原样返回，不吞错误信息。
func renderAsk(res askResult, mode askMode, maxLen int) string {
	if mode == modeJSON {
		enc, _ := json.MarshalIndent(res, "", "  ")
		return string(enc)
	}
	var resp struct {
		KB   string `json:"kb"`
		Hits []struct {
			Score   float64 `json:"score"`
			Payload struct {
				KB   string `json:"kb"`
				Dir  string `json:"dir"`
				Text string `json:"text"`
			} `json:"payload"`
		} `json:"hits"`
		// Filter 服务端过滤诊断（G06）：mode/threshold/rule/kept/dropped/reason，
		// 以及近重复折叠数 deduped（返回条数 = kept - deduped，见 internal/api/dedupe.go）。
		Filter *struct {
			Mode          string  `json:"mode"`
			Threshold     float64 `json:"threshold"`
			Rule          string  `json:"rule"`
			Kept          int     `json:"kept"`
			Dropped       int     `json:"dropped"`
			DroppedReason string  `json:"dropped_reason"`
			Deduped       int     `json:"deduped"`
		} `json:"filter"`
	}
	if err := json.Unmarshal(res.Result, &resp); err != nil {
		return string(res.Result)
	}
	var b strings.Builder
	if mode == modeDebug {
		fmt.Fprintf(&b, "# id=%s cache-hit=%t version=%d", res.ID, res.CacheHit, res.KBVersion)
		if res.CacheHit {
			fmt.Fprintf(&b, " sim=%.4f", res.Similarity)
		}
		b.WriteString("\n")
		if f := resp.Filter; f != nil {
			fmt.Fprintf(&b, "# filter: mode=%s threshold=%.2f rule=%s kept=%d dropped=%d",
				f.Mode, f.Threshold, f.Rule, f.Kept, f.Dropped)
			if f.Dropped > 0 && f.DroppedReason != "" {
				fmt.Fprintf(&b, " reason=%s", f.DroppedReason)
			}
			// 近重复折叠数：返回条数 = kept - deduped
			if f.Deduped > 0 {
				fmt.Fprintf(&b, " deduped=%d", f.Deduped)
			}
			b.WriteString("\n")
		}
	}
	if len(resp.Hits) == 0 {
		b.WriteString("无可用知识")
		return b.String()
	}
	for i, h := range resp.Hits {
		if i > 0 {
			// 多条答案换行分段（用户对齐 2026-09-07）：贴近 GLM 入库自然语言，
			// 零分隔线/零标签/零元数据；来源前缀仅 --debug 可见
			b.WriteString("\n\n")
		}
		if mode == modeDebug {
			fmt.Fprintf(&b, "[%d] %s", i+1, askHitLabel(resp.KB, h.Payload.KB, h.Payload.Dir))
			fmt.Fprintf(&b, " (score=%.2f)", h.Score)
			b.WriteString("\n")
		}
		b.WriteString(truncateRunes(h.Payload.Text, maxLen))
	}
	// 边界声明（2026-09-08）：末尾固定一行，独立于条目正文
	b.WriteString("\n")
	b.WriteString(askBoundaryNote)
	return b.String()
}

// truncateRunes 按「字」（rune）截断正文并追加尾标「…(截断,N字)」（N=原文字数）。
// n<=0 或未超长时原样返回。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("…(截断,%d字)", len(r))
}

// askHitLabel 条目标签 kb/dir（payload.kb 缺失回落响应级 kb；空段省略）。
func askHitLabel(respKB, hitKB, dir string) string {
	kb := hitKB
	if kb == "" {
		kb = respKB
	}
	switch {
	case kb != "" && dir != "":
		return kb + "/" + dir
	case kb != "":
		return kb
	case dir != "":
		return dir
	default:
		return ""
	}
}

const usageStatus = `status — 查看任务进度

用例:
  agmem status --task 42

说明: 显示任务内各条目的 pending→embedded→cleaning→cleaned/failed 状态。
单库版（2026-09-08）：--kb 已废弃（保留接受，服务端按全局唯一库处理）。
`

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	task := fs.Int64("task", 0, "task id（push 返回）")
	kb := fs.String("kb", "", "（已废弃，单库版忽略）知识库名——仅为旧脚本兼容保留")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageStatus)
		fs.PrintDefaults()
	}
	if done, err := parseFlags(fs, args); done {
		return err
	}
	if *task <= 0 {
		fs.Usage()
		return fmt.Errorf("需要 --task <id>")
	}
	kbName := *kb
	if kbName == "" {
		kbName = getenv("AGMEM_KB", config.DefaultKB)
	}
	client, err := newAgmemClient()
	if err != nil {
		return err
	}
	var t apiTask
	if err := client.do(http.MethodGet,
		fmt.Sprintf("/api/kb/%s/tasks/%d", kbName, *task), nil, &t); err != nil {
		return err
	}
	counts := map[string]int{}
	for _, it := range t.Items {
		counts[it.Status]++
	}
	fmt.Printf("task=%d status=%s items=%d", t.ID, t.Status, len(t.Items))
	for _, st := range []string{"pending", "embedded", "cleaning", "cleaned", "failed"} {
		if counts[st] > 0 {
			fmt.Printf(" %s=%d", st, counts[st])
		}
	}
	fmt.Println()
	for _, it := range t.Items {
		line := fmt.Sprintf("  item %-6d %-8s %.60s", it.ID, it.Status, it.OriginalText)
		if it.Error != nil && *it.Error != "" {
			line += " | err: " + *it.Error
		}
		fmt.Println(line)
	}
	return nil
}

const usageKB = `kb — 知识库/目录管理

用法: agmem kb <create|dir|list|dirs> [flags]
用例:
  agmem kb create --name sre
  agmem kb dir --kb sre --name mysql
  agmem kb list
  agmem kb dirs --kb sre
`

const usageKBCreate = `kb create — 创建知识库（1 级目录模型，同名冲突 409）

用例:
  agmem kb create --name sre
`

const usageKBDir = `kb dir — 建 1 级目录

用例:
  agmem kb dir --kb sre --name mysql
`

func cmdKB(args []string) error {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, usageKB)
		return fmt.Errorf("需要子命令 create|dir|list|dirs")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usageKB)
		return nil
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "create":
		fs := flag.NewFlagSet("kb create", flag.ContinueOnError)
		name := fs.String("name", "", "知识库名")
		fs.Usage = func() {
			fmt.Fprint(fs.Output(), usageKBCreate)
			fs.PrintDefaults()
		}
		if done, err := parseFlags(fs, rest); done {
			return err
		}
		if *name == "" {
			fs.Usage()
			return fmt.Errorf("需要 --name")
		}
		client, err := newAgmemClient()
		if err != nil {
			return err
		}
		var out struct {
			ID int64 `json:"id"`
		}
		if err := client.do(http.MethodPost, "/api/kb", map[string]string{"name": *name}, &out); err != nil {
			return err
		}
		fmt.Printf("已创建 kb=%s id=%d\n", *name, out.ID)
	case "dir":
		fs := flag.NewFlagSet("kb dir", flag.ContinueOnError)
		kb := fs.String("kb", "", "知识库名")
		name := fs.String("name", "", "1 级目录名（禁 /）")
		fs.Usage = func() {
			fmt.Fprint(fs.Output(), usageKBDir)
			fs.PrintDefaults()
		}
		if done, err := parseFlags(fs, rest); done {
			return err
		}
		if *kb == "" || *name == "" {
			fs.Usage()
			return fmt.Errorf("需要 --kb --name")
		}
		client, err := newAgmemClient()
		if err != nil {
			return err
		}
		var out struct {
			ID int64 `json:"id"`
		}
		if err := client.do(http.MethodPost, "/api/kb/"+*kb+"/dir",
			map[string]string{"name": *name}, &out); err != nil {
			return err
		}
		fmt.Printf("已创建 dir=%s (kb=%s id=%d)\n", *name, *kb, out.ID)
	case "list":
		client, err := newAgmemClient()
		if err != nil {
			return err
		}
		var out struct {
			KBs []apiKB `json:"kbs"`
		}
		if err := client.do(http.MethodGet, "/api/kb", nil, &out); err != nil {
			return err
		}
		if len(out.KBs) == 0 {
			fmt.Println("(无可见知识库)")
			return nil
		}
		fmt.Printf("%-16s %-6s %8s %8s %10s %s\n", "NAME", "ROLE", "DIRS", "ITEMS", "VERSION", "LAST_TASK")
		for _, k := range out.KBs {
			role := k.Role
			if role == "" {
				role = "machine"
			}
			last := "-"
			if k.LastTaskID > 0 {
				last = fmt.Sprintf("#%d %s", k.LastTaskID, k.LastTaskSt)
			}
			fmt.Printf("%-16s %-6s %8d %8d %10d %s\n", k.Name, role, k.DirCount, k.ItemCount, k.Version, last)
		}
	case "dirs":
		fs := flag.NewFlagSet("kb dirs", flag.ContinueOnError)
		kb := fs.String("kb", "", "知识库名")
		if done, err := parseFlags(fs, rest); done {
			return err
		}
		if *kb == "" {
			fs.Usage()
			return fmt.Errorf("需要 --kb")
		}
		client, err := newAgmemClient()
		if err != nil {
			return err
		}
		var out struct {
			Dirs []apiDir `json:"dirs"`
		}
		if err := client.do(http.MethodGet, "/api/kb/"+*kb+"/dirs", nil, &out); err != nil {
			return err
		}
		for _, d := range out.Dirs {
			fmt.Printf("%-24s id=%d\n", d.Name, d.ID)
		}
	default:
		return fmt.Errorf("未知子命令 %q（create|dir|list|dirs）", sub)
	}
	return nil
}

const usageForget = `forget — 删除条目（不可逆：qdrant 点位 + 元数据 + 版本 +1）

用例:
  agmem forget --id 42 --yes

说明:
  删除指定条目（--id = task_items.id）的全部向量点（original + 全部 clean 条目）
  与元数据，并递增 kb 版本使本地缓存失效（agent 不再命中已删知识）。
  无 --yes 时先交互确认（输入 y/yes 继续，其余取消）。
  条目不存在（含二次调用）→ 服务端 404，不静默成功。
  单库版（2026-09-08）：--kb 已废弃（保留接受，服务端按全局唯一库处理）。
`

func cmdForget(args []string) error {
	fs := flag.NewFlagSet("forget", flag.ContinueOnError)
	id := fs.Int64("id", 0, "条目 id（task_items.id，agmem status 可见）")
	yes := fs.Bool("yes", false, "跳过交互确认（脚本/CI 用）")
	kb := fs.String("kb", "", "（已废弃，单库版忽略）知识库名——仅为旧脚本兼容保留")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageForget)
		fs.PrintDefaults()
	}
	if done, err := parseFlags(fs, args); done {
		return err
	}
	if *id <= 0 {
		fs.Usage()
		return fmt.Errorf("需要 --id <item_id>")
	}
	if !*yes && !confirm(fmt.Sprintf("确认删除条目 %d？该操作不可恢复 [y/N] ", *id)) {
		fmt.Println("已取消（未删除）")
		return nil
	}
	kbName := *kb
	if kbName == "" {
		kbName = getenv("AGMEM_KB", config.DefaultKB)
	}
	client, err := newAgmemClient()
	if err != nil {
		return err
	}
	var out struct {
		DeletedPoints int    `json:"deleted_points"`
		Version       uint64 `json:"version"`
	}
	path := "/api/kb/" + kbName + "/items/" + strconv.FormatInt(*id, 10)
	if err := client.do(http.MethodDelete, path, nil, &out); err != nil {
		return err
	}
	fmt.Printf("已删除 item=%d deleted_points=%d version=%d\n", *id, out.DeletedPoints, out.Version)
	return nil
}

const usageRetry = `retry — 重试失败条目（failed → pending，worker 自动重跑）

用例:
  agmem retry --id 42

说明:
  仅 failed 条目可重试（重排为 pending，worker 按 ≤3 次指数退避重跑）；
  非 failed（cleaning/cleaned 等）→ 服务端 409，不改变状态。
  成功后 kb 版本 +1（本地缓存失效）。
  单库版（2026-09-08）：--kb 已废弃（保留接受，服务端按全局唯一库处理）。
`

func cmdRetry(args []string) error {
	fs := flag.NewFlagSet("retry", flag.ContinueOnError)
	id := fs.Int64("id", 0, "条目 id（task_items.id，agmem status 可见）")
	kb := fs.String("kb", "", "（已废弃，单库版忽略）知识库名——仅为旧脚本兼容保留")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageRetry)
		fs.PrintDefaults()
	}
	if done, err := parseFlags(fs, args); done {
		return err
	}
	if *id <= 0 {
		fs.Usage()
		return fmt.Errorf("需要 --id <item_id>")
	}
	kbName := *kb
	if kbName == "" {
		kbName = getenv("AGMEM_KB", config.DefaultKB)
	}
	client, err := newAgmemClient()
	if err != nil {
		return err
	}
	var out struct {
		Status  string `json:"status"`
		Version uint64 `json:"version"`
	}
	path := "/api/kb/" + kbName + "/items/" + strconv.FormatInt(*id, 10) + "/retry"
	if err := client.do(http.MethodPost, path, nil, &out); err != nil {
		return err
	}
	fmt.Printf("item=%d status=%s version=%d\n", *id, out.Status, out.Version)
	return nil
}

const usageCompletion = `completion — 输出 shell 补全脚本（内嵌于二进制，随版本发布）

用法: agmem completion <shell>

支持的 shell:
  bash    输出 bash 补全脚本（macOS bash 3.2 兼容）
  zsh     暂不支持（非零退出）

用例:
  agmem completion bash

说明:
  补全脚本经 go:embed 内嵌，以仓库 scripts/agmem-completion.bash 为唯一事实源，
  随二进制发布、与 CLI 同版本。安装：将 agmem completion bash 的输出重定向到
  ~/.local/share/bash-completion/completions/agmem，或 source <(agmem completion bash)。
`

// cmdCompletion 输出内嵌的 shell 补全脚本；无参列出支持的 shell，未知/不支持 shell 非零退出。
func cmdCompletion(args []string) error {
	if len(args) == 0 {
		fmt.Print(usageCompletion)
		return nil
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(usageCompletion)
		return nil
	}
	if len(args) > 1 {
		return fmt.Errorf("completion 只接受一个 shell 参数，收到多余参数 %q", strings.Join(args[1:], " "))
	}
	switch args[0] {
	case "bash":
		fmt.Print(scripts.BashCompletion)
		return nil
	case "zsh":
		return fmt.Errorf("暂不支持 shell %q（当前仅支持 bash）", "zsh")
	default:
		return fmt.Errorf("未知 shell %q（当前仅支持 bash）", args[0])
	}
}

// stdinReader 交互确认输入源（测试可替换；默认 os.Stdin）。
var stdinReader io.Reader = os.Stdin

// confirm 交互确认：读取一行，y/yes（大小写不敏感）视为确认；EOF/其它输入 = 否。
// 提示写 stderr（不污染 stdout 的结构化输出）。
func confirm(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt)
	sc := bufio.NewScanner(stdinReader)
	if !sc.Scan() {
		fmt.Fprintln(os.Stderr)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(sc.Text())) {
	case "y", "yes":
		return true
	}
	return false
}

const usageClear = `clear — 清理本地查询缓存（~/.cache/agmemd/，默认 500MB LRU）

用例:
  agmem clear --all
  agmem clear --id q-3f2a1b0c9d8e7f6a

说明:
  --id 单条（ask 输出的 id）；--all 全部。输出释放字节数。
  注意：清的是本地缓存，不影响云端知识库。
`

func cmdClear(args []string) error {
	fs := flag.NewFlagSet("clear", flag.ContinueOnError)
	id := fs.String("id", "", "按唯一提问 id 清除")
	all := fs.Bool("all", false, "清空全部缓存")
	asJSON := fs.Bool("json", false, "输出 JSON")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageClear)
		fs.PrintDefaults()
	}
	if done, err := parseFlags(fs, args); done {
		return err
	}
	if !*all && *id == "" {
		fs.Usage()
		return fmt.Errorf("需要 --id 或 --all")
	}
	req := map[string]any{"id": *id, "all": *all}
	var out struct {
		FreedBytes int64 `json:"freed_bytes"`
	}
	if err := ctldPost("/api/clear", req, &out); err != nil {
		return err
	}
	if *asJSON {
		enc, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(enc))
		return nil
	}
	fmt.Printf("已清理本地缓存 %d bytes\n", out.FreedBytes)
	return nil
}
