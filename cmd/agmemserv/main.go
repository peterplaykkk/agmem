// agmem：智能记忆系统服务端（API + 清洗 worker）。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	// 内嵌 tzdata：运行镜像是 alpine（默认无 /usr/share/zoneinfo），
	// AGMEM_WIKISYNC_TZ=Asia/Shanghai 需 time.LoadLocation 可解析。
	_ "time/tzdata"

	"sre/agmem/internal/api"
	"sre/agmem/internal/auth"
	"sre/agmem/internal/cache"
	"sre/agmem/internal/cleaner"
	"sre/agmem/internal/config"
	"sre/agmem/internal/embedding"
	"sre/agmem/internal/qdrant"
	"sre/agmem/internal/store"
	"sre/agmem/internal/wikisync"
	webui "sre/agmem/web"
)

var version = "dev"

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("agmem: config: %v", err)
	}
	cfg.Version = version

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.MySQLDSN)
	if err != nil {
		log.Fatalf("agmem: store: %v", err)
	}
	defer st.DB.Close()

	embedder := embedding.New(cfg.AIGatewayURL, cfg.AIGatewayToken,
		cfg.QianfanBaseURL, cfg.QianfanAPIKey, cfg.EmbeddingModel, cfg.EmbedBatchSize, 3)
	// 查询侧指令前缀（非对称检索）：只作用于 ask 路径的 EmbedQuery；
	// worker 写入路径恒用 Embed，不读本字段（见 cleaner.Worker）。
	embedder.QueryInstruction = cfg.QueryInstruction
	qc := qdrant.New(cfg.QdrantBaseURL, cfg.QdrantAPIKey, cfg.QdrantTimeout)
	glm := cleaner.NewGLMClient(cfg.GLMBaseURL, cfg.GLMModel, cfg.GLMThinkingMode, cfg.GLMToken)

	srv := &api.Server{
		Cfg:      cfg,
		Store:    st,
		Embedder: embedder,
		Qdrant:   qc,
		LLM:      glm,
		Machine:  auth.NewMachineAuth(cfg.MachineToken),
		Readonly: auth.NewReadonlyAuth(cfg.ClientReadonlyTokens),
		Session:  auth.NewSession(24 * time.Hour),
		OIDC: auth.NewOIDC(auth.OIDCConfig{
			Issuer:       cfg.LogtoIssuer,
			ClientID:     cfg.LogtoClientID,
			ClientSecret: cfg.LogtoClientSecret,
		}, cfg.ExternalURL),
	}
	// 指标接线（G31）：GLM 调用/错误计数包装清洗客户端的 HTTP 传输层（不改
	// cleaner.LLM 接口，避免破坏 worker 对 AnchorCleaner/SourceCleaner 的类型断言）。
	metrics := srv.MetricsRegistry()
	glm.HTTP = metrics.InstrumentHTTPClient(glm.HTTP)
	// 清洗队列 gauge：后台周期采样（30s），指标端点本身不查库。
	go srv.RunQueueMetrics(ctx, 30*time.Second)

	// 进程内本地查询缓存（2026-09-11）：落 Pod 的 PVC（AGMEMD_DIR，缺省
	// $HOME/.cache/agmemd）。只读根文件系统（readOnlyRootFilesystem：true）且
	// HOME=/ 时该缺省路径不可写——openAskCache 对此 fail-open：禁用缓存、仅记
	// 日志，服务与 /api/ask 照常（绝不因缓存不可用而启动/请求失败）。
	if cs := openAskCache(); cs != nil {
		srv.Cache = cs
		srv.CacheMaxBytes = cacheMaxBytesFromEnv()
		defer cs.DB.Close()
	}
	// 命中阈值（与 agmemd 同源，AGMEMD_SIM_THRESHOLD；默认 SimilarityThreshold=0.8 不变）。
	// 注意：agmemserv 的缓存近邻用的是 qwen3-embedding 查询向量（1024d，与 agmemd 的
	// bge-base-zh 768d 不同），阈值默认沿用 0.8；如需按本模型标定，用本变量覆盖
	// （非法/越界静默回落默认，调优项不阻断启动）。
	if v := os.Getenv("AGMEMD_SIM_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
			cache.SetSimThreshold(f)
			log.Printf("agmem: ask cache sim threshold overridden to %.4f", f)
		}
	}

	// wiki 增量同步（AGMEM_WIKISYNC_ENABLED，默认 false）：同进程后台 goroutine，
	// 每小时（允许窗口内）扫飞书 SRE 知识库变更文档，复用本进程入队/删除路径。
	// 缺凭证/非法 hours/tz 已在 config.FromEnv 阶段 fail-fast。
	// wsState 提到外层作用域：清洗前缀的 L2 父目录钩子（下方 worker 段）需要读它。
	var wsState *wikisync.StateStore
	if cfg.WikiSyncEnabled {
		hours, err := wikisync.ParseHours(cfg.WikiSyncHours)
		if err != nil {
			log.Fatalf("agmem: wikisync hours: %v", err)
		}
		loc, err := time.LoadLocation(cfg.WikiSyncTZ)
		if err != nil {
			log.Fatalf("agmem: wikisync tz: %v", err)
		}
		wsState = &wikisync.StateStore{DB: st.DB}
		if err := wsState.EnsureSchema(ctx); err != nil {
			log.Fatalf("agmem: wikisync state: %v", err)
		}
		syncer := &wikisync.Syncer{
			Client: &wikisync.FeishuClient{
				AppID:     cfg.WikiSyncAppID,
				AppSecret: cfg.WikiSyncAppSecret,
			},
			SpaceID:   cfg.WikiSyncSpaceID,
			State:     wsState,
			KB:        srv,
			Hours:     hours,
			Loc:       loc,
			HoursSpec: cfg.WikiSyncHours,
			Timeout:   20 * time.Minute,
			OnStatus:  metrics.ObserveWikiSync,
		}
		srv.WikiSync = syncer
		go syncer.Run(ctx)
		log.Printf("agmem: wiki sync enabled space=%s hours=%s tz=%s",
			cfg.WikiSyncSpaceID, cfg.WikiSyncHours, cfg.WikiSyncTZ)
	}

	if cfg.WorkerEnabled {
		worker := &cleaner.Worker{
			Store:    st,
			Embedder: embedder,
			Qdrant:   qc,
			LLM:      glm,
			// 来源行数据源（G01）：条目 → documents.source_name；解析为空时
			// cleaner 回退原文首行标题 / 未命名文档（不阻断清洗）。
			Sources: st,
			// collection = kb 名（每 kb 一个 collection）；目录名从 directories 表解析
			CollectionResolver: func(kbID int64) string {
				name, err := st.KBNameByID(context.Background(), kbID)
				if err != nil {
					log.Printf("cleaner: resolve kb %d: %v", kbID, err)
					return ""
				}
				return name
			},
			DirResolver: func(dirID int64) string {
				name, err := st.GetDirName(context.Background(), dirID)
				if err != nil {
					log.Printf("cleaner: resolve dir %d: %v", dirID, err)
					return fmt.Sprintf("dir-%d", dirID)
				}
				return name
			},
			// 混合检索：clean 点随写随带 bm25 稀疏向量（AGMEM_HYBRID=off 时
			// 同步关闭，与查询路径一起回退纯稠密）。
			SparseEnabled: cfg.HybridEnabled,
			Interval:      cfg.WorkerInterval,
			// 前缀缓存观测（S3）：带前缀清洗的 usage → /metrics。
			OnCleanStats: metrics.ObserveCleanCache,
		}
		// 入队唤醒（F5 空轮询退避，2026-09-30）：API/wiki 同步成功入队条目后
		// Poke worker，退避立即恢复满速（Poke 非阻塞，不拖慢请求路径）。
		srv.OnEnqueue = worker.Poke
		// 清洗不变前缀（S1）：把 store 适配成 cleaner.PrefixSource（cleaner 不依赖
		// store 包）。未启用时显式日志告知，不静默降级。
		if cfg.PrefixEnabled {
			worker.PrefixSource = storePrefixSource{st: st}
			// L2 目录上下文（目标目录 + 父链祖先，**无层数上限**，能装多少由 L2 额度
			// 决定）：父目录钩子取自 wiki 同步 state 的父子/深度关系（TreeNodeMap）。
			// wiki 同步未启用、state 为空或查询失败 → nil / 空串，cleaner 降级为
			// 「仅同目录」。预算与 L1/L2 切分见 internal/cleaner/prefix.go。
			worker.PrefixBuilder = &cleaner.PrefixBuilder{
				Budget:        cfg.PrefixBudget,
				ParentDirFunc: newParentDirFunc(ctx, wsState),
			}
			log.Printf("agmem: clean prefix ENABLED（L0+L1+L2 不变前缀；轮=(kb,目标目录) 冻结；budget=%d tokens）", cfg.PrefixBudget)
		} else {
			log.Printf("agmem: clean prefix DISABLED（AGMEM_PREFIX=off 或 AGMEM_PREFIX=false）；清洗走无前缀路径")
		}
		go worker.Run(ctx)
	}

	// RootHandler：GET /metrics 在 SPA 兜底之前注册（webui 只透传 /api/*）。
	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.RootHandler(webui.Handler(srv.Routes())),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("agmem %s listening on %s", version, cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("agmem: listen: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// storePrefixSource 把 *store.Store 适配成 cleaner.PrefixSource：cleaner 只认自己的
// KnowledgeEntry，不反向依赖 store 包。适配器放这里（进程装配层）。
type storePrefixSource struct{ st *store.Store }

func (a storePrefixSource) KnowledgeEntries(ctx context.Context, kbID int64) ([]cleaner.KnowledgeEntry, error) {
	rows, err := a.st.KnowledgeEntries(ctx, kbID)
	if err != nil {
		return nil, err
	}
	out := make([]cleaner.KnowledgeEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, cleaner.KnowledgeEntry{
			ItemID:  r.ItemID,
			DocName: r.DocName,
			Dir:     r.Dir,
			Text:    r.Text,
		})
	}
	return out, nil
}

// newParentDirFunc 构造 cleaner 的父目录钩子（PrefixBuilder.ParentDirFunc）：给定目标
// 目录名（`<dir>-clean` 口径）返回其父目录名；沿父链**上溯到根、无层数上限**（成环/
// 父缺失即停）由 cleaner 侧做（contextDirChain）。
//
// 映射来自 wiki_sync_state 的父子关系（StateStore.TreeNodeMap）。worker 只在目录切换时
// 构建一次前缀（每次钩子调用查一次库），故按需查库、不缓存——wiki 同步回填新目录后自动生效。
// ws 为 nil（wiki 同步未启用）、映射为空或查询失败 → 返回 nil/空串，cleaner 降级为
// 「仅同目录」并记一次日志，不阻塞清洗。
func newParentDirFunc(ctx context.Context, ws *wikisync.StateStore) func(string) string {
	if ws == nil {
		log.Printf("agmem: clean prefix 父目录上下文不可用（wiki 同步未启用）：L2 仅同目录")
		return nil
	}
	var loggedEmpty, loggedErr bool
	return func(dir string) string {
		m, err := ws.TreeNodeMap(ctx)
		if err != nil {
			if !loggedErr {
				loggedErr = true
				log.Printf("agmem: clean prefix 父目录映射查询失败（L2 降级为仅同目录，仅记一次）: %v", err)
			}
			return ""
		}
		if len(m) == 0 {
			if !loggedEmpty {
				loggedEmpty = true
				log.Printf("agmem: clean prefix 父目录映射为空（L2 降级为仅同目录；wiki 同步回填后自动生效）")
			}
			return ""
		}
		return m[dir]
	}
}

// openAskCache 打开进程内查询缓存（AGMEMD_DIR，缺省 $HOME/.cache/agmemd）。
// fail-open：目录不可写 / 创建失败 → 返回 nil（缓存禁用），仅记日志，绝不阻断
// 启动或让 /api/ask 失败。只读根文件系统（readOnlyRootFilesystem）且 HOME=/ 时，
// 缺省路径必然不可写——此时缓存静默降级为「无缓存直连检索」，服务照常。
func openAskCache() *cache.Store {
	dir := os.Getenv("AGMEMD_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".cache", "agmemd")
	}
	cs, err := cache.OpenCache(dir)
	if err != nil {
		log.Printf("agmem: ask cache DISABLED（目录不可写，fail-open）dir=%s: %v", dir, err)
		return nil
	}
	log.Printf("agmem: ask cache enabled dir=%s max_bytes=%d", dir, cacheMaxBytesFromEnv())
	return cs
}

// cacheMaxBytesFromEnv 解析 AGMEMD_MAX_BYTES（LRU 上限）；非法/非正回落默认 500MB。
func cacheMaxBytesFromEnv() int64 {
	if v := os.Getenv("AGMEMD_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return cache.DefaultMaxBytes
}
