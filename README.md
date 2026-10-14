# agmem — vectorized knowledge-base service (long-term memory for AI agents, shared team knowledge)

agmem turns raw documents (pushed manually or synced from a wiki) into
**executable knowledge entries of ≤600 characters** via LLM cleaning, embeds
them into Qdrant, and serves **hybrid retrieval** (dense + BM25 sparse, RRF
fusion) over HTTP. It has two kinds of consumers: AI agents (a CLI with a local
caching daemon for millisecond repeat queries) and team members (an embedded
web console).

中文说明: [README_ZH.md](README_ZH.md). Third-party licenses: [THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md).

## Components

| Component | Path | Description |
|---|---|---|
| agmemserv | `cmd/agmemserv` | Server: HTTP API + cleaning worker + in-process query cache + embedded web SPA (single static binary) |
| agmem | `cmd/agmem` | CLI: `ask`/`clear` go through the local daemon; `push`/`status`/`kb`/`forget`/`retry` call the server API |
| agmemd | `cmd/agmemd` | Local daemon: SQLite query cache + local ONNX embedding (bge-base-zh int8) + kb-version gate |
| agmem-migrate | `cmd/agmem-migrate` | Maintenance tool: point rebuild, re-split, dedup, re-index, sparse backfill, curated injection |

```
                 ┌──────────────┐    ┌──────────────────┐
  agent / user ──►│  agmemserv   │───►│ MySQL (metadata) │
  (HTTP / CLI)   │  API + worker│───►│ Qdrant (vectors) │
                 └──────┬───────┘    └──────────────────┘
                        │ OpenAI-compatible gateway (embedding + cleaning LLM)
                 agmemd (local cache: SQLite + local ONNX embedding)
```

## How it works

**Ingestion pipeline** (async, per-item state machine
`pending → embedded → cleaning → cleaned | failed`):

1. `POST /api/kb/{kb}/docs` accepts `{dir, texts[], files[]}`; identical
   `original_text` is deduplicated before enqueueing;
2. the worker embeds the raw text (dense) and upserts an `original` point;
3. the LLM cleans it into knowledge entries: each entry starts with a topic
   sentence and ends with a `Source: <document>` line; concrete values (IPs,
   ports, paths, versions, commands) must be preserved verbatim. After
   cleaning, an **anchor-fidelity check** re-runs a strict pass if any
   IP/port/path/version/domain anchor was lost — still missing means `failed`;
4. entries are split by markdown structure into independent **≤600-rune**
   entries (entries under 15 runes are dropped), each embedded and upserted as
   a `clean` point with a **BM25 character 2-gram sparse vector** (skipped when
   `AGMEM_HYBRID=off`).

**Retrieval** (`POST /api/ask`):

- the query embedding carries the Qwen3 asymmetric **instruction prefix**
  (query side only — never the document side; see `AGMEM_QUERY_INSTRUCT`);
- Qdrant runs dual prefetch (dense / bm25, top-20 each) with `fusion: rrf`
  (k=2);
- a hit is kept when **dense_score ≥ `AGMEM_SCORE_THRESHOLD` (default 0.5) or
  it is a strong sparse hit** (rank ≤ 3 and ≥ 0.6× the sparse leader and
  ≥ 1.0); ordering always uses the RRF score;
- results are semantically folded for duplicates (cosine ≥
  `AGMEM_DEDUP_COSINE`, default 0.8, keeping the longest).

**Two-level cache**: the server keeps an in-process cache (`AGMEMD_DIR`,
pure-Go SQLite) and the CLI runs agmemd. The exact-hit path
(`sha256(dir|question)`) **skips embedding entirely** (measured ~1.1s → ~15ms
for repeat questions); a vector nearest-neighbor path serves paraphrased
questions (`AGMEMD_SIM_THRESHOLD`). Cache hits are gated on the kb data version
(`/api/version/{kb}`): any knowledge change invalidates the cache back to the
upstream — conservative and always correct.

