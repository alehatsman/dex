# Architecture

dex is a local code-search service: it indexes a repo into one SQLite database
and serves search + graph navigation over MCP and a CLI. There is no server to
operate beyond optional model backends and no cloud component.

## One SQLite per project

Each project's index lives at `$DEX_INDEX_DIR/<sha256(realpath(root))>/index.db`
(default `~/.dex`). The driver is `mattn/go-sqlite3` with the `sqlite_fts5` tag;
`sqlite-vec` (`vec0`) is statically linked and auto-registered, so every
connection has both full-text search and vector KNN. WAL journaling +
`busy_timeout` let `dex index` and `dex watch` run concurrently.

## Two indexers, one watcher

| Indexer | Package | Writes | Contents |
|---------|---------|--------|----------|
| Chunk   | `internal/index` | `chunks`, `chunk_vecs` (vec0), `chunks_fts` (FTS5) | the semantic + lexical corpus |
| Graph   | `internal/graph` | `graph_nodes`, `graph_edges` | Go call graph + YAML + markdown doc graph, with PageRank |
| Watcher | `internal/watch` | — | fsnotify → debounce → re-runs both |

**Chunk pipeline** (`Indexer.Run`): walk → chunk → embed → upsert.
1. *Walk + chunk* — concurrent per-file work: ignore/binary/secret filtering,
   then tree-sitter parsing into chunks. An mtime/SHA fast-path skips unchanged
   files (no re-embed).
2. *Embed + upsert* — chunk batches go to an OpenAI-compatible `/v1/embeddings`
   endpoint; rows are written via one transaction per batch. SQLite triggers
   keep `chunks_fts` and `chunk_vecs` in sync, so callers never touch them.
3. *Prune* — rows whose `last_seen_at` predates the run (deleted files) are dropped.

**Graph pipeline** — Go (`go/types`), YAML, and markdown extractors build nodes
and edges (`calls`, `imports`, doc `links`/`wikilinks`/`tags`), join them to
chunk rows, then compute PageRank + degree centrality over `calls` edges.
`--graph=off` skips it; `--graph=only` skips the chunk passes.

The embedding dimension is fixed for the life of an index — changing the
embedding model requires `dex reindex`.

## Retrieval

`store.Search` runs two rankers in parallel over a candidate pool and fuses them:

- **semantic** — `vec0` cosine KNN, query embedded via the same `/v1/embeddings`.
- **lexical** — BM25 over `chunks_fts` (FTS5 parse errors fall back to semantic-only).

Fusion default is **FusionLinear** (convex combination on min-max-normalized
scores, dense weight α=0.7, calibrated in #317); RRF (`Σ 1/(60+rank)`) is the
alternative. An optional cross-encoder reranker (`DEX_RERANK_URL`) reorders the
top pool; if unreachable it silently falls through. `DEX_EMBED_ENGINE=none` or an
empty query drops the semantic lane (BM25-only).

Symbol lookup is exact-match against `chunks.name`, falling back to `graph_nodes`.
Graph queries (callers/callees/path/impact/clusters) walk `graph_edges`.

## The tool layer

`internal/mcp` wraps retrieval as the tool surface. `ask` leads: it routes a
free-text question across the lanes and, when a chat model is configured,
synthesizes a cited (`path:line`) answer from the evidence. The individual lanes
are also exposed as their own tools. The surface is **capability-derived** — a
tool appears only when its backend is wired (see [tools.md](tools.md)). `cmd/dex`
provides the CLI mirrors and the `dex mcp` (stdio) / `dex serve` (HTTP) entrypoints.

```
files ─► walk ─► chunk ─► embed ─► chunks / chunk_vecs / chunks_fts
files ─► extract ─► link ─► graph_nodes / graph_edges ─► PageRank

query ─► embed ─► vec0 cosine ┐
query ─► tokens ─► BM25 (FTS5) ┼─► fuse ─► (opt.) rerank ─► hits ─► (opt.) chat ─► answer
                    graph/symbol┘
```
