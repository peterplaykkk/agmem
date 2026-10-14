# agmem — 向量化知识库服务（agent 长期记忆 / 团队知识库）

agmem 把原始文档（手动推送或飞书 wiki 同步）经 LLM 清洗、分条成 ≤600 字的**可执行知识条目**，
向量化后存入 Qdrant，对外提供**混合检索**（dense + BM25 稀疏，RRF 融合）API。
它面向两类消费者：AI agent（CLI / 本地守护缓存，毫秒级重复查询）与团队成员（内嵌 Web 控制台）。

English README: [README.md](README.md). 第三方依赖许可：[THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md).

## 组件

| 组件 | 路径 | 说明 |
|---|---|---|
| agmemserv | `cmd/agmemserv` | 服务端：HTTP API + 清洗 worker + 进程内查询缓存 + 内嵌 Web SPA（单静态二进制） |
| agmem | `cmd/agmem` | CLI：`ask`/`clear` 走本地 agmemd；`push`/`status`/`kb`/`forget`/`retry` 调服务端 API |
| agmemd | `cmd/agmemd` | 本机守护：SQLite 查询缓存 + 本地 embedding（bge-base-zh int8，ONNX）+ 知识库版本门禁 |
| agmem-migrate | `cmd/agmem-migrate` | 维护工具：存量点位重建、重分条、去重、重编号、稀疏向量回填、策展注入 |

```
                 ┌──────────────┐    ┌──────────────────┐
  agent / 用户 ──►│  agmemserv   │───►│ MySQL（元数据）    │
  （HTTP / CLI）  │  API + worker│───►│ Qdrant（向量）     │
                 └──────┬───────┘    └──────────────────┘
                        │ OpenAI 兼容网关（embedding + 清洗 LLM）
                 agmemd（本机缓存：SQLite + 本地 ONNX embedding）
```

## 工作原理

**入库管线**（异步，单条状态机 `pending → embedded → cleaning → cleaned | failed`）：

1. `POST /api/kb/{kb}/docs` 接收 `{dir, texts[], files[]}`，入库前按 `original_text` 查重（重复直接跳过）；
2. worker 先嵌入原文（dense）并写入 `original` 点；
3. LLM 清洗为知识条目：每条以主题句开头、附「来源：<文档名>」行，具体数值/端口/路径/命令保留原样，
   清洗后做**锚点保真校验**（IP/端口/路径/版本/域名缺失则严格重清一次，仍缺记 `failed`）；
4. 按 markdown 结构分条为 **≤600 字**的独立条目（`<15` 字微型条目剔除），每条单独嵌入并写入
   `clean` 点，同时写入 **BM25 字符 2-gram 稀疏向量**（`AGMEM_HYBRID=off` 时停写）。

**检索**（`POST /api/ask`）：

- 查询向量带 Qwen3 非对称检索**指令前缀**（只加查询侧，文档侧不加，见 `AGMEM_QUERY_INSTRUCT`）；
- Qdrant 双路 prefetch（dense / bm25 各 top-20）+ `fusion: rrf` 融合（k=2）；
- 保留条件 = **dense_score ≥ `AGMEM_SCORE_THRESHOLD`（默认 0.5）或 sparse 强命中**
  （rank ≤ 3 且 ≥ 0.6×该查询榜首 且 ≥ 1.0）；排序恒用 RRF 分；
- 返回前做语义去重折叠（余弦 ≥ `AGMEM_DEDUP_COSINE`，默认 0.8 只保留最长一条）。

**两级缓存**：服务端进程内缓存（`AGMEMD_DIR`，纯 Go SQLite）+ CLI 侧 agmemd；
按 `sha256(dir|question)` **精确命中完全免 embedding**（实测 1.1s → 15ms 量级），
向量近邻路径用于改写/近义问句（阈值 `AGMEMD_SIM_THRESHOLD`）。命中同时校验知识库版本
（`/api/version/{kb}`），版本一变缓存整体失效回上游——保守但正确。