**Stable cleaning prefix**: already-extracted knowledge is assembled into a
byte-stable prompt prefix (L0 rules + L1 whole-store + L2 directory context) to
maximize the upstream model's KV prefix-cache hit rate, turning long prompts
from full-price into incremental-price. Design and measurements:
[docs/prefix-caching.md](docs/prefix-caching.md).

## Quick start

Prerequisites:

| Dependency | Requirement |
|---|---|
| MySQL | 8.x (metadata; embedded idempotent migrations run at startup) |
| Qdrant | ≥ 1.18 (named sparse vectors; tested on 1.19). One collection, named the same as `AGMEM_KB` |
| OpenAI-compatible gateway | embedding endpoint (default `qwen3-embedding-0.6b`, 1024-dim, query-side instruction prefix) and a chat model for cleaning (default `glm-5.3-flash`, any OpenAI-compatible model id) |
| Go | 1.24+ (`go.mod`; the Dockerfile uses 1.25) |
| Node | 22+ (web build only; `web/dist` is embedded into the server binary via `go:embed`) |

### Build

```sh
# 1) build the web SPA first (//go:embed all:dist requires web/dist to exist)
cd web && npm ci && npm run build && cd ..

# 2) server (static, pure Go)
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o agmemserv ./cmd/agmemserv

# 3) host-side tools (build natively for the target platform; agmemd is cgo
#    and loads onnxruntime at run time)
go build -trimpath -o ~/.local/bin/agmem ./cmd/agmem
CGO_ENABLED=1 go build -trimpath -o ~/.local/bin/agmemd ./cmd/agmemd
```

`agmemd` needs the onnxruntime shared library: macOS `brew install onnxruntime`
(searched in `/opt/homebrew/lib`, `/usr/local/lib`, `~/.cache/agmemd/lib`),
Linux place `libonnxruntime.so` in `~/.cache/agmemd/lib/` or point
`AGMEMD_ORT_LIB` at it. The first `ask` starts the daemon automatically and
downloads the bge-base-zh int8 model (~98MB).

### Run the server

```sh
export AGMEM_MYSQL_DSN='agmem:pass@tcp(127.0.0.1:3306)/agmem?parseTime=true&charset=utf8mb4'
export AGMEM_MACHINE_TOKEN='<machine token used by the CLI>'
export AIGATEWAY_TOKEN='<gateway token>'
export AIGATEWAY_BASE_URL='https://gateway.example.com/v1'  # required whenever AIGATEWAY_TOKEN is set
export AGMEM_QDRANT_URL='http://localhost:6333'             # default; point at your Qdrant
export AGMEM_QDRANT_API_KEY='<qdrant api key; empty if auth is off>'
./agmemserv

curl -s localhost:8080/api/healthz
curl -s -X POST localhost:8080/api/ask -H "Authorization: Bearer $AGMEM_MACHINE_TOKEN" \
  -H 'Content-Type: application/json' -d '{"query":"how do repeated queries get fast"}'
```

### CLI and local cache

```sh
export AGMEM_URL='http://localhost:8080'      # or wherever the server runs
export AGMEM_TOKEN="$AGMEM_MACHINE_TOKEN"

agmem push --dir handbook -t "MySQL binlog is kept for 3 days; configured via expire_logs_days"
agmem status                                   # task / entry progress
agmem ask "how do repeated queries get fast"   # first run boots agmemd and downloads the local model
agmem ask --dir handbook --k 10 "binlog retention"
agmem forget --id 42 --yes                     # delete an entry (irreversible; kb version +1)
agmem retry --id 42                            # retry a failed entry
agmem completion bash > ~/.local/share/bash-completion/completions/agmem
```

## Server environment variables (`internal/config`)

