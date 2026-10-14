// version.go：`agmem --version`（构建信息见 internal/version 包）。
//
// version / buildDate 由构建时 -X 注入（默认 dev / 空）——变量留在 main 包，
// 与 cmd/agmemserv 的 `-X main.version` 惯例一致。
package main

import (
	"flag"
	"fmt"
	"os"

	verinfo "sre/agmem/internal/version"
)

var (
	version   = "dev"
	buildDate = ""
)

const usageVersion = `agmem version — 打印构建信息（版本 / 提交 / 构建时间 / 构建参数）

用法: agmem --version [--json]
等价: agmem -v、agmem version

输出字段:
  version      版本标签（构建时 -X main.version 注入；未注入为 dev）
  commit       git 完整 commit hash（Go 内嵌 VCS 信息；无 VCS 时为 unknown）
  commit-time  该提交的时间（来自 git，与「构建时间」不同）
  built        构建时间（构建时 -X main.buildDate 注入；未注入为 unknown）
  go           Go 工具链版本
  target       GOOS/GOARCH
  cgo          CGO_ENABLED（agmemd 本地 embedding 需 1）
  modified     构建时工作区是否有未提交改动（true = 产物不可复现，需留意）
  flags        其余构建参数（-buildmode / -compiler / -trimpath 等，按名排序；
               Go 不记录 -ldflags，故 -s -w 不在此列；Go 运行时默认值如
               DefaultGODEBUG 不是构建参数，不列出）

常用示例:
  agmem --version
  agmem --version --json

说明: commit 与 flags 直接取自二进制内嵌的 Go 构建信息，因此「已安装的二进制
来自哪个提交」无需 Go 工具链即可核对。
`

func cmdVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), usageVersion) }
	jsonOut := fs.Bool("json", false, "以 JSON 输出（字段同文本标签）")
	if done, err := parseFlags(fs, args); done {
		return err
	}

	vi := verinfo.Read("agmem", version, buildDate)
	if *jsonOut {
		return vi.WriteJSON(os.Stdout)
	}
	return vi.WriteText(os.Stdout)
}
