# Stable-prefix context for the cleaning pipeline

Design rationale and measured evidence for the prompt-prefix construction in
`internal/cleaner` (code comments referencing `docs/prefix-caching.md §N` point
here). The goal: turn already-extracted knowledge into a byte-stable prompt
prefix so the upstream model's KV prefix cache keeps hitting.

## 0. The problem

Knowledge cleaning (raw document → executable knowledge entries) used to call
the model **independently for every 4000-character chunk**: the model could not
see other documents or what it had extracted in previous rounds. Three
problems followed:

1. Multiple documents in the same directory repeat the same content → each one
   is extracted again → cross-document duplicate entries;
2. Entries lack global context (no idea what else exists in the system) and
   naming/terminology drifts;
3. Every request's prompt is brand new → prefix cache hit rate is 0 and long
   prompts are billed in full.

Solution: **use the already-extracted knowledge itself as context** — built
into a byte-stable **stable prefix** placed at the front of every cleaning
request.

## 1. Prefix layers (stable first, volatile last — a hard constraint)

```
L0 global contract (never changes)      cleaning rules + output format contract
L1 global knowledge prefix (append-only) all knowledge entries up to the previous round
L2 directory context (fixed within a round) entries of the current directory + its ancestor chain
L3 current target (unique per request)  coverage declaration + the document body
```

L2 is **not depth-capped**: from the target directory up the parent chain to the
root, as many layers as the L2 token budget allows (stop on missing parent or
cycle). Observed wiki trees are up to 5 levels deep; the old hard-coded "max 2
levels" would miss deeper ancestors, so it was removed.

**Why L1 and L2 must stay separate**: directory relevance and cache continuity
are in tension. If "current directory entries" were placed first, the prefix
would change with every directory → miss after miss across directories. Split
into two layers:

| Change dimension | Hits |
|---|---|
| Within one directory, document by document | L0+L1+L2 all hit, only L3 misses |
| Across directories | L0+L1 still hit (**L1 is a true prefix of L2**), only L2+L3 miss |

**The order is not interchangeable.** Caches match prefixes byte by byte; any
per-request content (timestamps, random ordering, counters) that leaks into
L0–L2 zeroes the hit rate immediately.

## 2. Round model

```
Round 0: no knowledge in the store yet → prefix is only L0
Round N: prefix = L0 + all knowledge entries as of the end of round N-1
```

- **Round = (kb, target directory)**: while the worker drains the queue, one
  **frozen** prefix is reused for the whole directory; it is rebuilt only when
  the directory (or kb) changes (`internal/cleaner/worker.go` `prefixFor` /
  `prefixSession`).
- **Freezing is the worker's job, the builder does not freeze**:
  `PrefixBuilder.Build` assembles the passed entries once; freezing means
  taking one entry snapshot (`store.KnowledgeEntries`) at build time and not
  re-reading during the round.
- **The prefix is byte-identical within a round** → all requests in a round
  share the cached prefix (the first request writes it, the rest hit).
- **It only changes between rounds** → the first request of a new round is a
  prefix miss, the rest hit.
- Round boundaries: documents are processed in DFS order; a round ends when the
  newly produced entries would push the prefix past the 200k budget
  (budget-driven round splitting is not implemented yet; the current boundary
  is a directory switch).

## 3. Append-only, never reorder (the highest-leverage rule)

The upstream cache is "cached prefix unit + exact match":

| Practice | Consequence |
|---|---|
| Append (round N prefix ⊂ round N+1 prefix) | old cache units are still prefixes of the new one → **still hit**, only the new tail misses |
| Insert / reorder (sort by directory, insert entries mid-prefix) | matching **breaks at the insertion point → the whole prefix invalidates**, full miss |

### 3.1 The two conditions that make append-only actually hold

1. **L1 must exclude entries of the current target directory (and its
   ancestors).** Otherwise this round's new entries would be inserted into the
   middle of L1, and the old L1 would no longer be a prefix of the new L1.
2. **L2 must be frozen at round start** (snapshot); this round's output must
   not rewrite it, or L2 would differ for every document in the round and the
   within-round hit rate would collapse to zero.

So the invariant is:

```
L1(round N+1) == L1(round N) + [entries produced in round N belonging to other directories]  # append at the tail only
L2(round)     == frozen snapshot of "target directory + ancestors" entries at round start      # byte-identical within the round
```

**Corollary**: L2 and L1 are **mutually exclusive** (entries of one directory
appear only in L2, never in L1), with no duplication. `PrefixBuilder.Build`
implements exactly this: the L1 loop `continue`s over entries in `dirSet`; the
L2 loop takes only entries in `dirSet`; `Prefix.L1Text` (the L0+L1 segment) is
exposed separately for the append-only negative assertion.

### 3.2 Expected hit matrix

| Scenario | L0 | L1 | L2 | L3 |
|---|---|---|---|---|
| Within one directory, doc by doc | hit | hit | hit | miss |
| Across directories (dir A → dir B) | hit | hit (A's entries appended at L1's tail) | miss | miss |
| Across rounds (entries added) | hit | hit up to the append point | depends | miss |

## 4. Budget and packing (single-request prefix L0+L1+L2 ≤ 200k tokens)