**清洗不变前缀**：把「已提取的知识」构造成逐字节稳定前缀（L0 规则 + L1 全库 + L2 目录上下文），
显著提升上游模型的 KV 前缀缓存命中率，把长 prompt 的计费从全额降到增量。
设计与实测依据见 [docs/prefix-caching.md](docs/prefix-caching.md)。

## 快速开始

前置依赖：

| 依赖 | 要求 |
|---|---|
| MySQL | 8.x（元数据；迁移经 `go:embed` 启动时幂等执行） |
| Qdrant | ≥ 1.18（命名稀疏向量；实测 1.19）。单 collection，与 `AGMEM_KB` 同名 |
| OpenAI 兼容网关 | 提供 embedding（默认 `qwen3-embedding-0.6b`，1024 维，query 侧指令前缀见 `AGMEM_QUERY_INSTRUCT`）与 chat 清洗模型（默认 `glm-5.3-flash`，可配任意 OpenAI 兼容模型名） |
| Go | 1.24+（`go.mod`；Dockerfile 用 1.25） |
| Node | 22+（仅构建 Web；`web/dist` 经 `go:embed` 内嵌进服务端二进制） |

### 构建

```sh
# 1) 先构建 Web（//go:embed all:dist 要求 web/dist 存在，否则 go build 失败）
cd web && npm ci && npm run build && cd ..

# 2) 服务端（静态、纯 Go）
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o agmemserv ./cmd/agmemserv

# 3) 本机工具（平台原生构建；agmemd 是 cgo，onnxruntime 运行时 dlopen）
go build -trimpath -o ~/.local/bin/agmem ./cmd/agmem
CGO_ENABLED=1 go build -trimpath -o ~/.local/bin/agmemd ./cmd/agmemd
```

`agmemd` 运行需 onnxruntime 共享库：macOS `brew install onnxruntime`（自动搜索
`/opt/homebrew/lib`、`/usr/local/lib`、`~/.cache/agmemd/lib`），Linux 下载
`libonnxruntime.so` 放入 `~/.cache/agmemd/lib/` 或由 `AGMEMD_ORT_LIB` 指定；
首次 `ask` 会自动拉起 agmemd 并下载 bge-base-zh int8 模型（约 98MB）。

### 运行服务端

```sh
export AGMEM_MYSQL_DSN='agmem:pass@tcp(127.0.0.1:3306)/agmem?parseTime=true&charset=utf8mb4'
export AGMEM_MACHINE_TOKEN='<机器 token，CLI 用>'
export AIGATEWAY_TOKEN='<网关 token>'
export AIGATEWAY_BASE_URL='https://gateway.example.com/v1'   # 设置 token 时必填
export AGMEM_QDRANT_URL='http://localhost:6333'              # 默认即此；生产指向你的 Qdrant
export AGMEM_QDRANT_API_KEY='<qdrant api key，未启用鉴权可空>'
./agmemserv

curl -s localhost:8080/api/healthz
curl -s -X POST localhost:8080/api/ask -H "Authorization: Bearer $AGMEM_MACHINE_TOKEN" \
  -H 'Content-Type: application/json' -d '{"query":"统一出口代理地址"}'
```

### CLI 与本地缓存

```sh
export AGMEM_URL='http://localhost:8080'      # 或你的服务端地址
export AGMEM_TOKEN="$AGMEM_MACHINE_TOKEN"

agmem push --dir handbook -t "MySQL binlog 默认保留 3 天，用 expire_logs_days 控制"
agmem status                                   # 查看任务/条目进度
agmem ask "统一出口代理地址"                     # 首次自动拉起 agmemd（下载本地模型）
agmem ask --dir handbook --k 10 "binlog 保留几天"
agmem forget --id 42 --yes                     # 删除条目（不可逆，版本 +1）
agmem retry --id 42                            # 重试 failed 条目
agmem completion bash > ~/.local/share/bash-completion/completions/agmem
```

