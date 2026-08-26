---
id: storage
status: living
last_verified: c4b4bdc
owners: [aleh]
covers:
  - "internal/store/**"
---
# Storage

> **Note (#205, 2026-08-26):** dex's `record`/`notes` write verb and the whole knowledge subsystem (`knowledge_facts`/`knowledge_relations`/`scoped_notes`) were removed — the MCP surface is a single verb, `query`. Any mention of `record`/`notes`/`knowledge`/`remember` below is **historical**.


## Intent

Storage is dex's local index engine: the on-disk format the indexer writes and
every query surface reads. One SQLite file per project holds chunks, their
embedding vectors, and the full-text rows that back lexical search, plus the
structural graph. The store exists to make a
repo's semantic index durable, self-describing, and concurrently safe to read
while it is being rebuilt — so an interactive `ask`, a background `watch`
re-index, and a manual `dex index` can all touch the same project without
corrupting it. The engine's load-bearing constraints (its required build
features, its fixed-per-index embedding dimension, and the destructiveness of a
reindex) live here because getting them wrong silently destroys an index rather
than failing loudly. This spec covers the format and its access methods;
producing the data is the indexing spec's, and interpreting query results is the
query specs'.

## Behavior

- WHERE a project's index lives, the store is a single SQLite file under a
  per-project directory keyed by a hash of the project's real path
  (`DEX_INDEX_DIR`, default `~/.cache/dex`), so distinct repos never share an
  index and the same repo always resolves to the same file.
- WHEN the store is opened, it records and recovers the index's self-describing
  metadata — embedding dimension, embedding-model identity, project root, and
  last-indexed time — so later runs can validate compatibility and
  locate the source tree.
- WHEN the store engine lacks SQLite full-text (FTS5) support, opening or
  migrating an index fails loudly instead of degrading, because the lexical
  search leg depends on it and a silent fallback would hide a broken build.
- WHEN dex is built, it MUST be built with FTS5 enabled (the `sqlite_fts5` build
  tag on the CGO SQLite driver); a binary built without it cannot open an index,
  and because a reindex drops the index before rebuilding, running such a binary
  against an existing project destroys its index.
- WHEN a batch of chunks is written, the store persists them transactionally and
  idempotently keyed on (path, content), so re-indexing unchanged content does
  not duplicate rows or multiply on-disk cost.
- WHILE chunks are written, deleted, or updated, the full-text and vector
  indexes are kept in sync automatically, so a writer never has to maintain the
  derived indexes by hand and they can never drift from the canonical chunk
  rows.
- WHERE the embedding vector is concerned, the canonical copy is stored alongside
  the chunk so the vector index is always rebuildable from the chunk table, and
  an index whose vector table pre-dates the current engine is backfilled once on
  open.
- WHEN the embedding dimension is first established for an index, it is fixed for
  that index's lifetime; a later write whose vector dimension differs is
  rejected rather than silently mixed, so changing the embedding shape requires a
  reindex.
- WHEN the active embedding model differs from the one recorded for the index,
  the store rejects the run with a reindex hint, because two same-dimension
  models occupy different latent spaces and mixing them corrupts retrieval.
- WHEN concurrent writers contend for the index, each waits a bounded time for
  the write lock (WAL journaling, bounded busy-timeout) rather than failing
  immediately, so a `watch` re-index and a manual `dex index` can overlap.
- WHERE a single process holds the store, access is pinned to **one connection**
  (`SetMaxOpenConns(1)`, mirroring `veccache` and the #95 §6.2 one-store/one-reader
  decision), so intra-process reads and writes serialize through that connection.
  This closes the `SQLITE_BUSY_SNAPSHOT` gap (#185): the DSN `_busy_timeout` retries
  a plain `SQLITE_BUSY` but *not* a snapshot that a second pool connection upgrades
  read→write, which deadlocks non-retriably. One connection cannot hold a stale
  snapshot against itself, so the failure mode is structurally impossible rather
  than merely bounded. Safe because every write transaction is self-contained
  (uses only its `tx` handle, never re-enters the pool), so the single connection
  is never awaited by its own holder. Cross-process contention is unchanged — each
  process owns its own connection and still relies on WAL + `_busy_timeout`.
- WHEN a re-index completes, the store prunes chunks not seen in that pass, so
  vanished files leave no stale rows.
- WHEN search reads the index, the store serves both a vector-similarity leg and
  a lexical (FTS5/BM25) leg and fuses them, and a lexical-query parse error
  degrades to the vector-only result rather than surfacing as a search failure.
- WHEN a search reorders results with a reranker, the store memoizes rerank
  results in a bounded in-memory cache keyed on the query and candidate set, so
  repeated queries in a session don't repay the cross-encoder cost.
- WHEN `knowledge action=add` stores a fact whose key (archetype + normalized
  body) already exists, the store increments `revision_count` on the row rather
  than inserting a duplicate, so callers can distinguish a new fact ("Remembered.")
  from a repeated confirmation ("Confirmed (revision N).").
- WHEN `dex summarize` writes a per-file prose summary, it stores it in
  `file_summaries` keyed by path + `body_hash` (SHA-256 of the file's content),
  so a re-summarize of an unchanged file is a no-op; when the file changes the
  stale row is replaced rather than accumulated, so the table never holds
  multiple summaries for the same path.
- WHEN the on-disk `schemaVersion` differs from the binary's expected version,
  the store rejects the open with `ErrSchemaVersionMismatch` and a reindex
  hint — the index is never silently read with a mismatched schema.

## Non-goals

- **Producing what is stored.** Walking the repo, chunking, and computing
  embeddings belong to the **indexing** spec; storage begins at "persist this
  batch."
- **Interpreting reads.** Ranking semantics, symbol resolution, and graph
  traversal are owned by **semantic-search**, **symbol-search**, and **graph**;
  storage exposes the primitives, those specs define the query contracts.
- **The embedding model.** Which model serves vectors, its dimensions, and how it
  is run are the **embedding** spec's; storage only records the model identity
  and enforces dimension/model consistency.
- **Index orchestration & lifecycle commands.** Reindex/snapshot CLI flows and
  the per-project lock are driven from the indexing layer; storage provides the
  destructive-rebuild primitive but does not own when it runs.

## Checklist

- [x] One SQLite file per project under `DEX_INDEX_DIR/<hash(realpath)>/index.db`,
  with self-describing meta (dim, embed model, project root, timestamps).
- [x] Chunk writes are transactional and idempotent on (path, content); FTS5 and
  vector indexes stay in sync via triggers; the canonical vector lives on the
  chunk row.
- [x] Embedding dimension is fixed per index and model identity is enforced;
  mismatches are rejected with a reindex hint.
- [x] Build MUST use `-tags sqlite_fts5`; an FTS5-less binary fails on
  `migrate: no such module: fts5` and a reindex from such a binary wipes the
  index. (Enforced in `tasks.yml`/`Dockerfile`.)
- [x] Concurrent writers are bounded by WAL + busy-timeout; prune removes stale
  chunks. Intra-process access is pinned to one connection (`SetMaxOpenConns(1)`),
  making `SQLITE_BUSY_SNAPSHOT` structurally impossible (#185).
- [x] Hybrid read path (vector + BM25 fused) with graceful FTS-parse fallback and
  a bounded rerank cache.
- [x] `knowledge_facts.revision_count` incremented on ON CONFLICT UPDATE; migrated
  on existing DBs via guarded `ALTER TABLE` + meta flag.
- [x] Verified against the code by the verify workflow (flip to `living`)
