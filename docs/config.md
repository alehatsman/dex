# Configuration

Two layers: a per-project `.dex/config.yml` file, and `DEX_*` environment
variables that override it. `dex env` prints the effective configuration with
its source; `dex config init` scaffolds a commented file.

## `.dex/config.yml`

Lives at the project root. The most important section is `index:`, which is
**opt-in**: nothing is indexed unless `include` matches it. `ignore` composes on
top of `.gitignore` / `.dexignore`. Patterns use gitignore grammar.

```yaml
index:
  include:
    - cmd/
    - internal/
    - "*.md"
  ignore:
    - testdata/
    - "**/*_generated.go"
```

Endpoint/model/tool settings can also live here; env vars take precedence.

## Environment variables

Grouped by role (all optional; defaults shown where notable).

**Index & storage**
- `DEX_INDEX_DIR` — index location (default `~/.dex`)
- `DEX_INDEX_CONCURRENCY` — chunk-pass worker count
- `DEX_PROFILE` — named profile (e.g. weak-model anchor floor)

**Embeddings**
- `DEX_EMBED_URL` (default `http://localhost:11434`), `DEX_EMBED_MODEL`, `DEX_EMBED_DIM`
- `DEX_EMBED_ENGINE` — `ollama`/openai-compatible (default) · `none` (lean, BM25-only) · `onnx` (in-process)
- `DEX_EMBED_BATCH`, `DEX_EMBED_CONCURRENCY`, `DEX_EMBED_TIMEOUT`
- `DEX_NO_AUTO_OLLAMA` — disable local ollama auto-detection

**Chat & rerank**
- `DEX_CHAT_URL`, `DEX_CHAT_MODEL`, `DEX_CHAT_TIMEOUT`
- `DEX_RERANK_URL`, `DEX_RERANK_MODEL`, `DEX_RERANK_STYLE`, `DEX_DISABLE_RERANK`

**Retrieval tuning**
- `DEX_FUSION_MODE` (`linear` default · `rrf`), `DEX_FUSION_ALPHA` (dense weight, default `0.7`)
- `DEX_GRAPH_WEIGHT`, `DEX_GRAPH_GAMMA`, `DEX_GRAPH_HOP_CAP` — graph-lane influence
- `DEX_DISABLE_BM`, `DEX_MAX_HITS_PER_FILE`
- `DEX_EXPAND_MODE` / `DEX_EXPAND_URL` / `DEX_EXPAND_MODEL` — query expansion (e.g. HyDE)

**Tool surface**
- `DEX_EXPERT=1` — expose the graph/analysis power lane over MCP
- `DEX_DESCRIPTION_MODE` — tool-description verbosity (`full`|`terse`|`lazy`; default `terse`). Compact descriptions cut the per-turn token cost of the tools array; set `full` to opt out. Forced to `full` when `ENABLE_TOOL_SEARCH` is active (tool-search needs full docs to pick tools).
- `DEX_MCP_AUTOWATCH`, `DEX_WATCH_DEBOUNCE` — lazy per-session file watcher

**`dex serve` (HTTP)**
- `DEX_SERVE_TOKEN` — bearer token (required for non-loopback binds)
- `DEX_SERVE_WATCH` — eager per-project watchers
- `DEX_ALLOW_PATHS` — `shell` tool allow-list
- `DEX_SHELL_ALLOW_WRITES=1` — let `shell` commands use file-write redirects (`>`, `>>`, `tee`, heredoc-to-file); blocked by default so agents reach for the Write tool

**ONNX** (only with `-tags onnx`): `DEX_ONNXRUNTIME_LIB`, `DEX_ONNX_MODEL`,
`DEX_ONNX_TOKENIZER`, `DEX_ONNX_DIM`, `DEX_ONNX_MODEL_ID`, `DEX_ONNX_MAX_SEQ`, …

Run `dex env --doc` for the full list with inline descriptions, and `dex doctor`
to validate endpoints, the index directory, and MCP wiring end-to-end.
