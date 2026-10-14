package cleaner

import (
	"reflect"
	"strings"
	"testing"
)

// TestVerifyAnchors 表驱动：正例（全保留）/反例（缺失）/无锚点/加密原文跳过。
func TestVerifyAnchors(t *testing.T) {
	encryptedPEM := "-----BEGIN ENCRYPTED PRIVATE KEY-----\nMIIBVAIBADANBgkqhkiG9w0BAQEFAASCAT4wggE6AgEAAkEA\n-----END ENCRYPTED PRIVATE KEY-----"
	encryptedB64 := strings.Repeat("QWxr", 100) // 300 字符 base64 字母表 token

	cases := []struct {
		name     string
		original string
		entries  []string
		want     []string
	}{
		{
			name:     "全保留正例",
			original: "服务监听 192.0.2.162:1194/UDP，路径 /etc/agmem/agmem.env，版本 v0.1.7",
			entries:  []string{"服务监听 192.0.2.162:1194/UDP，配置见 /etc/agmem/agmem.env，版本 v0.1.7"},
			want:     nil,
		},
		{
			name:     "IP 首段被吞（G02 反例形态）",
			original: "服务监听 192.0.2.162:1194/UDP",
			entries:  []string{"服务监听 92.0.2.162:1194/UDP 的改写"},
			want:     []string{"192.0.2.162"},
		},
		{
			name:     "端口丢失",
			original: "服务端口 1194，管理端口 8080",
			entries:  []string{"服务端口 1194 需放行"},
			want:     []string{"8080"},
		},
		{
			name:     "路径丢失",
			original: "配置在 /etc/agmem/agmem.env，脚本在 ops/runbook.md",
			entries:  []string{"配置在 /etc/agmem/agmem.env"},
			want:     []string{"ops/runbook.md"},
		},
		{
			name:     "版本号丢失",
			original: "镜像 pub/agmem:v0.1.7 与 go1.24",
			entries:  []string{"镜像 pub/agmem 已发布"},
			want:     []string{"v0.1.7", "go1.24"},
		},
		{
			name:     "域名与端口丢失",
			original: "出口走 proxy.example.com:3128",
			entries:  []string{"出口走统一代理"},
			want:     []string{"3128", "proxy.example.com"},
		},
		{
			name:     "sha256 丢失",
			original: "镜像钉 @sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			entries:  []string{"镜像已钉 digest"},
			want:     []string{"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"},
		},
		{
			name:     "多锚点缺失保持首次出现顺序",
			original: "1.1.1.1 端口 9000 路径 /etc/hosts 版本 v2.0",
			entries:  []string{"全部改写"},
			want:     []string{"1.1.1.1", "9000", "/etc/hosts", "v2.0"},
		},
		{
			name:     "无锚点",
			original: "这是一段纯中文说明，没有任何数值或标识",
			entries:  []string{"完全改写的说明"},
			want:     nil,
		},
		{
			name:     "缩写 e.g. 不误判为域名锚点",
			original: "例如 e.g. 常见写法，版本 v1.0",
			entries:  []string{"例如常见写法，版本 v1.0"},
			want:     nil,
		},
		{
			name:     "加密 PEM 跳过",
			original: encryptedPEM,
			entries:  []string{"无关内容"},
			want:     nil,
		},
		{
			name:     "加密 base64 blob 跳过",
			original: encryptedB64,
			entries:  []string{"无关内容"},
			want:     nil,
		},
		{
			name:     "空原文",
			original: "   ",
			entries:  []string{"x"},
			want:     nil,
		},
		{
			name:     "条目为空则全部缺失",
			original: "版本 v1.2.3",
			entries:  nil,
			want:     []string{"v1.2.3"},
		},
		{
			// 2026-09-10 线上缺陷修复：字段名不是事实锚点，模型写成中文说明
			// 不应被判缺失（item 1144）。
			name:     "字段名/标识符不误判为锚点",
			original: "记录 archive_name/job_name/status，关联 commit.id",
			entries:  []string{"记录任务归档名、作业名与状态，并关联提交标识"},
			want:     nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := VerifyAnchors(c.original, c.entries)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("VerifyAnchors(%q, %#v) = %#v, want %#v", c.original, c.entries, got, c.want)
			}
		})
	}
}

// TestExtractAnchors_IdentifierNotAnchor：字段名/标识符不得被当作事实锚点
// （2026-09-10 线上缺陷修复，item 1144 的 archive_name/job_name/status、commit.id），
// 同时既有正例（IP/端口/路径/版本/域名/host:port/hex）不得回归。
func TestExtractAnchors_IdentifierNotAnchor(t *testing.T) {
	// 负例：标识符候选（含下划线字段路径、点分标识符）→ 不得进入锚点集合。
	negOriginal := "字段 archive_name/job_name/status、commit.id、job_name、status 与 archive_name 均为标识符"
	if got := extractAnchors(negOriginal); len(got) != 0 {
		t.Fatalf("标识符不应被抽为锚点，得到 %#v", got)
	}

	// 正例：混入标识符干扰时，事实值锚点必须全部保留。
	// 注：host:port 由抽取器拆成「IP」+「端口」两个锚点（reIPPort 只取端口，
	// IP 由 reIPv4 覆盖），故断言 192.0.2.106 与 5424 两个分量而非复合串。
	posOriginal := "IP 192.0.2.162 端口 5424 配置 /etc/openvpn/server.conf 版本 v0.1.7 " +
		"域名 proxy.example.com 端点 192.0.2.106:5424 digest " +
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789 " +
		"另含字段 archive_name/job_name/status、commit.id 不是锚点"
	wantPos := []string{
		"192.0.2.162",
		"5424",
		"192.0.2.106",
		"/etc/openvpn/server.conf",
		"v0.1.7",
		"proxy.example.com",
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
	}
	got := extractAnchors(posOriginal)
	for _, p := range wantPos {
		if !containsStr(got, p) {
			t.Errorf("正例锚点 %q 丢失，得到 %#v", p, got)
		}
	}
	for _, n := range []string{"archive_name/job_name/status", "commit.id", "archive_name", "job_name", "status"} {
		if containsStr(got, n) {
			t.Errorf("标识符 %q 不应被抽为锚点，得到 %#v", n, got)
		}
	}
}

// containsStr 判定切片是否含目标字符串（测试用）。
func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestExtractAnchors_DedupAndValidation：去重、host.tld/path 不误判为路径。
func TestExtractAnchors_DedupAndValidation(t *testing.T) {
	got := extractAnchors("1.1.1.1 与 1.1.1.1 重复；ai-gateway.example.com/v1 不是路径")
	want := []string{"1.1.1.1", "ai-gateway.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractAnchors = %#v, want %#v", got, want)
	}
}

// TestLooksEncrypted：PEM 与长 base64 token 判定；普通文本不误判。
func TestLooksEncrypted(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"PEM", "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----", true},
		{"长 base64 token", strings.Repeat("Ab3+/=", 50), true},
		{"短 base64 不判", strings.Repeat("Ab3", 10), false},
		{"普通中文", "部署脚本位于 /root/deploy", false},
		{"普通英文句子", "the quick brown fox jumps over the lazy dog repeatedly and quickly", false},
	}
	for _, c := range cases {
		if got := looksEncrypted(c.in); got != c.want {
			t.Errorf("%s: looksEncrypted = %v, want %v", c.name, got, c.want)
		}
	}
}
