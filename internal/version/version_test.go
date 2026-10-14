package version

import (
	"bytes"
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
)

// TestCollect 表驱动：完整构建信息 / 缺项 / 空切片（go run 或 -buildvcs=false）。
func TestCollect(t *testing.T) {
	full := []debug.BuildSetting{
		{Key: "-buildmode", Value: "exe"},
		{Key: "-compiler", Value: "gc"},
		{Key: "-trimpath", Value: "true"},
		{Key: "CGO_ENABLED", Value: "1"},
		{Key: "DefaultGODEBUG", Value: "containermaxprocs=0,tlsmlkem=0"},
		{Key: "GOARCH", Value: "arm64"},
		{Key: "GOOS", Value: "darwin"},
		{Key: "vcs", Value: "git"},
		{Key: "vcs.modified", Value: "false"},
		{Key: "vcs.revision", Value: "df732bd39ac6e6d3bb072d24cc76ed4b49df2879"},
		{Key: "vcs.time", Value: "2026-09-08T12:40:45Z"},
	}
	got := Collect("agmemd", full, "go1.27.0", "df732bd3", "2026-09-10T16:00:00Z")

	if got.Name != "agmemd" || got.Version != "df732bd3" {
		t.Errorf("Name/Version = %q/%q", got.Name, got.Version)
	}
	if got.Commit != "df732bd39ac6e6d3bb072d24cc76ed4b49df2879" {
		t.Errorf("Commit = %q", got.Commit)
	}
	if got.CommitTime != "2026-09-08T12:40:45Z" || got.Built != "2026-09-10T16:00:00Z" {
		t.Errorf("CommitTime/Built = %q/%q", got.CommitTime, got.Built)
	}
	if got.Go != "go1.27.0" || got.Target != "darwin/arm64" || got.CGO != "1" {
		t.Errorf("Go/Target/CGO = %q/%q/%q", got.Go, got.Target, got.CGO)
	}
	// commit 必须是完整 40 位 hash，不能像 -X 注入那样退化成短 sha。
	if len(got.Commit) != 40 {
		t.Errorf("commit 非完整 hash: %q", got.Commit)
	}
	// flags 含编译选项且按名排序；已单列项与运行时默认值都不得混入。
	if strings.Join(got.Flags, " ") != "-buildmode=exe -compiler=gc -trimpath=true" {
		t.Errorf("Flags = %v", got.Flags)
	}
	for _, f := range got.Flags {
		for _, k := range []string{"CGO_ENABLED=", "GOOS=", "GOARCH=", "vcs.", "DefaultGODEBUG="} {
			if strings.HasPrefix(f, k) {
				t.Errorf("flags 混入了非构建参数 %q", f)
			}
		}
	}
}

func TestCollect_Unknown(t *testing.T) {
	cases := []struct {
		name     string
		settings []debug.BuildSetting
		ver      string
		built    string
		want     map[string]string
	}{
		{"无 settings 且未注入", nil, "", "", map[string]string{
			"Version": Unknown, "Commit": Unknown, "CommitTime": Unknown,
			"Built": Unknown, "Target": Unknown, "CGO": Unknown, "Modified": Unknown,
		}},
		{"仅注入 version", nil, "abc1234", "", map[string]string{
			"Version": "abc1234", "Built": Unknown,
		}},
		{"只有 GOOS 无 GOARCH", []debug.BuildSetting{{Key: "GOOS", Value: "linux"}}, "", "", map[string]string{
			"Target": Unknown,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Collect("agmem", c.settings, "", c.ver, c.built)
			fields := map[string]string{
				"Version": got.Version, "Commit": got.Commit, "CommitTime": got.CommitTime,
				"Built": got.Built, "Target": got.Target, "CGO": got.CGO, "Modified": got.Modified,
			}
			for k, want := range c.want {
				if fields[k] != want {
					t.Errorf("%s = %q, want %q", k, fields[k], want)
				}
			}
		})
	}
}

// TestWriteText_Labels：文本标签与值一一对应；缺 buildDate 时给提示而非静默。
func TestWriteText_Labels(t *testing.T) {
	var buf bytes.Buffer
	info := Collect("agmemd", nil, "go1.27.0", "abc1234", "")
	if err := info.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "agmemd abc1234\n") {
		t.Fatalf("首行应为 %q，got:\n%s", "agmemd abc1234", out)
	}
	for _, want := range []string{
		"  commit       " + Unknown,
		"  commit-time  " + Unknown,
		"  built        " + Unknown,
		"  go           go1.27.0",
		"  target       " + Unknown,
		"  cgo          " + Unknown,
		"  modified     " + Unknown,
		"  flags        ",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("缺少行 %q，got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "未注入 -X main.buildDate") {
		t.Errorf("缺 buildDate 时应给提示，got:\n%s", out)
	}

	buf.Reset()
	withBuilt := Collect("agmemd", nil, "go1.27.0", "abc1234", "2026-09-10T16:00:00Z")
	if err := withBuilt.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "提示:") {
		t.Errorf("已注入 buildDate 时不应有提示，got:\n%s", buf.String())
	}
}

// TestWriteJSON_Fields：JSON 键与文本标签一致，且 name 随二进制变化。
func TestWriteJSON_Fields(t *testing.T) {
	var buf bytes.Buffer
	if err := Collect("agmem", nil, "go1.27.0", "abc1234", "2026-09-10T16:00:00Z").WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("非合法 JSON: %v\n%s", err, buf.String())
	}
	for _, k := range []string{"name", "version", "commit", "commit_time", "built", "go", "target", "cgo", "modified", "flags"} {
		if _, ok := m[k]; !ok {
			t.Errorf("JSON 缺字段 %q", k)
		}
	}
	if m["name"] != "agmem" {
		t.Errorf("name = %v", m["name"])
	}
}
