# dex Data Pipeline Architecture

dex is a local semantic code-search service. The pipeline has **three indexers writing to one SQLite per project** (keyed by `sha256(realpath(root))` at `$DEX_INDEX_DIR/<hash>/index.db`).

## Three indexers, one DB

| Indexer | Package | Output |
|---|---|---|
| **Chunk indexer** | `internal/index` | `chunks` (+ `chunk_vecs` vec0, `chunks_fts` FTS5) — the semantic + lexical search corpus |
| **Graph indexer** | `internal/graph` | `graph_nodes`, `graph_edges` — Go static call graph + YAML + markdown doc graph, with PageRank centrality |
| **Watcher** | `internal/watch` | fsnotify → debounce → re-runs the two above |

## Chunk pipeline (`Indexer.Run`, `internal/index/index.go:117`)

Comment at top of the file is literal: *"walk → chunk → embed → upsert"*. Three passes:

1. **Pass 1 — walk + chunk** (`internal/index/index.go:1` header). Single-threaded directory walk; per-file work (read, `ignore.Match`/binary/secret heuristics from `internal/ignore`, tree-sitter parse in `internal/chunk`) runs on `Options.Concurrency` workers (defaults to `GOMAXPROCS`).
   - **Mtime fast-path** — file mtime ≤ last index run → `UPDATE last_seen_at` only.
   - **SHA fast-path** — content unchanged → bump `last_seen_at`, backfill `name`, no embed.
   - **Slow path** — surviving files become `slowFile{rel, data, chunks}` for Pass 2.
2. **Pass 2 — embed + upsert**. Batches go to `internal/embed` (OpenAI-compatible `/v1/embeddings`, e.g. vLLM/TEI Qwen3). Result rows go through `store.UpsertMany`; triggers keep `chunk_vecs` (sqlite-vec) and `chunks_fts` in sync (`docs/internals.md:28-37`).
3. **Pass 3 — prune unseen**. `PruneUnseen` deletes rows whose `last_seen_at < startTime` (files removed since the run started).

## Graph pipeline (`internal/graph/graph.go:254`)

Independent of chunks; runs after them in `cmd/dex/main.go:cmdIndex`. `ExtractGo` (go/types) + `ExtractYAML` + `ExtractMarkdown` (one `document` node per .md/.markdown file + `heading` nodes per ATX heading with `contains` edges; `links`/`wikilinks`/`transcludes` edges between docs, resolved relative-path / vault-basename — Obsidian-style, anchored refs resolve to heading nodes (GitHub-slug or literal), backlinks = reverse direction and roll up section hits; plus `tag` nodes + `tagged` edges mined from `#tag`s) → `linkChunks` joins nodes to their chunk rows → `GraphUpsertNodes`/`GraphUpsertEdges` → `GraphPruneUnseen` → `ComputeCentrality` (PageRank + in/out degree + cross-pkg callers; `calls` edges only — doc edges don't affect rank yet) → `GraphSetCentrality`. Skippable via `--graph=off`; `--graph=only` skips chunk passes. Doc edges surface via `graph_links`/`graph_backlinks` (not `graph_callers`/`callees`, which stay `calls`-only).

## Retrieval (read side)

`store.Search` runs **two rankers in parallel and fuses with RRF** (`docs/internals.md:61-87`):
- **cosine** — `SELECT … FROM chunk_vecs WHERE embedding MATCH :blob AND k=:pool` (sqlite-vec); query is embedded via the same `/v1/embeddings` endpoint.
- **BM25** — `bm25()` against `chunks_fts`.
- final score `Σ 1/(60 + rank)`; optional cross-encoder rerank (`DEX_RERANK_URL`).

`internal/mcp` wraps this for the MCP tool surface. By default the MCP server exposes a **single tool, `ask`** — it composes the lanes above and, when a chat endpoint is configured, synthesizes a grounded prose `answer` (with `path:line` citations) from the evidence bundle (`internal/mcp/answer.go`). The raw lanes (`search_semantic`, `search_symbol`, `graph_*`, `file_view`, `status`) are gated behind `DEX_EXPOSE_RAW_TOOLS=1` (alias: `DEX_TOOLS=power`) for CLI parity / power use. `cmd/dex/main.go` provides the CLI mirrors and the MCP stdio server entrypoint.

## Live updates (`internal/watch/watch.go`)

`Watcher.Run`: fsnotify subscribes to the project tree → events filtered through the same `ignore.Matcher` → debounced (`Options.Debounce`, default 500ms) → dirty set drained by re-invoking `Indexer.Run` → `AfterIndex` hook re-runs the graph phase. Used by `dex watch` (`cmd/dex/main.go:1512`).

## Flow at a glance

```
files ──► walk (ignore) ──► chunk (tree-sitter) ──► embed (HTTP) ──► chunks/chunk_vecs/chunks_fts
files ──► ExtractGo/YAML ──► linkChunks ──► graph_nodes/graph_edges ──► PageRank → centrality

query ──► embed ──► chunk_vecs (cosine)  ┐
query ──► tokens ──► chunks_fts (BM25)   ├─► RRF ──► (opt.) rerank ──► hits ──► (opt.) chat synthesis ──► answer + evidence
```