## 服务端环境变量（`internal/config`）

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| AGMEM_MYSQL_DSN | 是 | — | 例 `user:pass@tcp(host:3306)/agmem?parseTime=true&charset=utf8mb4` |
| AGMEM_MACHINE_TOKEN | 是 | — | CLI 机器 Bearer token（sha256 后恒时比较） |
| AIGATEWAY_TOKEN | 是* | — | 网关 token（embedding 主通道 + 清洗共用；*与 QIANFAN_API_KEY 至少其一） |
| AIGATEWAY_BASE_URL | 条 | — | 网关 OpenAI 兼容基地址（**设置 AIGATEWAY_TOKEN 时必填**，启动即校验） |
| QIANFAN_API_KEY | 否 | — | 百度千帆直连 fallback（主通道失败自动降级） |
| AGMEM_QIANFAN_URL | 否 | https://qianfan.baidubce.com | 千帆 API 基地址 |
| AGMEM_LISTEN | 否 | :8080 | 监听地址 |
| AGMEM_KB | 否 | sre | 全局唯一知识库名（须与 Qdrant collection 同名） |
| AGMEM_QDRANT_URL | 否 | http://localhost:6333 | Qdrant 地址 |
| AGMEM_QDRANT_API_KEY | 否 | — | Qdrant api-key（全请求带 `api-key` header） |
| AGMEM_QDRANT_DIM | 否 | 1024 | 向量维度 |
| AGMEM_EMBEDDING_MODEL | 否 | qwen3-embedding-0.6b | embedding 模型名 |
| AGMEM_QUERY_INSTRUCT | 否 | Qwen3 检索指令前缀（见下） | 查询侧指令前缀；字面 `off` 禁用；末尾换行/空格是语义一部分 |
| AGMEM_GLM_URL | 否 | `$AIGATEWAY_BASE_URL` | 清洗 LLM 基地址（缺省回落网关地址） |
| AGMEM_GLM_MODEL | 否 | glm-5.3-flash | 清洗模型（任意 OpenAI 兼容模型名） |
| AGMEM_GLM_TOKEN | 否 | `$AIGATEWAY_TOKEN` | 清洗 token |
| AGMEM_GLM_THINKING | 否 | medium | 思考档位（透传上游可配参数） |
| AGMEM_GLM_TIMEOUT | 否 | 300s | 单次清洗 HTTP 超时（长文分片清洗可达分钟级，勿低于该量级） |
| AGMEM_GLM_PROBE | 否 | on | `off` 关闭 GET /models 探测降级链 |
| AGMEM_EXTERNAL_URL | 否* | — | 对外基地址（OIDC `redirect_uri={}/api/callback`；*配 OIDC 时必填） |
| AGMEM_LOGTO_ISSUER / _CLIENT_ID / _CLIENT_SECRET | 否 | — | Logto OIDC（浏览器登录身份 = 知识库 owner_sub） |
| AGMEM_CLIENT_READONLY_TOKEN | 否 | — | 只读客户端令牌（逗号分隔可多个）：仅放行 ask/version/me，其余 404 掩蔽 |
| AGMEM_WORKER_INTERVAL | 否 | 2s | 清洗任务轮询间隔 |
| AGMEM_WORKER_ENABLED | 否 | true | `0`/`false` 关闭 worker（只读部署） |
| AGMEM_SCORE_THRESHOLD | 否 | 0.5 | 检索保留阈值（dense 维度；sparse 强命中相对门槛豁免，见「工作原理」） |
| AGMEM_DEDUP_COSINE | 否 | 0.8 | 语义去重折叠阈值（0 = 关闭） |
| AGMEM_HYBRID | 否 | on | `off` 回退纯稠密检索并停写稀疏向量（回滚阀） |
| AGMEM_PREFIX | 否 | on | 清洗不变前缀开关（`off`/`false`/`0` 关闭，见 docs/prefix-caching.md） |
| AGMEM_PREFIX_BUDGET | 否 | 200000 | 不变前缀单请求 token 预算上限 |
| AGMEMD_DIR | 否 | `$HOME/.cache/agmemd` | 进程内查询缓存目录（只读根文件系统下不可写则 fail-open 禁用缓存） |
| AGMEMD_MAX_BYTES | 否 | 524288000 | 缓存 LRU 上限（500MB） |
| AGMEMD_SIM_THRESHOLD | 否 | 0.8 | 缓存近邻命中阈值（按所用 embedding 模型标定后再放开；同问题由精确命中路径承担，恒正确） |
| AGMEM_WIKISYNC_ENABLED | 否 | false | 飞书 wiki 增量同步（同进程后台 goroutine） |
| AGMEM_WIKISYNC_SPACE_ID | 条* | — | 目标 wiki 空间 id（*enabled=true 时必填） |
| AGMEM_WIKISYNC_APP_ID / _APP_SECRET | 否* | — | 飞书自建应用凭证（*enabled=true 时必填，缺一启动即报错） |
| AGMEM_WIKISYNC_HOURS | 否 | 0-8,12-13,18-23 | 允许执行的小时集合（每 30 分钟一个槽；避开上游模型高峰） |
| AGMEM_WIKISYNC_TZ | 否 | Asia/Shanghai | 调度时区（二进制内嵌 tzdata） |

