package config

// FromEnv 分数阈值（UC-1）表驱动测试：默认 0.5、合法覆盖、非法/越界回落默认。
// 其余必填项仅以满足校验为度（FromEnv 不发起网络/存储连接）。

import (
	"testing"

	"sre/agmem/internal/cleaner"
)

func TestFromEnv_ScoreThreshold(t *testing.T) {
	cases := []struct {
		name string
		env  string
		set  bool
		want float64
	}{
		{name: "未设置用默认0.5", set: false, want: 0.5},
		{name: "合法0.7", env: "0.7", set: true, want: 0.7},
		{name: "边界0保留", env: "0", set: true, want: 0},
		{name: "边界1保留", env: "1", set: true, want: 1},
		{name: "非数字回落0.5", env: "abc", set: true, want: 0.5},
		{name: "负数回落0.5", env: "-0.1", set: true, want: 0.5},
		{name: "越界1.5回落0.5", env: "1.5", set: true, want: 0.5},
		{name: "空串同未设置", env: "", set: true, want: 0.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AGMEM_MYSQL_DSN", "u:p@tcp(127.0.0.1:3306)/agmem")
			t.Setenv("AGMEM_MACHINE_TOKEN", "t")
			t.Setenv("AIGATEWAY_TOKEN", "t")
			t.Setenv("AIGATEWAY_BASE_URL", "https://gateway.example.com/v1")
			if c.set {
				t.Setenv("AGMEM_SCORE_THRESHOLD", c.env)
			}
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.ScoreThreshold != c.want {
				t.Fatalf("ScoreThreshold = %v, want %v", cfg.ScoreThreshold, c.want)
			}
		})
	}
}

// DefaultScoreThreshold 常量本身即 0.5（规格值，服务端与测试共同依赖）。
func TestDefaultScoreThresholdConstant(t *testing.T) {
	if DefaultScoreThreshold != 0.5 {
		t.Fatalf("DefaultScoreThreshold = %v, want 0.5", DefaultScoreThreshold)
	}
}

// 单库化（2026-09-08）：KB 默认 sre（DefaultKB 常量绑定），AGMEM_KB 可覆盖。
func TestFromEnv_KB(t *testing.T) {
	cases := []struct {
		name string
		env  string
		set  bool
		want string
	}{
		{name: "未设置用默认sre", set: false, want: "sre"},
		{name: "显式覆盖", env: "ops", set: true, want: "ops"},
		{name: "空串同未设置", env: "", set: true, want: "sre"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AGMEM_MYSQL_DSN", "u:p@tcp(127.0.0.1:3306)/agmem")
			t.Setenv("AGMEM_MACHINE_TOKEN", "t")
			t.Setenv("AIGATEWAY_TOKEN", "t")
			t.Setenv("AIGATEWAY_BASE_URL", "https://gateway.example.com/v1")
			if c.set {
				t.Setenv("AGMEM_KB", c.env)
			}
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.KB != c.want {
				t.Fatalf("KB = %q, want %q", cfg.KB, c.want)
			}
		})
	}
}

// DefaultKB 常量钉死：服务端 collection 名、CLI wire 兼容回填、daemon 版本门禁
// 三方共同依赖（单库化拍板，改动须整体评估）。
func TestDefaultKBConstant(t *testing.T) {
	if DefaultKB != "sre" {
		t.Fatalf("DefaultKB = %q, want \"sre\"", DefaultKB)
	}
}

// 查询侧指令前缀（2026-09-09 检索质量批）：默认来自常量、字面 off（大小写
// 不敏感）禁用、其余原样保留；空串语义是禁用，故默认值必须来自常量而非零值。
func TestFromEnv_QueryInstruction(t *testing.T) {
	cases := []struct {
		name string
		env  string
		set  bool
		want string
	}{
		{name: "未设置用常量默认", set: false, want: DefaultQueryInstruction},
		{name: "空串同未设置", env: "", set: true, want: DefaultQueryInstruction},
		{name: "off禁用置空", env: "off", set: true, want: ""},
		{name: "OFF大小写不敏感", env: "OFF", set: true, want: ""},
		{name: "Off混合大小写", env: "Off", set: true, want: ""},
		{name: "off前后空白容忍", env: "  off  ", set: true, want: ""},
		{name: "自定义指令原样保留", env: "Instruct: custom\nQuery: ", set: true, want: "Instruct: custom\nQuery: "},
		{name: "非off值不禁用", env: "on", set: true, want: "on"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AGMEM_MYSQL_DSN", "u:p@tcp(127.0.0.1:3306)/agmem")
			t.Setenv("AGMEM_MACHINE_TOKEN", "t")
			t.Setenv("AIGATEWAY_TOKEN", "t")
			t.Setenv("AIGATEWAY_BASE_URL", "https://gateway.example.com/v1")
			if c.set {
				t.Setenv("AGMEM_QUERY_INSTRUCT", c.env)
			}
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.QueryInstruction != c.want {
				t.Fatalf("QueryInstruction = %q, want %q", cfg.QueryInstruction, c.want)
			}
		})
	}
}

