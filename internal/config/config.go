// Package config 集中管理 agmem 的环境变量配置。
//
// 所有配置均来自环境变量，禁止在代码/测试中硬编码凭据。
// 变量清单见 README.md「环境变量」一节。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"sre/agmem/internal/cleaner"
	"sre/agmem/internal/wikisync"
)

// Config 是 agmem 进程级配置。
type Config struct {
	// HTTP
	ListenAddr string // :8080

	// MySQL 元数据存储
	MySQLDSN string // 例: user:pass@tcp(host:3306)/agmem?parseTime=true&charset=utf8mb4

	// Qdrant
	QdrantBaseURL string        // http://localhost:6333
	QdrantAPIKey  string        // 可空（集群内未启用鉴权时留空）
	QdrantDim     int           // 向量维度，当前 1024（qwen3-embedding-0.6b）
	QdrantTimeout time.Duration // 单请求超时

	// 千帆 embedding（fallback）
	QianfanBaseURL string // https://qianfan.baidubce.com
	QianfanAPIKey  string // Bearer key
	EmbeddingModel string // qwen3-embedding-0.6b
	EmbedBatchSize int    // 批量条数
	EmbedWorkers   int    // 并发批数上限

	// aigateway（embedding 主通道 + GLM 清洗，OpenAI 兼容）
	AIGatewayURL   string // https://ai-gateway.example.com/v1
	AIGatewayToken string // AIGATEWAY_TOKEN

	// GLM 清洗（aigateway，OpenAI 兼容）
	GLMBaseURL      string // https://ai-gateway.example.com/v1
	GLMModel        string // glm-5.3-flash
	GLMToken        string // env/secret 注入
	GLMThinkingMode string // 中等思考档位等，透传给 aigateway 的可配参数

	// 鉴权
	MachineToken string // agmem 用的机器 Bearer token（明文比较改 sha256 后比较）
	// ClientReadonlyTokens 只读客户端令牌（AGMEM_CLIENT_READONLY_TOKEN，逗号分隔，允许多个）：
	// 桌面客户端内置，持它只能访问 POST /api/ask（检索）、
	// GET /api/version/{kb} 与 GET /api/version/global（缓存失效锚点）以及 GET /api/me；
	// 其余端点按 404 掩蔽拒绝。为空 = 未配置（机器 token / OIDC 行为完全不变）。
	ClientReadonlyTokens []string
	ExternalURL          string // 对外基地址（OIDC redirect_uri = {ExternalURL}/api/callback），例 https://agmem-test.example.com
	LogtoIssuer          string // Logto OIDC issuer，例 https://logto.example.com/oidc
	LogtoClientID        string
	LogtoClientSecret    string
	SessionSecret        string // cookie 签名密钥

	// Worker
	WorkerInterval time.Duration // 任务轮询间隔
	WorkerEnabled  bool

	// 可观测
	Version string // 编译期注入

	// 检索质量
	ScoreThreshold float64 // 检索分数阈值（UC-1）：score < 阈值的命中不返回（AGMEM_SCORE_THRESHOLD，默认 0.5）
	// DedupCosine 语义去重阈值（AGMEM_DEDUP_COSINE，默认 0.8）：返回前对命中做
	// 向量余弦折叠，≥ 阈值视为同一条知识只留排序最高的。0 = 关闭。依据见
	// internal/api/dedupe_semantic.go 文件头实测数据。
	DedupCosine float64

	// QueryInstruction 查询侧 embedding 指令前缀（2026-09-09 检索质量批）：
	// Qwen3-Embedding 非对称检索约定——查询侧加 Instruct/Query 前缀、文档侧不加。
	// AGMEM_QUERY_INSTRUCT 覆盖；字面 off（大小写不敏感）= 禁用（空串语义即禁用，
	// 故默认值必须来自 DefaultQueryInstruction 常量而非结构体零值）。
	QueryInstruction string

	// 混合检索（dense + bm25 稀疏 RRF 融合，2026-09-08）：默认开启。
	// AGMEM_HYBRID=off 回退纯稠密 legacy Search 并关闭 worker 稀疏写入——
	// 存量 collection 未回填 bm25 稀疏向量期间的安全阀（回滚开关）。
	HybridEnabled bool

	// 清洗不变前缀（S1/S2/S3，2026-09-10）：默认开启。worker 用「上一轮已提取
	// 知识」构成逐字节稳定前缀（L0+L1+L2）配清洗请求，提升 DeepSeek 前缀缓存命中
	// （见 docs/prefix-caching.md）。AGMEM_PREFIX=off/false/0 关闭——回滚阀，关闭后
	// 回退无前缀清洗路径（main 会显式日志「前缀未启用」，不静默降级）。
	PrefixEnabled bool

	// PrefixBudget 不变前缀（L0+L1+L2）的单请求 token 预算上限（AGMEM_PREFIX_BUDGET，
	// 默认 cleaner.DefaultPrefixBudgetTokens = 200000）。L1/L2 的额度由它派生（先扣恒定
	// L0 再按 3:1 切分，见 internal/cleaner/prefix.go prefixQuotas 与 docs/prefix-caching.md
	// §4）。非法/非正值回落默认，不阻断启动（调优项，配置失误不该影响可用性）。
	PrefixBudget int

	// 单库化（2026-09-08 用户拍板）：产品只有一个知识库，用户不可见 kb 概念。
	// KB = 全局唯一 kb 名，ask/push 路径一律用它（请求里的 kb 字段忽略）；
	// 存量 collection `sre` 就是唯一的库，不迁移数据。AGMEM_KB 仅在部署改名时覆盖。
	KB string

	// 飞书 wiki 增量同步（internal/wikisync，agmemserv 内置后台 goroutine；2026-09-10）。
	// enabled 默认 false——未配飞书凭证的部署不该起这个任务；enabled=true 但缺凭证
	// 由 FromEnv 明确报错（不静默不跑）。
	WikiSyncEnabled   bool   // AGMEM_WIKISYNC_ENABLED
	WikiSyncSpaceID   string // AGMEM_WIKISYNC_SPACE_ID
	WikiSyncAppID     string // AGMEM_WIKISYNC_APP_ID（飞书自建应用，secret 注入）
	WikiSyncAppSecret string // AGMEM_WIKISYNC_APP_SECRET（secret 注入）
	WikiSyncHours     string // AGMEM_WIKISYNC_HOURS（"0-8,12-13,18-23"）
	WikiSyncTZ        string // AGMEM_WIKISYNC_TZ（Asia/Shanghai）
}

