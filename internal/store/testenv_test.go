package store

import "os"

func envOrSkip(t interface{ Skip(args ...any) }, key string) string {
	v := os.Getenv(key)
	if v == "" {
		t.Skip("环境变量 " + key + " 未设置，跳过（本测试需 MySQL）")
	}
	return v
}
