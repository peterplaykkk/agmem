package config

import (
	"reflect"
	"testing"
)

// AGMEM_CLIENT_READONLY_TOKEN 解析：逗号分隔、逐项 TrimSpace、丢弃空项；未设置 → nil。
func TestFromEnv_ClientReadonlyTokens(t *testing.T) {
	setRequired := func(t *testing.T) {
		t.Setenv("AGMEM_MYSQL_DSN", "u:p@tcp(h:3306)/db?parseTime=true")
		t.Setenv("AIGATEWAY_TOKEN", "gw")
		t.Setenv("AIGATEWAY_BASE_URL", "https://gateway.example.com/v1")
		t.Setenv("AGMEM_MACHINE_TOKEN", "machine")
	}
	cases := []struct {
		name string
		val  string
		want []string
	}{
		{"未设置", "", nil},
		{"单令牌", "ro-1", []string{"ro-1"}},
		{"多令牌空格容忍", " ro-1 , ro-2 ", []string{"ro-1", "ro-2"}},
		{"含空项丢弃", "ro-1,,ro-2,", []string{"ro-1", "ro-2"}},
		{"纯空白", "   ", []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setRequired(t)
			t.Setenv("AGMEM_CLIENT_READONLY_TOKEN", c.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if c.want == nil {
				if cfg.ClientReadonlyTokens != nil {
					t.Fatalf("want nil, got %#v", cfg.ClientReadonlyTokens)
				}
				return
			}
			if len(c.want) == 0 {
				if len(cfg.ClientReadonlyTokens) != 0 {
					t.Fatalf("want empty, got %#v", cfg.ClientReadonlyTokens)
				}
				return
			}
			if !reflect.DeepEqual(cfg.ClientReadonlyTokens, c.want) {
				t.Fatalf("want %#v, got %#v", c.want, cfg.ClientReadonlyTokens)
			}
		})
	}
}