// wiki 同步默认值。
const (
	// DefaultWikiSyncSpaceID 默认空：启用 wikisync 时必须显式配置目标 wiki 空间 id。
	DefaultWikiSyncSpaceID = ""
	// DefaultWikiSyncHours 默认允许小时集合（Asia/Shanghai）：{0..8,12,13,18..23}。
	// 每 30 分钟一个槽（见 wikisync.SlotInterval）。
	// 刻意排除 9-12 与 14-18——deepseek-flash 的调用高峰期，知识提取避峰执行。
	DefaultWikiSyncHours = "0-8,12-13,18-23"
	// DefaultWikiSyncTZ 默认时区。
	DefaultWikiSyncTZ = "Asia/Shanghai"
)

// FromEnv 从环境变量读取配置，必填缺失时返回 error。
func FromEnv() (Config, error) {
	c := Config{
		ListenAddr:           getenv("AGMEM_LISTEN", ":8080"),
		MySQLDSN:             os.Getenv("AGMEM_MYSQL_DSN"),
		QdrantBaseURL:        getenv("AGMEM_QDRANT_URL", "http://localhost:6333"),
		QdrantAPIKey:         os.Getenv("AGMEM_QDRANT_API_KEY"),
		QdrantDim:            1024,
		QdrantTimeout:        10 * time.Second,
		QianfanBaseURL:       getenv("AGMEM_QIANFAN_URL", "https://qianfan.baidubce.com"),
		QianfanAPIKey:        os.Getenv("QIANFAN_API_KEY"),
		EmbeddingModel:       getenv("AGMEM_EMBEDDING_MODEL", "qwen3-embedding-0.6b"),
		EmbedBatchSize:       16,
		EmbedWorkers:         4,
		AIGatewayURL:         os.Getenv("AIGATEWAY_BASE_URL"),
		AIGatewayToken:       os.Getenv("AIGATEWAY_TOKEN"),
		GLMBaseURL:           getenv("AGMEM_GLM_URL", os.Getenv("AIGATEWAY_BASE_URL")),
		GLMModel:             getenv("AGMEM_GLM_MODEL", "glm-5.3-flash"),
		GLMToken:             getenvFirst("AGMEM_GLM_TOKEN", "AIGATEWAY_TOKEN"),
		GLMThinkingMode:      getenv("AGMEM_GLM_THINKING", "medium"),
		MachineToken:         os.Getenv("AGMEM_MACHINE_TOKEN"),
		ClientReadonlyTokens: splitNonEmpty(os.Getenv("AGMEM_CLIENT_READONLY_TOKEN")),
		ExternalURL:          strings.TrimRight(os.Getenv("AGMEM_EXTERNAL_URL"), "/"),
		LogtoIssuer:          os.Getenv("AGMEM_LOGTO_ISSUER"),
		LogtoClientID:        os.Getenv("AGMEM_LOGTO_CLIENT_ID"),
		LogtoClientSecret:    os.Getenv("AGMEM_LOGTO_CLIENT_SECRET"),
		SessionSecret:        os.Getenv("AGMEM_SESSION_SECRET"),
		WorkerInterval:       2 * time.Second,
		WorkerEnabled:        true,
		Version:              getenv("AGMEM_VERSION", "dev"),
		ScoreThreshold:       DefaultScoreThreshold,
		DedupCosine:          DefaultDedupCosine,
		QueryInstruction:     DefaultQueryInstruction,
		HybridEnabled:        true,
		PrefixEnabled:        getenvBool("AGMEM_PREFIX", true),
		PrefixBudget:         DefaultPrefixBudget,
		KB:                   getenv("AGMEM_KB", DefaultKB),

		WikiSyncEnabled:   getenvBool("AGMEM_WIKISYNC_ENABLED", false),
		WikiSyncSpaceID:   getenv("AGMEM_WIKISYNC_SPACE_ID", DefaultWikiSyncSpaceID),
		WikiSyncAppID:     os.Getenv("AGMEM_WIKISYNC_APP_ID"),
		WikiSyncAppSecret: os.Getenv("AGMEM_WIKISYNC_APP_SECRET"),
		WikiSyncHours:     getenv("AGMEM_WIKISYNC_HOURS", DefaultWikiSyncHours),
		WikiSyncTZ:        getenv("AGMEM_WIKISYNC_TZ", DefaultWikiSyncTZ),
	}
	if v := os.Getenv("AGMEM_QDRANT_DIM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, fmt.Errorf("AGMEM_QDRANT_DIM: %w", err)
		}
		c.QdrantDim = n
	}
	// 分数阈值（UC-1）：未设置用默认；非法（非数字 / 越界 [0,1]）回落默认而非启动失败
	// ——阈值是质量调优项，配置失误不应阻断服务可用性。
	if v := os.Getenv("AGMEM_DEDUP_COSINE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			c.DedupCosine = f
		}
	}
	if v := os.Getenv("AGMEM_SCORE_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			c.ScoreThreshold = DefaultScoreThreshold
		} else {
			c.ScoreThreshold = f
		}
	}
	// 查询侧指令前缀：未设置用常量默认；字面 off（大小写不敏感，前后空白容忍）
	// 置空禁用；其余原样保留（前缀含换行/末尾空格，是语义的一部分，不 Trim）。
	if v := os.Getenv("AGMEM_QUERY_INSTRUCT"); v != "" {
		if strings.EqualFold(strings.TrimSpace(v), "off") {
			c.QueryInstruction = ""
		} else {
			c.QueryInstruction = v
		}
	}
	if v := os.Getenv("AGMEM_WORKER_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGMEM_WORKER_INTERVAL: %w", err)
		}
		c.WorkerInterval = d
	}
	if v := os.Getenv("AGMEM_WORKER_ENABLED"); v == "0" || v == "false" {
		c.WorkerEnabled = false
	}
	// 混合检索开关：仅字面 off 关闭（默认 on；回退阀见 HybridEnabled 注释）
	if os.Getenv("AGMEM_HYBRID") == "off" {
		c.HybridEnabled = false
	}
	// 不变前缀预算：合法正数覆盖，非法/非正回落默认（调优项，不阻断启动，方向安全：
	// 回落默认 200k 仍是硬上限）。
	if v := os.Getenv("AGMEM_PREFIX_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.PrefixBudget = n
		}
	}
	if c.MySQLDSN == "" {
		return c, fmt.Errorf("config: AGMEM_MYSQL_DSN 必填")
	}
	if c.AIGatewayToken == "" && c.QianfanAPIKey == "" {
		return c, fmt.Errorf("config: AIGATEWAY_TOKEN 与 QIANFAN_API_KEY 至少其一必填")
	}
	if c.GLMToken == "" {
		c.GLMToken = c.AIGatewayToken // GLM 清洗复用 aigateway token
	}
	// 公开版不内置任何网关地址：设置了 token 就必须显式给出 base URL，
	// 否则错误会推迟到首个请求才以晦涩的网络错误暴露。
	if c.AIGatewayToken != "" && c.AIGatewayURL == "" {
		return c, fmt.Errorf("config: 设置 AIGATEWAY_TOKEN 时 AIGATEWAY_BASE_URL 必填")
	}
	if c.GLMToken != "" && c.GLMBaseURL == "" {
		return c, fmt.Errorf("config: 设置 AGMEM_GLM_TOKEN/AIGATEWAY_TOKEN 时 AGMEM_GLM_URL（或 AIGATEWAY_BASE_URL）必填")
	}
	if c.MachineToken == "" {
		return c, fmt.Errorf("config: AGMEM_MACHINE_TOKEN 必填")
	}
	// wiki 同步：仅在启用时校验（默认关闭的部署不受影响）。enabled=true 但缺凭证一律
	// fail-fast——需求明确要求「缺凭证时明确报错而不是静默不跑」。
	if c.WikiSyncEnabled {
		if c.WikiSyncAppID == "" || c.WikiSyncAppSecret == "" {
			return c, fmt.Errorf("config: AGMEM_WIKISYNC_ENABLED=true 但 AGMEM_WIKISYNC_APP_ID/AGMEM_WIKISYNC_APP_SECRET 缺失")
		}
		if c.WikiSyncSpaceID == "" {
			return c, fmt.Errorf("config: AGMEM_WIKISYNC_SPACE_ID 为空")
		}
		if _, err := wikisync.ParseHours(c.WikiSyncHours); err != nil {
			return c, fmt.Errorf("config: AGMEM_WIKISYNC_HOURS: %w", err)
		}
		if _, err := time.LoadLocation(c.WikiSyncTZ); err != nil {
			return c, fmt.Errorf("config: AGMEM_WIKISYNC_TZ: %w", err)
		}
	}
	return c, nil
}

