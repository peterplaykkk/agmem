// Package version：二进制构建信息（版本 / git commit / 构建时间 / 构建参数）。
//
// 数据来源分两层：
//   - Version / BuildDate：构建时经 `-X main.version=` / `-X main.buildDate=` 注入
//     （见 README 的构建一节）；未注入时 Version=dev、BuildDate 空。
//   - commit / commit-time / go / target / cgo / flags：运行时读 Go 内嵌构建信息
//     （runtime/debug.ReadBuildInfo），**无需构建方配合**，`go run` 下也成立。
//
// 之所以不把 commit 也做成 -X 注入：Go 自 1.18 起在二进制内嵌 VCS 信息
// （vcs.revision 即完整 40 位 hash），注入反而会在 `-buildvcs=false` 或非 git
// 目录构建时静默变成空串。
//
// 本包被 cmd/agmem 与 cmd/agmemd 共用：注入变量留在各自 main 包（与
// cmd/agmemserv 的 `-X main.version` 惯例一致），由调用方传入本包汇总。
package version

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
)

// listedKeys 已单列展示的构建信息键（不重复计入 flags）。
var listedKeys = []string{
	"CGO_ENABLED", "GOOS", "GOARCH", "GOARM64", "GOAMD64",
	"vcs", "vcs.revision", "vcs.time", "vcs.modified",
}

// envOnlyKeys 不是「构建参数」而是 Go 运行时/环境默认值的键，不进 flags 行。
//
// DefaultGODEBUG 是工具链按 go.mod 语言版本推出的 GODEBUG 默认集合，长度可达
// 数百字符且在每次升级 Go 时变化，列出来会淹没真正的构建选项。
// 实测（go1.27.0 darwin/arm64）：`-ldflags` 不会被写进 Go 构建信息，
// 因此 `-s -w` 这类链接参数在 flags 行看不到——要核对完整构建参数请看
// README 的构建配方。
var envOnlyKeys = []string{"DefaultGODEBUG"}

// Unknown 取值缺失时的占位：不臆造、也不留空串（空串会被读成"没有这个字段"）。
const Unknown = "unknown"

// Info 构建信息（字段名即 --json 的键）。
type Info struct {
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	Commit     string   `json:"commit"`
	CommitTime string   `json:"commit_time"`
	Built      string   `json:"built"`
	Go         string   `json:"go"`
	Target     string   `json:"target"`
	CGO        string   `json:"cgo"`
	Modified   string   `json:"modified"`
	Flags      []string `json:"flags"`
}

// Collect 汇总构建信息（纯函数，便于表驱动测试）。
func Collect(name string, settings []debug.BuildSetting, goVersion, ver, built string) Info {
	m := make(map[string]string, len(settings))
	for _, s := range settings {
		m[s.Key] = s.Value
	}
	unknown := func(s string) string {
		if s == "" {
			return Unknown
		}
		return s
	}
	listed := make(map[string]bool, len(listedKeys)+len(envOnlyKeys))
	for _, k := range listedKeys {
		listed[k] = true
	}
	for _, k := range envOnlyKeys {
		listed[k] = true
	}
	// flags：非单列项 + 全部编译选项（"-" 前缀），按名排序保证输出稳定可比。
	var flags []string
	for _, s := range settings {
		if listed[s.Key] {
			continue
		}
		flags = append(flags, s.Key+"="+s.Value)
	}
	sort.Strings(flags)
	// target 需两者俱全才成立：只有一半时拼接出的 "linux/" 会被误读成有效平台。
	target := ""
	if m["GOOS"] != "" && m["GOARCH"] != "" {
		target = m["GOOS"] + "/" + m["GOARCH"]
	}
	return Info{
		Name:       unknown(name),
		Version:    unknown(ver),
		Commit:     unknown(m["vcs.revision"]),
		CommitTime: unknown(m["vcs.time"]),
		Built:      unknown(built),
		Go:         unknown(goVersion),
		Target:     unknown(target),
		CGO:        unknown(m["CGO_ENABLED"]),
		Modified:   unknown(m["vcs.modified"]),
		Flags:      flags,
	}
}

// Read 读当前二进制的构建信息；无内嵌信息时仍返回含注入值与运行时值的结构
// （字段为 unknown 而非空串）。
func Read(name, ver, built string) Info {
	var settings []debug.BuildSetting
	goVersion := runtime.Version()
	if bi, ok := debug.ReadBuildInfo(); ok {
		settings = bi.Settings
		if bi.GoVersion != "" {
			goVersion = bi.GoVersion
		}
	}
	return Collect(name, settings, goVersion, ver, built)
}

// WriteText 打印人类可读形态（首行为 "<name> <version>"）。
func (i Info) WriteText(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "%s %s\n", i.Name, i.Version); err != nil {
		return err
	}
	for _, f := range [][2]string{
		{"commit", i.Commit},
		{"commit-time", i.CommitTime},
		{"built", i.Built},
		{"go", i.Go},
		{"target", i.Target},
		{"cgo", i.CGO},
		{"modified", i.Modified},
	} {
		if _, err := fmt.Fprintf(w, "  %-12s %s\n", f[0], f[1]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "  %-12s %s\n", "flags", strings.Join(i.Flags, " ")); err != nil {
		return err
	}
	if i.Built == Unknown {
		// 提示而非静默：commit-time 是提交时间，容易被误当成构建时间。
		if _, err := fmt.Fprintln(w, "提示: 构建时未注入 -X main.buildDate=<RFC3339>；commit-time 是提交时间，不是构建时间。"); err != nil {
			return err
		}
	}
	return nil
}

// WriteJSON 结构化输出（字段与 WriteText 标签一一对应）。
func (i Info) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(i)
}