> 查询指令前缀默认值：`Instruct: Given a search query, retrieve relevant passages that answer the query\nQuery: `
> （Qwen3-Embedding 非对称检索约定；**只加查询侧、文档侧不加**，给文档侧也加会破坏召回）。

## CLI / agmemd 环境变量

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| AGMEM_URL | 是 | — | 服务端地址（agmemd 启动必填） |
| AGMEM_TOKEN | 是 | — | 机器 Bearer token |
| AGMEM_KB | 否 | sre | 仅用于回填 wire 兼容字段（单库版服务端忽略请求 kb） |
| AGMEMD_ADDR | 否 | 127.0.0.1:8377 | 守护监听地址（仅回环） |
| AGMEMD_DIR | 否 | ~/.cache/agmemd | 缓存/模型/日志目录 |
| AGMEMD_MAX_BYTES | 否 | 524288000 | 本地缓存 LRU 上限 |
| AGMEMD_SIM_THRESHOLD | 否 | 0.8 | 本地命中相似度阈值 |
| AGMEMD_EP | 否 | cpu | 执行后端：`cpu`｜`coreml`（macOS）｜`cuda`/`rocm`（预留） |
| AGMEMD_ORT_LIB | 否 | 平台默认名 | onnxruntime 共享库路径 |
| LOCAL_EMBED_MODEL_URL / LOCAL_EMBED_VOCAB_URL | 否 | hf-mirror Xenova/bge-base-zh-v1.5 | 本地模型下载源 |

端点（127.0.0.1）：`GET /healthz`、`POST /api/ask {query,dir,top_k}`、`POST /api/clear {id?|all}`。

## 服务端 API 一览

| 方法 路径 | 说明 |
|---|---|
| GET /api/healthz | 健康检查（含 wiki_sync 状态段） |
| GET /api/me | 当前身份（sub/role/kb_count） |
| GET /api/login, /api/callback | OIDC 授权码登录（配 Logto；owner_sub = OIDC sub） |
| GET /api/kb | 知识库列表（owner+reader 可见；含目录数/条目数/版本/role） |
| POST /api/kb | 建知识库（自动建同名 Qdrant collection） |
| GET /api/kb/{kb}/dirs | 一级目录列表 |
| POST /api/kb/{kb}/dir | 建一级目录（仅 owner） |
| POST /api/kb/{kb}/docs | 上传 `{dir, texts[], files[]}` → 202 `task_id`（异步管线；重复内容自动去重，全重复返回 `status=duplicate`） |
| GET /api/kb/{kb}/tasks/{task_id} | 任务与条目状态 |
| GET /api/kb/{kb}/points?status=&page=&page_size= | 分页列条目 |
| DELETE /api/kb/{kb}/items/{item_id} | 删除条目（owner-only；显式点 id 清单删除，版本 +1） |
| POST /api/kb/{kb}/items/{item_id}/retry | 重试 failed 条目（owner-only） |
| GET / POST /api/kb/{kb}/readers, DELETE …/readers/{sub} | 读者管理（owner-only，幂等） |
| POST /api/ask | 查询 `{query, dir, top_k}`（恒全库，`dir` 可收窄单目录；返回 hit 带 rrf 分/dense 分/sparse 名次） |
| GET /api/version/{kb} | 知识库数据版本号（缓存失效锚点） |
| GET /api/wiki-sync/status | wiki 同步状态（需鉴权） |
| GET /metrics | Prometheus 文本指标（手写 Registry，零第三方依赖；**公网入口建议拒绝**） |