// DefaultPrefixBudget 不变前缀 token 预算默认值 = cleaner 的同一常量（前缀构建器归
// cleaner 所有，此处仅转出别名，两包共用单一事实来源）。TestFromEnv_PrefixBudget 另
// 钉死其数值为 200000，改动须走设计拍板。
const DefaultPrefixBudget = cleaner.DefaultPrefixBudgetTokens

// DefaultScoreThreshold 检索分数阈值默认值（UC-1）：score < 0.5 的命中不返回。
// 对 AI 消费场景，低分命中 = 无关信息 = 性能恶化，宁缺毋滥。
const DefaultScoreThreshold = 0.5

// DefaultDedupCosine 语义去重默认阈值（见 internal/api/dedupe_semantic.go 实测依据）。
const DefaultDedupCosine = 0.8

// DefaultKB 全局唯一知识库名（单库化拍板 2026-09-08）：现有 collection `sre`
// 就是唯一的库，不迁移数据；服务端（AGMEM_KB）与 CLI/daemon wire 兼容字段
// 回填共用本常量，改默认名须三处同步评估（qdrant collection 须同名存在）。
const DefaultKB = "sre"

// DefaultQueryInstruction 查询侧 embedding 指令前缀默认值（2026-09-09 检索质量
// 批实测）：Qwen3-Embedding 非对称检索约定，只加在查询侧、文档侧不加。实测同一
// 口语化查询加前缀后正确条目 dense 0.58→0.69、噪声 0.53→0.43；存量 2577 向量
// 无需重索引。末尾换行与空格是前缀的一部分（模型训练格式），勿 Trim/改写。
const DefaultQueryInstruction = "Instruct: Given a search query, retrieve relevant passages that answer the query\nQuery: "

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// getenvBool 布尔环境变量：1/true/on/yes（大小写不敏感）=真，0/false/off/no=假，
// 空或其它值回落 def。
func getenvBool(k string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "":
		return def
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	default:
		return def
	}
}

// getenvFirst 取第一个非空环境变量。
func getenvFirst(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// splitNonEmpty 把逗号分隔的环境变量拆成非空项列表（逐项 TrimSpace，丢弃空项）。
// 用于 AGMEM_CLIENT_READONLY_TOKEN 这类「允许多个、以逗号分隔」的令牌集合；
// 未设置/全空返回 nil（调用方据此判定未配置）。
func splitNonEmpty(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
