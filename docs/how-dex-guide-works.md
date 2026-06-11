# How LLM_GUIDE.md is generated

Two-phase model. Phase 1 (summarization) and phase 2 (rendering) are
decoupled — the guide renders from whatever summary chunks already
exist in the store. Zero LLM calls happen at render time.

## Phase 1: summaries in the store

`dex guide` reads two kinds of chunks from the store:

| Kind | What |
|---|---|
| `package_summary` | LLM-generated prose per directory |
| `repo_summary` | single repo-level overview |

These chunks are written by the summarization pipeline. As of #314, the
pipeline is no longer triggered automatically by `dex index`. Chunks
from a previous summarization run persist and continue to feed the
guide; a fresh index with no prior summaries produces a guide with empty
prose sections.

**Cache keys**: each summary is stored with a deterministic SHA over its
inputs. Re-summarizing with no changes → cache hits → no LLM calls.

The chat client is OpenAI-compatible (`internal/chat/client.go`) —
Ollama, vLLM, anything that speaks `/v1/chat/completions`. Configuration
is via `DEX_SUMMARY_URL` / `DEX_SUMMARY_MODEL`.

## Phase 2: dex guide renders

`dex guide .` does **zero LLM calls**. The flow lives in
`internal/guide/render.go`:

1. Load `repo_summary` + `package_summary` chunks via
   `SummariesByKindWithMeta` (path, content, last_seen_at).
2. Read `.dex/llm_guide.manifest.json` — get `last_summary_seen_at`.
3. Dirty check: any summary chunk's `last_seen_at` greater than the
   manifest's recorded value, OR the guide file is missing, OR `--full`
   was passed.
4. If clean → exit. If dirty → format markdown, write `LLM_GUIDE.md`,
   update manifest.

## Output shape

Each module section combines LLM prose with graph-grounded data:

```
## Module: <path>

<package_summary content from LLM>          ← narrative

**Exported API** (N)                        ← from graph_nodes
- `func` Name — file:line
- `method` Type.Name — file:line
...

**Key entry points** (top 5 by PageRank)    ← from graph_nodes.pagerank
- `Name` — file:line — in-degree N
...

**Depends on**                              ← from graph_nodes (kind=import)
- project: internal/foo, internal/bar
- external: context, fmt, github.com/...

**Used by**                                 ← reverse import edges
- cmd/dex, internal/mcp
```

### Section sources

| Section | Source | Filter |
|---|---|---|
| Exported API | `graph_nodes` kind ∈ {function, method, struct, interface, type} | name starts with capital |
| Key entry points | `graph_nodes` kind ∈ {function, method}, ORDER BY pagerank DESC | exported preferred; falls back to internal hot spots (with a visible heading change) when no exported nodes have centrality |
| Depends on | `graph_nodes` kind=import, scoped to the directory's Go package paths | split into project (matches go.mod module prefix) vs external (stdlib + third-party) |
| Used by | inverse of Depends on — packages whose import nodes name this module's package paths | strips module prefix to display directories |

### Quirks

- `file_path` is empty on `kind='import'` rows (imports are a
  package-level fact, not per-file). Queries resolve via `package_path`
  for these rows; only declaration nodes (`function`, `method`, etc.)
  carry `file_path`.
- Non-Go directories (`testdata/`, `scripts/`, `docs/`) get only the
  LLM prose section — graph queries return empty and each subsection
  is omitted gracefully.
- The renderer reads `go.mod` once per render to discover the module
  prefix used to split project vs. external imports.

## Why the split

Separating "produce summaries" from "format guide" gives:

- **Cheap re-renders.** The guide can re-run instantly because formatting
  requires no LLM.
- **Reusable summaries.** The same `package_summary` chunks power `ask`'s
  suggested-reads and MCP context routing — the guide is a new consumer,
  not a new producer.
- **Hallucination resistance.** LLM prose carries the narrative; graph
  data carries the facts. If they disagree, the facts are the source
  of truth and a reader can see both.