// DefaultQueryInstruction 常量钉死：Qwen3-Embedding 训练格式（Instruct 行 +
// Query: 前缀，末尾空格）——模型侧约定，改动即改变检索行为，须走设计拍板。
func TestDefaultQueryInstructionConstant(t *testing.T) {
	want := "Instruct: Given a search query, retrieve relevant passages that answer the query\nQuery: "
	if DefaultQueryInstruction != want {
		t.Fatalf("DefaultQueryInstruction = %q, want %q", DefaultQueryInstruction, want)
	}
}

// 混合检索开关（2026-09-08）：默认开启，仅字面 off 关闭（回退阀语义）。
func TestFromEnv_HybridEnabled(t *testing.T) {
	cases := []struct {
		name string
		env  string
		set  bool
		want bool
	}{
		{name: "未设置默认开启", set: false, want: true},
		{name: "off显式关闭", env: "off", set: true, want: false},
		{name: "on保持开启", env: "on", set: true, want: true},
		{name: "任意非off值保持开启", env: "0", set: true, want: true},
		{name: "OFF大小写敏感保持开启", env: "OFF", set: true, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AGMEM_MYSQL_DSN", "u:p@tcp(127.0.0.1:3306)/agmem")
			t.Setenv("AGMEM_MACHINE_TOKEN", "t")
			t.Setenv("AIGATEWAY_TOKEN", "t")
			t.Setenv("AIGATEWAY_BASE_URL", "https://gateway.example.com/v1")
			if c.set {
				t.Setenv("AGMEM_HYBRID", c.env)
			}
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.HybridEnabled != c.want {
				t.Fatalf("HybridEnabled = %v, want %v", cfg.HybridEnabled, c.want)
			}
		})
	}
}

func TestFromEnv_PrefixEnabled(t *testing.T) {
	// AGMEM_PREFIX 走 getenvBool：off/false/0/no 关闭，其余（含未设置）默认开启。
	cases := []struct {
		name string
		env  string
		set  bool
		want bool
	}{
		{name: "未设置默认开启", set: false, want: true},
		{name: "off关闭", env: "off", set: true, want: false},
		{name: "false关闭", env: "false", set: true, want: false},
		{name: "0关闭", env: "0", set: true, want: false},
		{name: "on开启", env: "on", set: true, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AGMEM_MYSQL_DSN", "u:p@tcp(127.0.0.1:3306)/agmem")
			t.Setenv("AGMEM_MACHINE_TOKEN", "t")
			t.Setenv("AIGATEWAY_TOKEN", "t")
			t.Setenv("AIGATEWAY_BASE_URL", "https://gateway.example.com/v1")
			if c.set {
				t.Setenv("AGMEM_PREFIX", c.env)
			}
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.PrefixEnabled != c.want {
				t.Fatalf("PrefixEnabled = %v, want %v", cfg.PrefixEnabled, c.want)
			}
		})
	}
}

// 不变前缀预算（2026-09-10）：默认 200000（= cleaner.DefaultPrefixBudgetTokens，
// 两处必须一致），AGMEM_PREFIX_BUDGET 合法正数覆盖；非法/非正回落默认（不阻断启动）。
func TestFromEnv_PrefixBudget(t *testing.T) {
	if DefaultPrefixBudget != cleaner.DefaultPrefixBudgetTokens {
		t.Fatalf("DefaultPrefixBudget=%d 与 cleaner.DefaultPrefixBudgetTokens=%d 不一致",
			DefaultPrefixBudget, cleaner.DefaultPrefixBudgetTokens)
	}
	if DefaultPrefixBudget != 200000 {
		t.Fatalf("DefaultPrefixBudget=%d, want 200000", DefaultPrefixBudget)
	}
	cases := []struct {
		name string
		env  string
		set  bool
		want int
	}{
		{name: "未设置用默认200000", set: false, want: 200000},
		{name: "合法覆盖", env: "123456", set: true, want: 123456},
		{name: "非法回落默认", env: "abc", set: true, want: 200000},
		{name: "零回落默认", env: "0", set: true, want: 200000},
		{name: "负数回落默认", env: "-5", set: true, want: 200000},
		{name: "空串同未设置", env: "", set: true, want: 200000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AGMEM_MYSQL_DSN", "u:p@tcp(127.0.0.1:3306)/agmem")
			t.Setenv("AGMEM_MACHINE_TOKEN", "t")
			t.Setenv("AIGATEWAY_TOKEN", "t")
			t.Setenv("AIGATEWAY_BASE_URL", "https://gateway.example.com/v1")
			if c.set {
				t.Setenv("AGMEM_PREFIX_BUDGET", c.env)
			}
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.PrefixBudget != c.want {
				t.Fatalf("PrefixBudget = %d, want %d", cfg.PrefixBudget, c.want)
			}
		})
	}
}