The budget is controlled by `AGMEM_PREFIX_BUDGET`, default **200000**
(`cleaner.DefaultPrefixBudgetTokens`, tightened from 800k on 2026-09-10).
Upstream requests with several-hundred-k-token prompts have been observed to
return normally; the reason for tightening to 200k is that a byte-stable prefix
grows with the store and would crowd out the request budget forever — 200k
leaves room for L3 (document body + coverage declaration) and the output, and
by observed wiki-tree scale it still covers the whole ancestor chain plus the
vast majority of the store.

**The budget must be split between layers** (never competing — this is the
lifeline of cache continuity):

| Layer | Quota | Fill |
|---|---|---|
| L0 | constant, deducted first | 2–5k |
| L1 | **3/4** of what remains after L0 | all-store entries **excluding the target directory and its whole ancestor chain**, in production-time order |
| L2 | **1/4** of what remains after L0 | target directory + ancestors (nearest → farthest), **frozen at round start** |

Why split rather than first-come-first-served: L1 is the only carrier of
cross-directory reuse of the L0+L1 segment; its size must be **independent of
the target directory**. If the unbounded ancestor chain (L2) competed for the
same pool, deep directories would eat more budget and shrink L1 → L1 would
change on every directory switch → all cross-directory L0+L1 hits would die.
With the split, L1's quota is constant across directories and L2 has its own:
an L2 overflow only trims L2 from the far end of the ancestor chain and never
compresses L1.

Fill order: **fill L2's quota first** (target directory + each ancestor level,
near → far), **then L1's quota** (excluding the target directory and its whole
ancestor chain, in production-time order; over quota **trim from the tail
only** — cut the newest, keep the stable old part as the cache base; trimmed
entries re-enter next round, still append-only semantics). L2 stops when its
own quota is full (equivalent to trimming the far end of the ancestor chain).
Entries that don't fit wait for the next round — never dropped, never truncated
mid-entry.

`Prefix` exposes `L1Tokens` / `L2Tokens` / `DirDepth` (how many directory
levels L2 actually packed) for observability; `L1Tokens+L2Tokens == EstTokens`;
the cleaning log prints `l1=<n> l2=<n> depth=<n>`; `/metrics` exposes the rest
(§7).

## 5. Coverage declaration (avoiding cross-document re-extraction)

L2 groups entries by **document name** and states explicitly "do **not**
extract entries from these documents"; L3 then declares the single target
document of this request. Without both, the model re-extracts whatever it can
see in the background.

### 5.1 Coverage identity uses document names, not URLs (recorded deviation)

The draft design said "coverage URL list"; the implementation uses **document
names** (`documents.source_name`) as the stable identity:

- The store holds no wiki URLs. Building reliable links needs `node_token`,
  but `internal/wikisync`'s state table stores `obj_token` (the `doc_token`
  column), which cannot be turned into a stable clickable wiki link; this
  batch does not change wikisync for that.
- Code: `KnowledgeEntry.DocName`, `Prefix.CoveredDocs` / `DocCounts`, L3 text
  「【本前缀覆盖的文档】」/「【本次提取目标文档】」 (see `internal/cleaner/prefix.go`
  header, `glm.go`'s `PrefixUserBlock`).
- If `documents` ever gains a `node_token` column, switch the identity back to
  URLs in one place.

## 6. Only changed documents are cleaned (verified)

The cleaning side **does not rescan the store or re-clean unchanged
documents**: each round the worker claims one queued item
(`internal/cleaner/worker.go` `processEmbedded`), and queue items only appear
for new/changed documents. Change detection lives in `internal/wikisync` — a
new version is enqueued only when `wiki_sync_state.obj_edit_time` grows;
unchanged documents take the `default` branch and are skipped ("zero cost,
zero external calls"). Manual push (`POST /docs`) and the migration tool
(`cmd/agmem-migrate`) are the only other enqueue sources, both explicitly
triggered. There is **no** "rescan and re-clean everything" path.

## 7. Observability and acceptance

Cleaning requests must record and expose via `/metrics`:

- `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` (returned by the
  gateway, confirmed observed)
- `reasoning_tokens` (thinking mode is on by default upstream and billed as
  output; watch the share)
- per-round prefix hash / version / size / layering
  (`agmem_clean_prefix_l1_tokens` / `agmem_clean_prefix_l2_tokens` /
  `agmem_clean_prefix_dir_depth`)

Acceptance criteria: **steady-state within-round hit rate > 90%**; per-request
prefix ≤ 200k; the eval baseline does not regress; cross-document duplicate
entries decrease.

## 8. Relation to other components

- Entry **provenance and dedup** remain owned by `internal/store` + Qdrant;
  this design only changes "the context fed to the model".
- `internal/wikisync` delivers changed documents into the pipeline and is
  orthogonal ("only changed documents are cleaned", §6).
- The prefix's **version and hash** are written into entry payloads for
  provenance ("which background version extracted this batch").
- Switch: `AGMEM_PREFIX` (default on; literal `off`/`false`/`0` disables it —
  a rollback valve). When disabled or when no `PrefixSource` is injected, the
  worker logs "prefix not enabled" exactly once — it never degrades silently.
