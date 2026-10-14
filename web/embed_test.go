package webui

// embed 自检：dist 产物必须存在且含 SPA 入口（防止空 dist 被打进单二进制）。

import (
	"io/fs"
	"testing"
)

func TestEmbedDistHasSPA(t *testing.T) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		t.Fatalf("fs.Sub(dist): %v", err)
	}
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("dist/index.html 读取失败（dist 未构建或未入库）: %v", err)
	}
	if len(b) < 100 {
		t.Fatalf("index.html 过小（%d bytes），疑似空产物", len(b))
	}
	if !containsStr(string(b), "<div") && !containsStr(string(b), "<!DOCTYPE") && !containsStr(string(b), "<html") {
		t.Fatalf("index.html 不像 SPA 入口: %.80s", b)
	}
	// assets 目录应存在（vite 产物特征）
	entries, err := fs.ReadDir(sub, "assets")
	if err != nil || len(entries) == 0 {
		t.Fatalf("dist/assets 缺失或为空: err=%v n=%d", err, len(entries))
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