// 飞书 wiki 同步配置（2026-09-10）：默认关闭；enabled=true 缺凭证/非法 hours/tz
// 一律 fail-fast（需求：缺凭证明确报错而非静默不跑）；hours/tz/space 可覆盖。
func TestFromEnv_WikiSync(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("AGMEM_MYSQL_DSN", "u:p@tcp(127.0.0.1:3306)/agmem")
		t.Setenv("AGMEM_MACHINE_TOKEN", "t")
		t.Setenv("AIGATEWAY_TOKEN", "t")
		t.Setenv("AIGATEWAY_BASE_URL", "https://gateway.example.com/v1")
	}

	t.Run("默认关闭且默认值正确", func(t *testing.T) {
		base(t)
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.WikiSyncEnabled {
			t.Fatal("默认应关闭")
		}
		if cfg.WikiSyncHours != DefaultWikiSyncHours {
			t.Fatalf("hours=%q want %q", cfg.WikiSyncHours, DefaultWikiSyncHours)
		}
		if cfg.WikiSyncTZ != DefaultWikiSyncTZ {
			t.Fatalf("tz=%q want %q", cfg.WikiSyncTZ, DefaultWikiSyncTZ)
		}
		if cfg.WikiSyncSpaceID != DefaultWikiSyncSpaceID {
			t.Fatalf("space=%q want %q", cfg.WikiSyncSpaceID, DefaultWikiSyncSpaceID)
		}
	})

	t.Run("enabled缺凭证报错", func(t *testing.T) {
		base(t)
		t.Setenv("AGMEM_WIKISYNC_ENABLED", "true")
		t.Setenv("AGMEM_WIKISYNC_SPACE_ID", "7600000000000000000")
		if _, err := FromEnv(); err == nil {
			t.Fatal("enabled=true 缺 app_id/secret 应报错")
		}
	})

	t.Run("enabled仅缺secret报错", func(t *testing.T) {
		base(t)
		t.Setenv("AGMEM_WIKISYNC_ENABLED", "true")
		t.Setenv("AGMEM_WIKISYNC_SPACE_ID", "7600000000000000000")
		t.Setenv("AGMEM_WIKISYNC_APP_ID", "cli_x")
		if _, err := FromEnv(); err == nil {
			t.Fatal("缺 app_secret 应报错")
		}
	})

	t.Run("enabled全配通过", func(t *testing.T) {
		base(t)
		t.Setenv("AGMEM_WIKISYNC_ENABLED", "true")
		t.Setenv("AGMEM_WIKISYNC_SPACE_ID", "7600000000000000000")
		t.Setenv("AGMEM_WIKISYNC_APP_ID", "cli_x")
		t.Setenv("AGMEM_WIKISYNC_APP_SECRET", "s3cr3t")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if !cfg.WikiSyncEnabled || cfg.WikiSyncAppID != "cli_x" || cfg.WikiSyncAppSecret != "s3cr3t" {
			t.Fatalf("配置未生效: %+v", cfg)
		}
	})

	t.Run("非法hours报错", func(t *testing.T) {
		base(t)
		t.Setenv("AGMEM_WIKISYNC_ENABLED", "true")
		t.Setenv("AGMEM_WIKISYNC_SPACE_ID", "7600000000000000000")
		t.Setenv("AGMEM_WIKISYNC_APP_ID", "cli_x")
		t.Setenv("AGMEM_WIKISYNC_APP_SECRET", "s")
		t.Setenv("AGMEM_WIKISYNC_HOURS", "0-9,25")
		if _, err := FromEnv(); err == nil {
			t.Fatal("越界 hours 应报错")
		}
	})

	t.Run("非法tz报错", func(t *testing.T) {
		base(t)
		t.Setenv("AGMEM_WIKISYNC_ENABLED", "true")
		t.Setenv("AGMEM_WIKISYNC_SPACE_ID", "7600000000000000000")
		t.Setenv("AGMEM_WIKISYNC_APP_ID", "cli_x")
		t.Setenv("AGMEM_WIKISYNC_APP_SECRET", "s")
		t.Setenv("AGMEM_WIKISYNC_TZ", "Not/AZone")
		if _, err := FromEnv(); err == nil {
			t.Fatal("非法 tz 应报错")
		}
	})

	t.Run("hours自定义覆盖", func(t *testing.T) {
		base(t)
		t.Setenv("AGMEM_WIKISYNC_ENABLED", "true")
		t.Setenv("AGMEM_WIKISYNC_SPACE_ID", "7600000000000000000")
		t.Setenv("AGMEM_WIKISYNC_APP_ID", "cli_x")
		t.Setenv("AGMEM_WIKISYNC_APP_SECRET", "s")
		t.Setenv("AGMEM_WIKISYNC_HOURS", "1,3")
		t.Setenv("AGMEM_WIKISYNC_TZ", "UTC")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.WikiSyncHours != "1,3" || cfg.WikiSyncTZ != "UTC" {
			t.Fatalf("覆盖失败: %+v", cfg)
		}
	})
}
