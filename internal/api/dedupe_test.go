package api

import (
	"strings"
	"testing"

	"sre/agmem/internal/qdrant"
)

func hit(text string, score float64) qdrant.SearchHit {
	return qdrant.SearchHit{Score: score, Payload: qdrant.PointPayload{Text: text}}
}

// 近重复折叠：只折叠「几乎逐字相同」的条目，保留排序最高的一条。
func TestCollapseNearDuplicates(t *testing.T) {
	base := "GitLab 提交代码流程：克隆仓库后从 origin/main 切功能分支并用 worktree 隔离开发，提交信息用规范格式。"
	cases := []struct {
		name        string
		in          []qdrant.SearchHit
		wantKept    int
		wantDropped int
	}{
		{
			name:        "逐字相同折叠为一条",
			in:          []qdrant.SearchHit{hit(base, 0.9), hit(base, 0.8)},
			wantKept:    1,
			wantDropped: 1,
		},
		{
			name: "仅空白与大小写差异也折叠",
			in: []qdrant.SearchHit{hit(base, 0.9),
				hit(strings.ReplaceAll(base, " ", "\n")+"  ", 0.8)}, // 换行+尾空格+大小写不敏感
			wantKept:    1,
			wantDropped: 1,
		},
		{
			name: "内容互补（同主题不同细节）不折叠",
			in: []qdrant.SearchHit{
				hit(base, 0.9),
				hit("GitLab 推送凭证配置：macOS 用 credential.helper osxkeychain；禁止 credential.helper store 明文落盘。", 0.8),
			},
			wantKept:    2,
			wantDropped: 0,
		},
		{
			name:        "无关条目互不影响",
			in:          []qdrant.SearchHit{hit(base, 0.9), hit("Nexus 仓库地址与镜像配置说明。", 0.7)},
			wantKept:    2,
			wantDropped: 0,
		},
		{
			name:        "空正文不参与折叠",
			in:          []qdrant.SearchHit{hit("", 0.9), hit("", 0.8), hit(base, 0.7)},
			wantKept:    3,
			wantDropped: 0,
		},
		{
			name:        "三条同文只留第一条",
			in:          []qdrant.SearchHit{hit(base, 0.9), hit(base, 0.8), hit(base, 0.7)},
			wantKept:    1,
			wantDropped: 2,
		},
		{
			name:        "单条不折叠",
			in:          []qdrant.SearchHit{hit(base, 0.9)},
			wantKept:    1,
			wantDropped: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kept, dropped := collapseNearDuplicates(c.in)
			if len(kept) != c.wantKept || dropped != c.wantDropped {
				t.Fatalf("kept=%d dropped=%d, want kept=%d dropped=%d",
					len(kept), dropped, c.wantKept, c.wantDropped)
			}
		})
	}
}

// 折叠时保留**正文更长**的那条，位置仍是原排序位置（输入按 Score 降序）。
func TestCollapseNearDuplicates_KeepsHighestScored(t *testing.T) {
	short := "统一出口代理走 proxy.example.com:3128，仅白名单域名可用。"
	long := short + "补充：3130 用于可信大文件 bump，3132 仅 buildhost 源可用。"
	kept, dropped := collapseNearDuplicates([]qdrant.SearchHit{
		hit(short, 0.95), hit("无关条目甲。", 0.9), hit(long, 0.85), hit("无关条目乙。", 0.8),
	})
	if len(kept) != 3 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d want 3/1", len(kept), dropped)
	}
	// 位置保持原排序（第 0 位），但正文换成更详尽的那条，Score 随之来自该条。
	if kept[0].Payload.Text != long || kept[0].Score != 0.85 {
		t.Fatalf("折叠后应留详版且位置不变: score=%v text=%s", kept[0].Score, kept[0].Payload.Text)
	}
	if kept[1].Score != 0.9 || kept[2].Score != 0.8 {
		t.Fatalf("未折叠项顺序错误: %v %v", kept[1].Score, kept[2].Score)
	}
}

// 阈值边界：Jaccard 恰在阈值上视为重复；明显低于阈值不折叠。
func TestJaccardBoundary(t *testing.T) {
	a := textTrigrams("abcdefghij")
	if jaccard(a, a) != 1 {
		t.Fatal("自身相似度应为 1")
	}
	if jaccard(a, nil) != 0 || jaccard(nil, a) != 0 {
		t.Fatal("空集相似度应为 0（不误判重复）")
	}
	// 只差 1 个 3-gram：Jaccard = 7/9 ≈ 0.78 ≥ 0.75 → 折叠
	if j := jaccard(textTrigrams("abcdefghij"), textTrigrams("abcdefghix")); j < nearDupJaccard {
		t.Fatalf("近似串应达阈值, got %.3f", j)
	}
	// 差异较大：不折叠
	if j := jaccard(textTrigrams("abcdefghij"), textTrigrams("abcdefgXYZ")); j >= nearDupJaccard {
		t.Fatalf("差异串不应达阈值, got %.3f", j)
	}
}

// 包含度折叠：短条目近乎被长条目覆盖时折叠（长短详略是线上最常见的重复形态）。
func TestCollapseNearDuplicates_Containment(t *testing.T) {
	short := "推送代码到 GitLab 的命令步骤：1. 配置 remote：git remote set-url origin https://<username>@gitlab.example.com/<group>/<project>.git 2. 推送当前分支并建立跟踪：git push -u origin <branch> 3. 已推送分支后续直接 git push。"
	long := short + "\n补充：首次需要创建 Personal access token 并勾选 write_repository；macOS 用 credential.helper osxkeychain 保存凭据，禁止 credential.helper store 明文落盘。"
	kept, dropped := collapseNearDuplicates([]qdrant.SearchHit{hit(long, 0.9), hit(short, 0.8)})
	if len(kept) != 1 || dropped != 1 {
		t.Fatalf("短条目应被长条目包含折叠: kept=%d dropped=%d", len(kept), dropped)
	}
	if kept[0].Score != 0.9 {
		t.Fatalf("应保留排序更高的长条目, got %v", kept[0].Score)
	}
	// 反向（短在前、长在后）：包含度是对称判定的，同样折叠；且**保留长版**
	// ——这正是线上「命令步骤(278字) 排在 完整步骤(580字) 之前」的形态。
	kept2, dropped2 := collapseNearDuplicates([]qdrant.SearchHit{hit(short, 0.9), hit(long, 0.8)})
	if len(kept2) != 1 || dropped2 != 1 {
		t.Fatalf("短在前时也应折叠: kept=%d dropped=%d", len(kept2), dropped2)
	}
	if kept2[0].Payload.Text != long {
		t.Fatalf("应保留更详尽的长版, got %d 字", runeLen(kept2[0].Payload.Text))
	}
}

func TestContainment(t *testing.T) {
	short := textTrigrams("abcdefghij")
	long := textTrigrams("abcdefghijKLMNOP")
	if c := containment(short, long); c != 1 {
		t.Fatalf("全包含应为 1, got %.2f", c)
	}
	if c := containment(long, short); c >= nearDupContainment {
		t.Fatalf("反向不应达阈值, got %.2f", c)
	}
	if containment(nil, long) != 0 {
		t.Fatal("空集应为 0")
	}
}
