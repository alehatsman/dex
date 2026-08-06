# Design — #121 Content-addressed vector cache surviving reindex (L2)

Status: **spec / to implement** · Epic #121 · subsumes the reindex half of #98,
extends the #77 §2 / #91 line of work.

## Goal

Make a **full `dex reindex` nearly free of embedding cost** when file content is
unchanged. Today `reindex` drops the whole index DB and re-embeds every chunk +
all ~35k graph-node signatures from scratch — even when the content is
byte-identical to the index it just dropped. Embedding is the only CPU/GPU-heavy
step (the walk/chunk/SHA/sqlite work is cheap, per #77), so a reindex-on-upgrade
is pure wasted compute and the source of the "Mac runs hot on reindex" pain.

Out of scope: the incremental edit path (already lean — mtime fast-path + SHA
dedup + fsnotify watch); embed-model/backend choice (#77); changing NodeKNN
fusion semantics (#98 Tier B, still eval-gated).

## Why reindex re-embeds identical content

`reindexOne` (`cmd/dex/main_manage.go:187`) builds a fresh temp DB from source,
swaps it in, then `clearCacheKeepLock` sweeps the old cache dir. It rescues the
knowledge store (notes) but **not a single vector**. The schema has "no
ALTER/backfill; an older index fails the version gate and is rebuilt from
source" (`store_migrate.go`), so *every* dex-version bump that touches the schema
triggers this full re-embed. The vectors are the expensive artifact and they are
thrown away and recomputed identically.

## Design

A second-level, content-addressed **vector cache** that survives the reindex
drop. Same shape as LangChain `CacheBackedEmbeddings` / LlamaIndex
`IngestionCache`: a decorator over the embedder backed by a key→vector store.

### Pillar A — `CachingEmbedder` decorator (`internal/embed/caching.go`)

Wraps an `Embedder`, sits **outside** `dimCapEmbedder` so it caches final
truncated+L2-normalized vectors. Per `Embed(inputs)`:

1. Compute `key(text)` for each input.
2. Batch-lookup the store; split inputs into hits and misses.
3. `inner.Embed(misses)` — the live embedder (its internal concurrency fan-out
   is below this wrapper, unaffected).
4. Best-effort write the miss vectors back to the store.
5. Reassemble outputs in input order.

Pass-through `Health/Endpoint/ModelName/BatchSize/EmbedConcurrency` to `inner`
(exactly like `dimCapEmbedder`). `WithCache(inner, nil)` returns `inner`
unchanged (feature-off / open-failure degradation).

The embed package stays sqlite-free: the decorator depends on a tiny interface

```go
// internal/embed/caching.go
type VecStore interface {
    Get(ctx context.Context, keys []string) (map[string][]float32, error)
    Put(ctx context.Context, entries map[string][]float32) error
}
```

implemented in `internal/veccache` over sqlite. Store errors are swallowed
(best-effort): a `Get` error → treat all as miss (embed everything); a `Put`
error → ignore. **The cache can never fail or slow indexing into a wrong
result** — on any doubt it degrades to a live embed.

### Pillar B — the cache key (the anti-corruption guard)

```
key = hex(sha256( modelTag  ⧺ 0x00 ⧺ formatVersion ⧺ 0x00 ⧺ literalInputText ))
```

- `modelTag = inner.ModelName()` — the `model@<dim>` identity that
  `EnsureEmbedModel` already guards. A model or dim-cap swap changes the tag →
  total cache miss → correct re-embed. This is the composite-key discipline every
  source stressed: **naive content-only keys silently serve wrong vectors after a
  model/tokenizer/normalization change.**
- `literalInputText` — keying on the *exact string handed to `Embed`*
  transparently absorbs chunking, the `// context:` prefix
  (`EmbedTextWithContext`), and node signatures (`nodeEmbedText`). Any change to
  how text is composed changes the string → miss. No need to enumerate those axes.
- `formatVersion` — a package const bumped if the vec blob encoding or key
  recipe changes; a bump invalidates the whole cache cleanly.

Trust boundary (documented, inherited from the index itself): if an operator
swaps model *weights* while keeping the same name/tag, both the index and this
cache go stale together — dex already treats model identity as the name, so the
cache adds no new risk.

### Pillar C — surviving reindex (`internal/veccache` + `clearCacheKeepLock`)

- Sidecar sqlite at `filepath.Join(p.CacheDir, "veccache.db")`, opened WAL +
  `_busy_timeout`. Schema:

  ```sql
  CREATE TABLE IF NOT EXISTS vec_cache (
      key        TEXT PRIMARY KEY,
      dim        INTEGER NOT NULL,
      vec        BLOB    NOT NULL,   -- little-endian float32, matches store.encodeVec
      created_at INTEGER NOT NULL
  );
  ```

- `clearCacheKeepLock` (`cmd/dex/main_lock.go:86`) gains a keep-rule for the
  `veccache.db` prefix (covers `-wal`/`-shm`) so the reindex sweep never removes
  it. Because it is in the keep-list it is never `RemoveAll`'d, so there is no
  open-handle race with a live cache connection.

- **Bounded**: `DEX_VEC_CACHE_MAX` rows (default 500_000; `0` = unbounded).
  Pruned oldest-`created_at`-first on open when over cap. No read-side writes
  (no LRU touch) — the working set of a reindex is "current index contents,"
  bounded by repo size; historical versions age out by insertion order. Cheap
  and non-amplifying.

### Wiring — cache the INDEXING embedder only, never queries

Query embeds are one-off NL strings: low hit rate, unbounded growth, no benefit.
The cache wraps only the embedder handed to `index.New` and `EmbedNodes`:

- CLI `dex index` / `dex reindex`: wrap at the embed-client construction site
  (`cmd/dex/main_clients.go` region) used by the index path.
- MCP `runWatcher` (`internal/mcp/server.go:653`): wrap `s.EmbedClient` before
  `index.New(...)` and the `AfterIndex` `EmbedNodes(...)`; the query path keeps
  the raw `s.EmbedClient`.

Both chunk pass and node pass go through the same wrapper, so node signatures are
cached too — that is how L2 kills the reindex 35k without touching fusion.

## Relationship to #98

#98 (Tier B) proposes deriving node neighbors from the enclosing chunk vector to
delete the node embed pass outright — this **changes NodeKNN semantics** and stays
behind a retrieval eval. L2 instead **memoizes the node's own vector**, so:

- Reindex node cost → ~0 (cache hits), retrieval **identical by construction**,
  no eval needed.
- Only the *first-ever cold index* still pays the 35k node pass (+ all chunks).
  Eliminating *that* is the remaining, separately-eval-gated #98 Tier B.

So #121 delivers the safe, larger, reindex-wide win now; #98 narrows to the
cold-index case and can be judged on its own merits later.

## Edge cases

| Case | Behavior |
|---|---|
| First-ever index (cold cache) | All misses → embed everything, populate cache. Same cost as today, +1 cheap sqlite write pass. |
| Reindex, content unchanged | ~100% hits → ~0 embeds. The win. |
| Reindex after model/dim swap | `modelTag` changes → total miss → full correct re-embed. Old-tag rows age out by cap. |
| Incremental edit (watch) | Changed file's chunks/nodes are new text → miss → embed (unchanged from today). No regression. |
| Cache DB open fails / corrupt | `WithCache(inner, nil)` path — indexing proceeds on the live embedder. |
| `Get`/`Put` error mid-run | Get error → all-miss; Put error → ignored. Never fails the index. |
| Interrupted reindex resumes | Vectors written per super-batch before the crash are cache hits on resume — resume is cheaper, not broken. |
| Two indexers on one project | Prevented upstream by the project lock; sql.DB + WAL + busy_timeout tolerate it regardless. |

## Validation

Retrieval quality is unchanged **by construction** — the cache returns the exact
vectors the embedder produced. Tests:

1. `caching_test.go`: hit returns the byte-identical vector a miss produced;
   output order preserved; misses interleaved with hits reassemble correctly.
2. Model-tag change → full miss (new tag ⇒ new keys).
3. `inner.Embed` call-count: second `Embed` over the same inputs makes **zero**
   inner calls (a fake counting embedder).
4. `veccache` store: Put→Get round-trip; prune drops oldest over cap; open on a
   junk file degrades to nil (best-effort), not a panic.
5. Integration: index a temp repo twice; assert the 2nd run's embedder call count
   is ~0 while chunk/node rows are identical.
6. `clearCacheKeepLock` leaves `veccache.db*` in place (unit over a temp dir).

Gate: `mooncake task ci`.

## Follow-ups (not in #121)

- #98 Tier B cold-index node elimination (eval-gated) — now scoped to cold index
  only.
- Optional `dex reindex --no-vec-cache` / `--fresh` escape hatch for a paranoid
  from-absolute-scratch rebuild (default reindex uses the cache — that is the
  point).
- CC `PostToolUse` change-notify hook — orthogonal (latency/worktree routing, not
  embed heat); separate issue.
</content>
</invoke>
