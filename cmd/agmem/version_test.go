package main

import (
	"os"
	"strings"
	"testing"
)

// cmdVersion 只做参数解析 + 委派：覆盖 --json 与 --help，行为不依赖构建注入值。
func TestCmdVersion_ExitZero(t *testing.T) {
	for _, args := range [][]string{{}, {"--json"}, {"--help"}, {"-h"}} {
		if err := cmdVersion(args); err != nil {
			t.Errorf("cmdVersion(%v) = %v, want nil", args, err)
		}
	}
}

func TestCmdVersion_UnknownFlag(t *testing.T) {
	if err := cmdVersion([]string{"--nope"}); err == nil {
		t.Fatal("未知 flag 应报错")
	}
}

// 别名可达性门禁：main() 的 dispatch 在测试里不可执行（os.Exit），故直接核对
// 源码的 case 列表——删掉任一别名（--version/-v/version）本测试必红。
// 格式化敏感（重新换行会误报），但比「拿列表跟自己比」的空断言有意义。
func TestVersionAliasesRegisteredInDispatch(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	want := `case "--version", "-v", "version":`
	if !strings.Contains(string(src), want) {
		t.Fatalf("main.go 缺少 dispatch 分支 %s（别名 --version/-v/version 需同时可达）", want)
	}
	if !strings.Contains(string(src), "err = cmdVersion(") {
		t.Fatal("main.go 的版本分支未调用 cmdVersion")
	}
}
