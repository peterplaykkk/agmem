// version.go：`agmemd --version`（构建信息见 internal/version 包）。
//
// version / buildDate 由构建时 -X 注入（默认 dev / 空）——与 cmd/agmem、
// cmd/agmemserv 的 `-X main.version` 惯例一致。
//
// 与 agmem 的差别：agmemd 是守护进程、没有子命令框架，且启动即要求
// AGMEM_URL 必填。因此版本查询做成**最早的短路**（见 main 首行）：
// 查版本不该要求配置齐全，也不该建缓存目录、加载模型。
package main

import (
	"fmt"
	"io"
	"os"

	verinfo "sre/agmem/internal/version"
)

var (
	version   = "dev"
	buildDate = ""
)

// printVersionIfRequested 处理 --version / -v / version（可跟 --json）。
// 命中即写出构建信息并返回 true，调用方应立即 return。
//
// 只认首个参数：守护进程的其余参数（未来可能的 -addr 等）不应被版本分支吞掉。
func printVersionIfRequested(args []string, w io.Writer) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "--version", "-v", "version":
	default:
		return false
	}
	vi := verinfo.Read("agmemd", version, buildDate)
	var err error
	if len(args) > 1 && args[1] == "--json" {
		err = vi.WriteJSON(w)
	} else {
		err = vi.WriteText(w)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agmemd:", err)
	}
	return true
}
