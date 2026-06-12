# dex reading order

Start with what matters for your goal. Each entry is one doc or file — read
the ones marked **required** for your path, skim the rest on demand.

---

## Path 1 — New to the codebase

Goal: understand what dex is, how it's structured, and where to find things.

| Step | Read | What you get |
|------|------|--------------|
| 1 | [`README.md`](../README.md) | Installation, quick-start, tool surface |
| 2 | [`docs/vision.md`](vision.md) | The capability ladder and *why* things are the way they are |
| 3 | `internal/store/store.go` — top comment | The SQLite schema: chunks, vec0, FTS5, symbols, graph edges |
| 4 | `internal/index/index.go` — `Run()` | Indexer pipeline (walk → chunk → embed → upsert → prune) |
| 5 | `internal/mcp/server.go` — tool registrations | Which MCP tools exist and what they call |
| 6 | [`CONTRIBUTING.md`](../CONTRIBUTING.md) | Workflow, build commands, conventions |

After these six you can navigate the codebase with `dex ask` and land in the
right file on the first try.

---

## Path 2 — Fixing a bug

Goal: reproduce, locate, and fix a specific failure.

| Step | Read | What you get |
|------|------|--------------|
| 1 | [`docs/observability.md`](observability.md) | Log fields and jq recipes to isolate the failure |
| 2 | Run `dex doctor` | Health check — catches misconfigured endpoints, stale indexes, MCP wiring gaps |
| 3 | `dex ask "<symptom>"` | Locates the relevant code in one hop |
| 4 | Relevant package | Read only the files the symptom touches |
| 5 | `mooncake task test` | Reproduce via existing tests or write a new one |

---

## Path 3 — Designing or implementing a feature

Goal: understand where a new feature fits and avoid re-inventing what exists.

| Step | Read | What you get |
|------|------|--------------|
| 1 | [`docs/vision.md`](vision.md) | Which rung does this feature serve? |
| 2 | [`docs/agent-roadmap.md`](agent-roadmap.md) | Open backlog — is it already planned or in progress? |
| 3 | `mgit issue list --state todo,in_progress` | Live queue — check before creating a duplicate |
| 4 | `dex ask "<feature area>"` | Existing code in the area |
| 5 | Relevant `docs/*.md` below | Subsystem-specific constraints |

**Subsystem guides** (read the ones relevant to your feature):

| Subsystem | Doc |
|-----------|-----|
| Retrieval quality | [`docs/retrieval-eval.md`](retrieval-eval.md) — eval harness, golden set, NDCG/Recall/MRR |
| Compression | [`docs/compress-bench.md`](compress-bench.md) — what the compress ruler measures |
| Perf / latency | [`docs/perf-bench.md`](perf-bench.md) — what the perf ruler measures |
| Model selection | [`docs/model-selection.md`](model-selection.md) — embed/chat/reranker tradeoffs |
| No-GPU / lean | [`docs/lean-profile.md`](lean-profile.md) — BM25-only, ONNX, DEX_EMBED_ENGINE |
| Logging | [`docs/observability.md`](observability.md) — canonical slog keys, jq recipes |

---

## Path 4 — Operating dex (deploying or tuning)

Goal: run dex in production, understand config, and monitor it.

| Step | Read | What you get |
|------|------|--------------|
| 1 | [`README.md`](../README.md) — *Configuration* section | `.dex/config.yml` fields |
| 2 | [`docs/model-selection.md`](model-selection.md) | Which models to wire for your hardware |
| 3 | [`docs/lean-profile.md`](lean-profile.md) | CPU-only / no-GPU deployment modes |
| 4 | [`docs/observability.md`](observability.md) | What to monitor and how |
| 5 | `dex doctor` | Verify every endpoint and config is reachable |

---

## Package map (one-line summaries)

| Package | Role |
|---------|------|
| `cmd/dex` | CLI entry point — all subcommands wired here |
| `internal/index` | Indexer pipeline, watcher, progress |
| `internal/store` | SQLite store — FTS5, vec0, symbols, graph, knowledge |
| `internal/mcp` | MCP server — tool handlers, compress, context assembly |
| `internal/chunk` | Language-aware AST chunker (Go, TS, Python, Rust, …) |
| `internal/embed` | Embedder interface, HTTP client, ONNX engine (opt-in) |
| `internal/graph` | Call-graph extraction and BFS traversal |
| `internal/eval` | Offline retrieval eval harness (golden set, NDCG, MRR) |
| `internal/compress` | Compress passes: entropy, n-gram codebook, delta, dedup |
| `internal/logx` | Canonical slog attribute helpers |
| `internal/proj` | Project root detection, cache path layout |
| `internal/ignore` | `.gitignore` + `.dexignore` matcher |
| `internal/proxy` | ANTHROPIC_BASE_URL proxy — history pruning, tool compression |
| `internal/knowledge` | Knowledge store — semantic recall with decay |
| `internal/corpus` | Multi-repo eval corpus (pinned repos, per-cell scoring) |