| Variable | Required | Default | Meaning |
|---|---|---|---|
| AGMEM_MYSQL_DSN | yes | — | e.g. `user:pass@tcp(host:3306)/agmem?parseTime=true&charset=utf8mb4` |
| AGMEM_MACHINE_TOKEN | yes | — | machine Bearer token for the CLI (compared as sha256, constant-time) |
| AIGATEWAY_TOKEN | yes* | — | gateway token (embedding main channel + cleaning; *at least one of this / QIANFAN_API_KEY) |
| AIGATEWAY_BASE_URL | cond | — | gateway OpenAI-compatible base URL (**required when AIGATEWAY_TOKEN is set**; validated at startup) |
| QIANFAN_API_KEY | no | — | Baidu Qianfan direct fallback (used when the main channel fails) |
| AGMEM_QIANFAN_URL | no | https://qianfan.baidubce.com | Qianfan API base |
| AGMEM_LISTEN | no | :8080 | listen address |
| AGMEM_KB | no | sre | the single global knowledge base name (must match the Qdrant collection) |
| AGMEM_QDRANT_URL | no | http://localhost:6333 | Qdrant endpoint |
| AGMEM_QDRANT_API_KEY | no | — | Qdrant api-key (`api-key` header on every request) |
| AGMEM_QDRANT_DIM | no | 1024 | vector dimension |
| AGMEM_EMBEDDING_MODEL | no | qwen3-embedding-0.6b | embedding model id |
| AGMEM_QUERY_INSTRUCT | no | Qwen3 retrieval instruction (below) | query-side instruction prefix; literal `off` disables; trailing newline/space are part of the prefix |
| AGMEM_GLM_URL | no | `$AIGATEWAY_BASE_URL` | cleaning LLM base URL (falls back to the gateway URL) |
| AGMEM_GLM_MODEL | no | glm-5.3-flash | cleaning model (any OpenAI-compatible model id) |
| AGMEM_GLM_TOKEN | no | `$AIGATEWAY_TOKEN` | cleaning token |
| AGMEM_GLM_THINKING | no | medium | thinking mode (passed through to the upstream) |
| AGMEM_GLM_TIMEOUT | no | 300s | per-request cleaning timeout (long chunks take minutes; do not lower much) |
| AGMEM_GLM_PROBE | no | on | `off` disables the GET /models probe for the degrade chain |
| AGMEM_EXTERNAL_URL | no* | — | external base URL (OIDC `redirect_uri={}/api/callback`; *required with OIDC) |
| AGMEM_LOGTO_ISSUER / _CLIENT_ID / _CLIENT_SECRET | no | — | Logto OIDC (browser login identity = kb owner_sub) |
| AGMEM_CLIENT_READONLY_TOKEN | no | — | read-only client tokens (comma-separated): only ask/version/me are allowed, everything else 404-masked |
| AGMEM_WORKER_INTERVAL | no | 2s | cleaning queue poll interval |
| AGMEM_WORKER_ENABLED | no | true | `0`/`false` disables the worker (read-only deployment) |
| AGMEM_SCORE_THRESHOLD | no | 0.5 | retrieval keep threshold (dense dimension; strong sparse hits are exempt, see "How it works") |
| AGMEM_DEDUP_COSINE | no | 0.8 | semantic dedup fold threshold (0 = off) |
| AGMEM_HYBRID | no | on | `off` reverts to dense-only retrieval and stops sparse writes (rollback valve) |
| AGMEM_PREFIX | no | on | stable cleaning prefix (`off`/`false`/`0` disables; see docs/prefix-caching.md) |
| AGMEM_PREFIX_BUDGET | no | 200000 | per-request prefix token budget |
| AGMEMD_DIR | no | `$HOME/.cache/agmemd` | in-process query cache directory (fail-open: read-only rootfs disables the cache, the service keeps working) |
| AGMEMD_MAX_BYTES | no | 524288000 | cache LRU cap (500MB) |
| AGMEMD_SIM_THRESHOLD | no | 0.8 | cache nearest-hit threshold (calibrate for your embedding model before raising; exact repeats are handled by the always-correct exact-hit path) |
| AGMEM_WIKISYNC_ENABLED | no | false | wiki incremental sync (in-process background goroutine) |
| AGMEM_WIKISYNC_SPACE_ID | cond* | — | target wiki space id (*required when enabled) |
| AGMEM_WIKISYNC_APP_ID / _APP_SECRET | no* | — | wiki app credentials (*required when enabled; startup fails fast if missing) |
| AGMEM_WIKISYNC_HOURS | no | 0-8,12-13,18-23 | allowed hours (one slot per 30 minutes; avoids the upstream model's peak hours) |
| AGMEM_WIKISYNC_TZ | no | Asia/Shanghai | schedule timezone (tzdata is embedded in the binary) |

> Default query instruction: `Instruct: Given a search query, retrieve relevant passages that answer the query\nQuery: `
> (Qwen3-Embedding asymmetric retrieval; **query side only** — adding it to the
> document side breaks recall).

## CLI / agmemd environment variables

| Variable | Required | Default | Meaning |
|---|---|---|---|
| AGMEM_URL | yes | — | server address (also required by agmemd at startup) |
| AGMEM_TOKEN | yes | — | machine Bearer token |
| AGMEM_KB | no | sre | wire-compat field only (the single-kb server ignores request kb) |
| AGMEMD_ADDR | no | 127.0.0.1:8377 | daemon listen address (loopback only) |
| AGMEMD_DIR | no | ~/.cache/agmemd | cache / model / log directory |
| AGMEMD_MAX_BYTES | no | 524288000 | local cache LRU cap |
| AGMEMD_SIM_THRESHOLD | no | 0.8 | local hit similarity threshold |
| AGMEMD_EP | no | cpu | execution provider: `cpu`｜`coreml` (macOS)｜`cuda`/`rocm` (reserved) |
| AGMEMD_ORT_LIB | no | platform default | path to the onnxruntime shared library |
| LOCAL_EMBED_MODEL_URL / LOCAL_EMBED_VOCAB_URL | no | hf-mirror Xenova/bge-base-zh-v1.5 | local model download source |

Daemon endpoints (127.0.0.1): `GET /healthz`, `POST /api/ask {query,dir,top_k}`,
`POST /api/clear {id?|all}`.

## Server API summary

| Method Path | Description |
|---|---|
| GET /api/healthz | health check (includes the wiki_sync status segment) |
| GET /api/me | current identity (sub/role/kb_count) |
| GET /api/login, /api/callback | OIDC authorization-code login (with Logto; owner_sub = OIDC sub) |
| GET /api/kb | list knowledge bases (owner+reader visible; dirs/entries/version/role) |
| POST /api/kb | create a knowledge base (auto-creates the same-named Qdrant collection) |
| GET /api/kb/{kb}/dirs | list top-level directories |
| POST /api/kb/{kb}/dir | create a top-level directory (owner only) |
| POST /api/kb/{kb}/docs | upload `{dir, texts[], files[]}` → 202 `task_id` (async; duplicates skipped, all-duplicate returns `status=duplicate`) |
| GET /api/kb/{kb}/tasks/{task_id} | task and item status |
| GET /api/kb/{kb}/points?status=&page=&page_size= | paginated entry listing |
| DELETE /api/kb/{kb}/items/{item_id} | delete an entry (owner only; explicit point-id list, kb version +1) |
| POST /api/kb/{kb}/items/{item_id}/retry | retry a failed entry (owner only) |
| GET / POST /api/kb/{kb}/readers, DELETE …/readers/{sub} | reader management (owner only, idempotent) |
| POST /api/ask | query `{query, dir, top_k}` (always whole-kb; `dir` narrows to one directory; hits carry rrf/dense/sparse scores) |
| GET /api/version/{kb} | knowledge-base data version (cache invalidation anchor) |
| GET /api/wiki-sync/status | wiki sync status (authenticated) |
| GET /metrics | Prometheus text metrics (hand-written registry, zero third-party deps; **consider denying this at your public ingress**) |

Authorization: the machine token passes everything; OIDC logins are filtered by
kb owner/reader (unauthorized kbs are 404-masked). Writes (docs, directories,
lifecycle) are owner-only (readers get 403, non-readers 404).

## Data model

MySQL (`internal/store/migrations/*.sql`, embedded idempotent migrations):
`knowledge_bases` / `directories` / `documents` / `tasks` / `task_items` /
`kb_versions` / `kb_readers` / `wiki_sync_state`. Qdrant stores one collection
per knowledge base: `point_type=original` (one full-text point) and
`point_type=clean` (split entries) carrying `entry_index` / `entry_count` / the
`bm25` sparse vector. Entry ids have a **reserved range convention**: entries
injected directly via `SRC_FILE` must use `item_id ≥ 900000` to avoid colliding
with MySQL auto-increment ids.

Task-level status (`tasks.status`): all pending → `pending`; any in progress →
`cleaning`; all terminal without failures → `completed`; with failures →
`completed_with_errors` (partial success: cleaned entries stay queryable).

## Maintenance tool (`cmd/agmem-migrate`)

For data-structure upgrades and repair jobs; DRY_RUN by default, set
`DRY_RUN=0` to execute:

```sh
DRY_RUN=1 COLLECTION=sre QDRANT_URL=http://localhost:6333 go run ./cmd/agmem-migrate
```

Modes (env switches): long-point rebuild (default), `RESPLIT=1` (re-split
stored entries), `DEDUP=1` (duplicate point cleanup), `REINDEX_ENTRY=1`
(entry re-indexing), `SET_SOURCE=1` (source backfill), `SPARSE=1` (backfill
BM25 sparse vectors on existing points), `SRC_FILE=<jsonl>` (curated/migration
injection using the reserved id range; real ids require an explicit
`SRC_ALLOW_REAL_IDS=1`). Set `AGMEM_MYSQL_DSN` for the MySQL parts (version
bump + execution lock).

## Wiki incremental sync (optional)

With `AGMEM_WIKISYNC_ENABLED=true` the server runs an in-process background
task: every 30 minutes, inside the allowed-hour window, it scans the target
wiki space and re-enqueues cleaning for docx documents whose `obj_edit_time`
changed (**new version enqueued first, old entries deleted after**; unchanged
documents cost zero external calls). Non-docx nodes are skipped; documents that
disappeared from the tree have their entries and state row removed. State lives
in MySQL. Missing credentials or invalid parameters fail at startup — the task
never silently does nothing.

## Tests

```sh
go test ./...        # MySQL-backed store tests skip without AGMEM_TEST_MYSQL_DSN;
                     # onnxruntime-dependent tests skip when the library is absent (CI-friendly)
go vet ./...
```

## Docker

```sh
docker build -t agmem .            # multi-stage: web SPA -> static server binary -> alpine, non-root (10001)
docker run --rm -p 8080:8080 -e AGMEM_MYSQL_DSN=... -e AGMEM_MACHINE_TOKEN=... \
  -e AIGATEWAY_TOKEN=... -e AIGATEWAY_BASE_URL=... agmem
```

The image contains only `agmemserv`. `agmem`/`agmemd` are host-side tools —
build them natively for the target platform (see "Build"). Mount a writable
volume at `AGMEMD_DIR` to enable the in-process query cache inside the
container (with a read-only rootfs the cache disables itself and the service is
unaffected).

## License

0BSD (BSD Zero Clause License) — no conditions, use and distribute however you
like (see [LICENSE](LICENSE)). Third-party dependencies and runtime components
(onnxruntime, the bge model, …) are listed in
[THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md).
