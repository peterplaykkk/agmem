// Package scripts 内嵌 agmem 用户侧脚本资产（bash 补全），随单二进制发布。
//
// 唯一事实源：本目录 agmem-completion.bash（仓库内仅此一份）。
// cmd/agmem 的 `completion bash` 通过 //go:embed 原样输出，用户无需知道文件
// 路径，也保证用户侧补全脚本与 CLI 同版本、不漂移。
//
// 为什么 embed 声明放在 scripts/：//go:embed 只能引用本包目录内的文件，
// 无法跨目录引用 ../scripts/agmem-completion.bash，故此处仅放一个 embed 声明。
package scripts

import _ "embed"

// BashCompletion 是 agmem 的 bash 补全脚本原文（macOS bash 3.2 兼容）。
//
//go:embed agmem-completion.bash
var BashCompletion string