权限：机器 token 全量放行；OIDC 登录按 kb owner/reader 过滤（无权 kb 以 404 掩蔽）；
写操作（docs、目录、生命周期）仅 owner（reader 403、非读者 404 掩蔽）。

## 数据模型

MySQL（`internal/store/migrations/*.sql`，go:embed 幂等迁移）：
`knowledge_bases` / `directories` / `documents` / `tasks` / `task_items` / `kb_versions` / `kb_readers` / `wiki_sync_state`。
Qdrant 每个知识库一个 collection：`point_type=original`（全文单点）与 `point_type=clean`（分条知识），
clean 点带 `entry_index` / `entry_count` / `bm25` 稀疏向量；条目 id 有**保留段约定**
（`SRC_FILE` 直写注入必须用 `item_id ≥ 900000`，避免与 MySQL 自增空间冲突）。

任务级状态（`tasks.status`）：全部 pending → `pending`；有进行中条目 → `cleaning`；
全部终态无 failed → `completed`；有 failed → `completed_with_errors`（部分成功语义，已清洗条目保留可查）。

## 维护工具（`cmd/agmem-migrate`）

存入量点位结构升级、修复类作业；默认 DRY_RUN 演示，`DRY_RUN=0` 实际执行：

```sh
DRY_RUN=1 COLLECTION=sre QDRANT_URL=http://localhost:6333 go run ./cmd/agmem-migrate
```

常用模式（环境变量开关）：长点位重建（默认）、`RESPLIT=1`（存量条目重分条）、
`DEDUP=1`（重复点清理）、`REINDEX_ENTRY=1`（条目重编号）、`SET_SOURCE=1`（来源回填）、
`SPARSE=1`（存量点回填 BM25 稀疏向量）、`SRC_FILE=<jsonl>`（策展/迁移注入，走保留段 id，
显式 `SRC_ALLOW_REAL_IDS=1` 才允许真实 id）。需要 MySQL 直连（版本 bump + 执行锁）时设 `AGMEM_MYSQL_DSN`。

## 飞书 wiki 增量同步（可选）

`AGMEM_WIKISYNC_ENABLED=true` 时，服务端内置后台任务每 30 分钟在允许小时窗口内扫描目标 wiki 空间：
`obj_edit_time` 变化的 docx 重新入队清洗（**先入队新版本、再删旧版本**；未变文档零外部调用）。
非 docx 跳过；文档消失则删条目与 state 行。state 表在 MySQL。凭证缺失/参数非法时启动即报错，不静默不跑。

## 测试

```sh
go test ./...        # 需 MySQL 的 store 集成测试在未设 AGMEM_TEST_MYSQL_DSN 时自动 skip；
                     # 依赖 onnxruntime 的用例在库缺失时自动 skip（CI 友好）
go vet ./...
```

## Docker

```sh
docker build -t agmem .            # 多阶段：构建 Web SPA → 静态服务端二进制 → alpine 非 root(10001)
docker run --rm -p 8080:8080 -e AGMEM_MYSQL_DSN=... -e AGMEM_MACHINE_TOKEN=... \
  -e AIGATEWAY_TOKEN=... -e AIGATEWAY_BASE_URL=... agmem
```

镜像只包含 `agmemserv`。`agmem`/`agmemd` 是用户主机上的本地工具，按「构建」一节在目标平台原生编译。
容器内挂载可写卷到 `AGMEMD_DIR` 可启用进程内查询缓存（只读根文件系统时缓存自动禁用、服务不受影响）。

## 许可

0BSD（BSD Zero Clause License）——零条件、不限制任何使用与分发方式（见 [LICENSE](LICENSE)）。
第三方依赖与运行时组件（onnxruntime、bge 模型等）见 [THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md)。
