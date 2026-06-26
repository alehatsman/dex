---
id: indexing
status: living
last_verified: 621894f
owners: [aleh]
covers:
  - "internal/index/**"
  - "internal/chunk/**"
  - "internal/embed/**"
---
# Indexing

## Intent

Indexing is dex's foundation: it turns a repository into a semantic index that
every query surface (semantic search, symbol search, graph, ask) reads from. A
repo is walked, each eligible file is split into retrieval-sized chunks, each
chunk is embedded into a vector, and the vectors plus their metadata are stored
locally. The pipeline is built to run unattended on a developer box — indexing
is opt-in per repo so dex never embeds a tree the owner didn't ask it to, and a
degraded embedding backend fails soft so a consumer can fall back to grep rather
than break. This spec covers producing the index; the on-disk format is the
storage spec's, and reading it is the query specs'.

## Behavior

- WHEN a repo is indexed, dex walks it, selects eligible files (ignore rules plus
  an indexable-extension/-basename allowlist, skipping binaries), splits each into
  chunks, embeds them, and stores the vectors with their file/location metadata.
- WHERE indexing is opt-in, a repo is only indexed when it declares an include
  set (`.dex/config.yml` `index.include`); without one, the matcher selects no
  files and the index stays empty, so dex never embeds an un-opted-in tree.
- WHEN a file is chunked, dex extracts top-level declarations (functions, methods,
  classes, types) using a tree-sitter grammar where one exists, and falls back to
  a fixed-line sliding window with overlap for unknown languages or parse
  failures; any chunk larger than the byte cap is split to fit.
- WHEN chunks are embedded, dex sends them in batches to an OpenAI-compatible
  `/v1/embeddings` endpoint and stores the returned float32 vectors.
- IF the embedding backend is unreachable at index time, indexing surfaces a distinct
  unreachable condition rather than silently producing an empty or partial index,
  so a consumer can degrade (e.g. fall back to grep) instead of trusting a broken index.
- WHEN the embedding backend is unreachable at query time (search, ask), dex degrades
  to BM25-only — the semantic lane is skipped, symbol and graph lanes run, and the
  caller gets real results rather than an error; the response carries
  `status:"embedding-service-unreachable"` with a hint to start the endpoint.
- WHEN `DEX_EMBED_URL` and `DEX_CHAT_URL` are both unset and ollama is installed but
  not listening on its default port, dex attempts a best-effort `ollama serve` (detached,
  polled, bounded); this auto-start runs once per process and can be disabled with
  `DEX_NO_AUTO_OLLAMA=1`.
- WHEN a repo is reindexed, the existing index is dropped and rebuilt from
  scratch — reindex is destructive by design, so it must only run from a binary
  whose storage support is intact, never one that would rebuild into a crippled
  index.
- WHEN a repo is updated incrementally, only changed files are re-chunked and
  re-embedded, so keeping an index current does not require a full rebuild.
- WHILE an index operation runs, it holds a per-project lock so two indexers don't
  corrupt the same index concurrently.

## Non-goals

- **The index format & storage engine.** How vectors, chunks, and FTS rows are
  stored and the build-tag requirements of that engine are the storage spec; this
  spec stops at "store the vectors."
- **Querying the index.** Semantic search, symbol lookup, graph, and ask read the
  index and are specified separately.
- **The embedding model itself.** Which model/provider serves `/v1/embeddings`,
  its dimensions, and how it's run are the embedding spec / operator config; here
  only that chunks are sent in batches and vectors come back.
- **The watch daemon.** Triggering reindex on file changes is the watch spec;
  this spec is the pipeline it invokes.
- **Ignore-rule semantics.** The precise ignore/allowlist/opt-in matching is the
  ignore spec; here only that indexing applies it.

## Checklist

- [x] Walk + filter (ignore, indexable ext/basename, skip binary)
- [x] Opt-in via `.dex/config.yml` `index.include`; no include → empty index
- [x] Tree-sitter top-level-decl chunking; sliding-window fallback; byte cap
- [x] Batched embedding to an OpenAI-compatible `/v1/embeddings`
- [x] Unreachable backend at index time surfaces a distinct condition (not a silent empty index)
- [x] Query-time embed unreachable degrades to BM25-only; `status:"embedding-service-unreachable"` returned
- [x] Ollama auto-start: when `DEX_EMBED_URL`/`DEX_CHAT_URL` unset and ollama installed but down, best-effort `ollama serve`; opt-out via `DEX_NO_AUTO_OLLAMA=1`
- [x] Reindex drops + rebuilds; incremental updates only changed files
- [x] Per-project index lock against concurrent indexers
- [x] Verified against the code by the verify workflow (flip to `living`)
