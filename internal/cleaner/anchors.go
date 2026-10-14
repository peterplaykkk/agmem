// anchors.go：事实锚点保真校验（v5 审计 G02，2026-09-09）。
//
// 动机：v4 写入红线要求「事实数值/端口/路径原样保留」，但全库扫描发现 56–98 个
// 已清洗条目丢失原文锚点（如 192.0.2.162:1194/UDP 被改写为 92.0.2.162:1194/UDP，
// IP 首段被吞）。本文件提供确定性后处理校验：从原文抽取数值/标识锚点，检查其是否
// 出现在清洗条目文本并集；缺失则调用方用严格 prompt 重清一次，仍缺失则标记 failed
// 并记录缺失清单（可重试）。
package cleaner

import (
	"regexp"
	"strings"
)

var (
	reIPv4       = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	reIPPort     = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}:(\d{2,5})\b`)
	reDomainPort = regexp.MustCompile(`\b[a-z0-9][a-z0-9\-]*(?:\.[a-z0-9\-]+)+:(\d{2,5})\b`)
	rePortLabel  = regexp.MustCompile(`端口[:：]?\s*(\d{2,5})`)
	rePortSuffix = regexp.MustCompile(`(\d{2,5})\s*端口`)
	reAbsPath    = regexp.MustCompile(`~?(?:/[A-Za-z0-9._\-]+){2,}/?`)
	reRelPath    = regexp.MustCompile(`[A-Za-z0-9._\-]+(?:/[A-Za-z0-9._\-]+)+/?`)
	reVersion    = regexp.MustCompile(`\b[A-Za-z]{0,4}v?\d+\.\d+(?:\.\d+)*(?:[-+][A-Za-z0-9.]+)?\b`)
	reDomain     = regexp.MustCompile(`\b[a-z0-9][a-z0-9\-]*(?:\.[a-z0-9][a-z0-9\-]*)+\b`)
	reDigest     = regexp.MustCompile(`\b[0-9a-fA-F]{40,64}\b`)
)

// VerifyAnchors 校验原文锚点是否全部出现在清洗条目文本并集中（G02）。
// 返回缺失锚点清单（按首次出现顺序、去重）；原文无可校验锚点、或原文为加密
// 二进制 blob（无法可靠抽取锚点）时返回 nil。
//
// 纯函数：不访问网络/存储，供 worker 与迁移工具复用。
func VerifyAnchors(original string, entries []string) []string {
	if strings.TrimSpace(original) == "" || looksEncrypted(original) {
		return nil
	}
	anchors := extractAnchors(original)
	if len(anchors) == 0 {
		return nil
	}
	joined := strings.Join(entries, "\n")
	var missing []string
	for _, a := range anchors {
		if !strings.Contains(joined, a) {
			missing = append(missing, a)
		}
	}
	return missing
}

// extractAnchors 从原文抽取锚点（IPv4、IP:port、端口、路径、版本号、域名、
// sha/digest），去重并保持首次出现顺序。过短/无信息量的匹配被丢弃。
// 字段名/标识符候选（isIdentifierToken）在此一并排除——只把「事实值」当锚点。
func extractAnchors(s string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(a string) {
		a = strings.TrimSpace(a)
		// 字段名/标识符候选先于去重丢弃（见 isIdentifierToken）。
		if a == "" || isIdentifierToken(a) || seen[a] {
			return
		}
		seen[a] = true
		out = append(out, a)
	}

	for _, m := range reIPv4.FindAllString(s, -1) {
		if validIPv4(m) {
			add(m)
		}
	}
	for _, m := range reIPPort.FindAllStringSubmatch(s, -1) {
		add(m[1]) // 端口号（IP 本身已由 reIPv4 覆盖）
	}
	for _, m := range reDomainPort.FindAllStringSubmatch(s, -1) {
		add(m[1])
	}
	for _, m := range rePortLabel.FindAllStringSubmatch(s, -1) {
		add(m[1])
	}
	for _, m := range rePortSuffix.FindAllStringSubmatch(s, -1) {
		add(m[1])
	}
	for _, m := range reAbsPath.FindAllString(s, -1) {
		if runeLen(m) >= 3 {
			add(m)
		}
	}
	for _, m := range reRelPath.FindAllString(s, -1) {
		if isRelativePathAnchor(m) {
			add(m)
		}
	}
	for _, m := range reVersion.FindAllString(s, -1) {
		if strings.ContainsAny(m, "0123456789") {
			add(m)
		}
	}
	for _, m := range reDomain.FindAllString(s, -1) {
		if isDomainAnchor(m) {
			add(m)
		}
	}
	for _, m := range reDigest.FindAllString(s, -1) {
		add(m)
	}
	return dropSubAnchors(out)
}

// dropSubAnchors 丢弃被更长锚点包含的短锚点（保留长锚点即已保证短锚点出现，
// 避免相对路径与绝对路径、子串版本号重复计数）。
func dropSubAnchors(anchors []string) []string {
	var out []string
	for i, a := range anchors {
		sub := false
		for j, b := range anchors {
			if i != j && len(b) > len(a) && strings.Contains(b, a) {
				sub = true
				break
			}
		}
		if !sub {
			out = append(out, a)
		}
	}
	return out
}

// validIPv4 校验四段均 ≤255（正则只保证 1-3 位数字）。
func validIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return false
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}

// isRelativePathAnchor 过滤相对路径候选：至少 2 段、首段不含点（避免把
// host.tld/path 误当路径）、整体含非数字字符。
func isRelativePathAnchor(m string) bool {
	m = strings.TrimSuffix(m, "/")
	parts := strings.Split(m, "/")
	if len(parts) < 2 {
		return false
	}
	if strings.Contains(parts[0], ".") {
		return false
	}
	return strings.IndexFunc(m, func(r rune) bool { return r < '0' || r > '9' }) >= 0
}

// isDomainAnchor 过滤域名候选：至少一个点、顶级段 ≥2 字符（排除 e.g/i.e 等
// 缩写）、且必须含字母（排除纯数字点分串）。
func isDomainAnchor(m string) bool {
	parts := strings.Split(m, ".")
	if len(parts) < 2 || len(parts[len(parts)-1]) < 2 {
		return false
	}
	for _, r := range m {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

// isIdentifierToken 判定候选是否为「字段名 / 标识符」而非事实值锚点
// （2026-09-10 线上缺陷修复）。
//
// 动机（线上实证）：item 1144 的原文含 `archive_name/job_name/status`、
// `commit.id` 等字段名，旧实现把它们当作「事实数值锚点」要求条目逐字保留；
// 模型按语义写成中文说明即被判缺失 → 整条 failed，并白跑一次严格重清。
//
// 规则（确定性、可解释）：
//  1. 路径形态（含 '/'）：去掉首尾空段后 ≥2 段，每段均 ∈ [a-z0-9_]，且至少一段
//     含 '_' → 字段路径（如 archive_name/job_name/status、/job_name/status）。
//     要求「至少一段含 '_'」是刻意收窄：仅全小写字母的普通路径（/etc/hosts、
//     sre/main）仍按路径锚点保留，避免误杀真实文件/分支路径。
//  2. 含 '_' 且整体 ∈ [a-z0-9_] → snake_case 标识符（archive_name、job_name）；
//  3. 恰好两段点分且各段均为纯 ASCII 字母 → 点分标识符（commit.id）。
//     只认两段：≥3 段的纯字母点分 token（如 qianfan.baidubce.com）仍是域名锚点。
//
// 反例（仍视为锚点，不得回归）：192.0.2.162、192.0.2.106:5424（拆成 IP+端口两个
// 锚点）、proxy.example.com、v0.1.7、/etc/openvpn/server.conf、40/64 位 hex、
// 5424（端口语境）。
//
// 已知取舍：两段纯字母域名（如 baidu.com）不再强制逐字保留；含下划线段的普通路径
// （如 docs/foo_bar）也不再强制——宁可漏检，不可误杀。
func isIdentifierToken(s string) bool {
	if s == "" {
		return false
	}
	if strings.ContainsRune(s, '/') {
		var segs []string
		hasUnderscore := false
		for _, seg := range strings.Split(s, "/") {
			if seg == "" {
				continue
			}
			segs = append(segs, seg)
			if strings.ContainsRune(seg, '_') {
				hasUnderscore = true
			}
		}
		if len(segs) < 2 || !hasUnderscore {
			return false
		}
		for _, seg := range segs {
			if !isIdentifierSegment(seg) {
				return false
			}
		}
		return true
	}
	if strings.ContainsRune(s, '_') && isIdentifierSegment(s) {
		return true
	}
	if parts := strings.Split(s, "."); len(parts) == 2 && isASCIILetters(parts[0]) && isASCIILetters(parts[1]) {
		return true
	}
	return false
}

// isIdentifierSegment 判定单段是否为小写标识符段：非空且字符全部 ∈ [a-z0-9_]。
func isIdentifierSegment(seg string) bool {
	if seg == "" {
		return false
	}
	for _, r := range seg {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}

// isASCIILetters 判定字符串非空且全部为 ASCII 字母（点分标识符的段判据）。
func isASCIILetters(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// looksEncrypted 判定原文是否为加密/二进制 blob（无法可靠抽取事实锚点）：
//   - PEM 标记（-----BEGIN ...）；
//   - 单个 ≥200 字符、仅由 base64 字母表（含 +/= -_）构成的 token。
//
// 命中时 VerifyAnchors 跳过校验（审计 G02 已确认 item 657 的加密原文反例不成立）。
func looksEncrypted(s string) bool {
	if strings.Contains(s, "-----BEGIN ") {
		return true
	}
	for _, tok := range strings.Fields(s) {
		if len(tok) >= 200 && isBase64ish(tok) {
			return true
		}
	}
	return false
}

// isBase64ish 判定 token 是否只含 base64 字母表且长度 ≥200。
func isBase64ish(tok string) bool {
	n := 0
	for _, r := range tok {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '+', r == '/', r == '=', r == '-', r == '_':
			n++
		default:
			return false
		}
	}
	return n >= 200
}
