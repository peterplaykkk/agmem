package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 命中版本查询：返回 true（调用方立即 return，不做 AGMEM_URL 校验/建目录）。
func TestPrintVersionIfRequested_Hit(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"-v"}, {"version"}, {"--version", "--json"}} {
		var buf bytes.Buffer
		if !printVersionIfRequested(args, &buf) {
			t.Errorf("printVersionIfRequested(%v) = false, want true", args)
			continue
		}
		out := buf.String()
		if args[len(args)-1] == "--json" {
			var m map[string]any
			if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
				t.Errorf("args=%v --json 非合法 JSON: %v", args, err)
			} else if m["name"] != "agmemd" {
				t.Errorf("name = %v, want agmemd", m["name"])
			}
			continue
		}
		if !strings.HasPrefix(out, "agmemd ") {
			t.Errorf("args=%v 输出应以 %q 开头，got:\n%s", args, "agmemd ", out)
		}
		if !strings.Contains(out, "commit") || !strings.Contains(out, "flags") {
			t.Errorf("args=%v 文本输出缺字段，got:\n%s", args, out)
		}
	}
}

// 未命中：守护进程正常启动路径不受影响（含无参、以及无法识别的参数）。
func TestPrintVersionIfRequested_Miss(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"--addr", "127.0.0.1:8377"}, {"-h"}, {"--versionx"}} {
		var buf bytes.Buffer
		if printVersionIfRequested(args, &buf) {
			t.Errorf("printVersionIfRequested(%v) = true, want false", args)
		}
		if buf.Len() != 0 {
			t.Errorf("args=%v 未命中却写出内容: %q", args, buf.String())
		}
	}
}

// 只认首个参数：版本分支不得吞掉后续参数（未来 -addr 等不应被误判）。
func TestPrintVersionIfRequested_OnlyFirstArg(t *testing.T) {
	var buf bytes.Buffer
	if printVersionIfRequested([]string{"--addr", "--version"}, &buf) {
		t.Fatal("非首位 --version 不应触发版本输出")
	}
}
