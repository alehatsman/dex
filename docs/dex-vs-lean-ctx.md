# dex vs lean-ctx — Storage, Usability, and GPU Roadmap

Brainstorm / design note. Not a committed plan.

---

## 1. Storage Comparison

### lean-ctx (Rust)

Flat files, per-project, scattered across `~/.config/lean-ctx/`.

| Data | Format | Path |
|------|--------|------|
| Knowledge facts | JSON | `knowledge/{project_hash}/knowledge.json` |
| Sessions | JSON | `sessions/{session_id}.json` |
| Latest session pointer | JSON | `sessions/latest.json` |
| BM25 index | Binary + zstd | `vectors/{namespace_hash}/bm25_index.bin.zst` |
| Embeddings | Optional Qdrant or in-memory | external or `vectors/` |
| File read cache | In-memory (zstd) | Session-aware etag cache (`readCache`: sessionID→path→hash); `status=unchanged` on re-reads |
| TF-IDF semantic cache | In-memory | — |
| Config | TOML | `config.toml` |
| Stats | JSON | `stats.json` |

**Inference:** ONNX model runs in-process (`rten` runtime). Default model: `all-MiniLM-L6-v2`. No external endpoint needed. Optional Qdrant for persistent vectors.

**Project identity:** MD5 of normalized project root path. Branch-aware variant appends `|branch:{name}`.

**Locking:** In-process mutex + cross-process file lock (`.knowledge.lock`). Atomic writes via temp-and-rename.

**Key design choice:** Storage is append-friendly JSON blobs. No schema migrations. Cheap to write but no relational queries.

---

### dex (Go)

One SQLite database per project at `$DEX_INDEX_DIR/<sha256(root)>/index.db`.

| Table | Type | Purpose |
|-------|------|---------|
| `chunks` | Regular | Code chunks, vector as packed float32 BLOB |
| `chunks_fts` | FTS5 virtual | BM25 full-text search |
| `chunk_vecs` | sqlite-vec virtual | KNN cosine index |
| `graph_nodes` | Regular | Call-graph nodes (functions, types, structs) |
| `graph_edges` | Regular | Call/import/doc edges |
| `pending_summaries` | Regular | Deferred LLM summarization queue |
| `meta` | Regular | Dimension, model name, timestamps |

**Inference:** External OpenAI-compatible endpoint (vLLM, TEI, ollama, llama-server). No bundled model.

**Project identity:** SHA-256 of canonical root path.

**Key design choice:** SQLite with sqlite-vec gives relational queries, atomic transactions, and KNN in one file. The tradeoff is embedding model changes require a full reindex (`dex reindex`).

---

### Core difference

lean-ctx owns its embedding model in-process (always available, zero-config).  
dex delegates to an external endpoint (higher quality potential, but requires setup).

Both use per-project hashed namespaces. lean-ctx splits storage across multiple flat files; dex consolidates into one SQLite file with relational power.

---

## 2. What Makes lean-ctx So Usable

Not features — **it removes friction at every step**.

### Always works, no setup
ONNX model ships with the binary. No `EMBED_ENDPOINT` to configure, no service to keep running, no version mismatch. Open the tool → it works.

### Zero-config session context
lean-ctx knows what files you touched, what task you declared, what decisions you made — all persisted across invocations without you managing any state. An agent resuming work gets all that context for ~13 tokens.

### Knowledge that accumulates
Facts, gotchas, architecture patterns, preferences — stored and ranked by confidence + recency. Salience scoring (Architecture=15, Gotcha=14) surfaces what matters. Agents don't re-discover the same things twice.

### 10 read modes
`full`, `signatures`, `map`, `diff`, `task`, `reference`, `aggressive`, `entropy`, `lines:N-M`, `auto`. The model picks what to deliver — not the caller. Heavy compression by default; escalate only when needed.

### 67 tools that compose
Every operation has a named tool. Agents don't have to build workarounds; they call `ctx_outline`, `ctx_routes`, `ctx_delta`, `ctx_dedup`, `ctx_impact`, etc. directly. Narrow surface = predictable behavior.

### Shell compression
56+ patterns intercept raw CLI output (git, cargo, npm, docker, kubectl…) and compress 60–99% before reaching the LLM. This is a force multiplier: it works passively on every shell command without the agent doing anything.

### Token awareness
lean-ctx tracks tokens saved, cost, cache hits per session. Agents can introspect with `ctx_metrics`. Users see the value.

---

## 3. What dex Has That lean-ctx Doesn't

- **Better graph analysis:** PageRank, impact analysis, multi-hop traversal
- **RRF symbol-lane fusion:** Identifiers extracted from query, fused with semantic results
- **HTTP-MCP transport:** Remote access without stdin/stdout piping
- **SQLite KNN:** Relational queries + vector search in one transaction
- **Multi-language call graph:** Go, TypeScript, Python, Java, Rust, Markdown, YAML
- **Pending summaries queue:** Background LLM summarization without blocking queries
- **Proper index lifecycle:** Auto-watch, incremental updates, reindex, stale detection

---

## 4. Gaps: What dex Needs to Match lean-ctx

### G1: Local embedding (zero-config)

**Gap:** dex requires an external embedding endpoint. lean-ctx works out of the box.  
**Fix:** Ship a default local model. Options:
- Call ollama's HTTP API if it's already running — zero binary size cost
- Embed a small ONNX model via CGO (like lean-ctx), or use a pure-Go ONNX runner
- Auto-detect: if `EMBED_ENDPOINT` is set → use it; if ollama is on localhost → use it; else pull a model to `~/.dex/models/`

The binary should answer queries without the user knowing what "embedding" means.

### G2: Session memory

**Gap:** dex has no cross-request memory. Every call is stateless.  
**Fix:** Add a `sessions` table (or a sidecar JSON file) per project:
- Current task declaration (`what am I working on`)
- Files read/modified in this session with timestamps
- Short decision log (manually added via `index_session update`)
- Persist in the same SQLite DB or alongside it

This is a small investment with outsized ergonomic returns.

### G3: Knowledge base

**Gap:** dex doesn't accumulate project facts.  
**Fix:** Add a `knowledge` table to the project DB:
- Rows: fact text, archetype (Architecture/Gotcha/Convention/…), confidence, created_at, last_seen_at, retrieval_count
- Exposed via MCP: `knowledge_add`, `knowledge_list`, `knowledge_query`
- Auto-inject top-K high-salience facts into `ask` context before sending to LLM

This turns dex from a stateless query tool into a project-aware assistant.

### G4: More read modes for file_view

**Partially done (recent commits):** `full`, `signatures`, `lines:N-M` landed.  
**Still missing:**
- `map` — structural outline without LLM (tree-sitter symbols, imports, exports; lean-ctx parity)
- `diff` — show what changed since last read (requires session tracking)
- `task` — filter content to what's relevant to declared task (requires session G2)
- `aggressive` — maximum lossy compression for large files

### G5: Shell output compression tool

**Gap:** lean-ctx intercepts all shell output; dex has no equivalent.  
**Option A:** Add a `compress_output` MCP tool that accepts raw command output and returns compressed version (lean-ctx patterns ported to Go).  
**Option B:** Ship a thin shell hook script (`eval "$(dex shell-hook)"`) that wraps commands.  
Option A is simpler, more portable, and doesn't require shell integration.

### G6: Directory listing tool

**Gap:** dex has no `ctx_tree` equivalent. Agents use `ask` or an external `ls`.  
**Fix:** Add `file_tree(path, depth, filter)` — returns compact directory listing with file counts, filtered to indexed extensions. Useful for orientation without reading file contents.

### G7: Token tracking

**Gap:** dex doesn't track or report token savings.  
**Fix:** Add optional telemetry to `status`:
- Tokens served (cumulative)
- Compression ratio achieved (via mode)
- Cache hit rate (if file cache is added)

---

## 5. GPU Utilization: The Differentiator

The existing `docs/vision.md` is correct: dex should not bundle llama.cpp. The binary's job is **awareness and routing**, not inference. But awareness requires more than checking a single env var.

### G8: Local model discovery

Auto-detect what's running:
```
1. Probe ollama (localhost:11434) — list models, pick best embedding model by size/name
2. Probe llama-server / vLLM candidates on common ports (8080, 8000, 11434)
3. Check VRAM heuristic: NVIDIA via nvml.h (CGO optional), Apple via sysctl or IOKit
4. Report discovery in status: { "local_embed": "nomic-embed-text:latest@ollama", "local_llm": "llama3.2:3b@ollama", "free_vram_gb": 18.4 }
```

Expose as `status` fields. Let the user and agent see what dex found.

### G9: Tiered routing by GPU load

When a local GPU endpoint exists, route differently:
- **Embeddings:** Local ONNX or ollama embed model (low latency, no cost, no privacy concern)
- **file_view:** Local LLM (ollama, llama-server) for non-sensitive files; remote for high-complexity requests
- **ask synthesis:** Local LLM if VRAM headroom exists; skip synthesis entirely if not (return raw hits)
- **Reranking:** Local cross-encoder if available; fall back to score-based

Config: `DEX_LOCAL_EMBED_URL`, `DEX_LOCAL_LLM_URL`, `DEX_PREFER_LOCAL=true`. Auto-detect fills in what's missing.

### G10: Streaming inference for ask/summarize

lean-ctx uses request/response only. dex already has HTTP-MCP (streaming-capable).  
Hook up streaming responses from local llama-server to MCP streaming events. Agents get the first tokens in <200ms instead of waiting for a full LLM response. This matters on large `file_view` calls.

### G11: Embedding model auto-pull

If no embedding endpoint is found and ollama is installed:
```
dex index status → "no embed endpoint found; run: ollama pull nomic-embed-text"
dex reindex --pull-model  # pulls nomic-embed-text via ollama API, then reindexes
```

Lower barrier than any lean-ctx workaround.

### G12: VRAM-aware batch sizing

Large reindex jobs can OOM a GPU if batches are too big. Detect free VRAM (via `nvidia-smi --query-gpu` or `system_profiler` on macOS), adjust embedding batch size dynamically:
- <4 GB free → batch 8
- 4–16 GB → batch 64
- >16 GB → batch 256

---

## 6. What's Already Adopted (Recent Commits as of 2026-06-04)

| Commit | What Landed | lean-ctx Parity |
|--------|-------------|-----------------|
| `f1c51a5` / `85fc282` | `view_summarize` modes: `full`, `signatures`, `lines:N-M`; `SymbolsByFile()` for signatures mode | Partial G4: 3 of 5 read modes |
| `8c464bd` | Symbol lane fused into `search_semantic` via RRF; `extractIdentifiers()` | None in lean-ctx (dex-specific improvement) |
| `6a7c924` | Native HTTP-MCP transport at `/v1/projects/{id}/mcp` | None in lean-ctx (dex-specific) |
| `e681778` | Remote stdio shim (`dex mcp --remote`); `toolSurface` abstraction | None in lean-ctx (dex-specific) |
| `9f7da0e` | Five tools in one commit: `view_summarize mode=map` (imports + exported symbols, no LLM); `graph_impact` (transitive caller BFS, depth-sorted); `overview` (task-relevant file ranking with centrality boost); `smells` (long functions, dead exports, god files via SQL); `routes` (HTTP/MCP/gRPC handler detection from name patterns + edges) | Partial G4 (`map` mode); `overview` is a lean-ctx `ctx_overview` analogue |
| `bc6d9d8` | 7 lean-ctx features: graph-proximity RRF lane; graph-aware hints in `view_summarize`; 6 new shell compression patterns (kubectl/make/gh/pip/terraform/cmake); `session action=snapshot` recovery block; type-first ordering in `signatures` mode; repeated-search throttle hints; `knowledge action=export/import` | N1–N7 (second-wave gaps) |
| `944cbb8` | 6 lean-ctx features: `readOnlyHint=true` on 18 read-only MCP tools; view_summarize large-file mode hint (>250 lines); 3 new knowledge archetypes (Dependency/Pattern/Fact); cache-stable LLM prompt ordering (SESSION CONTEXT moved to suffix); post-RRF local reranking (noise penalties, definition boost, coherence boost, MMR diversity); BM25 path-column weighting 2× | N8–N13 (third-wave gaps) |
| `ed5632f` | N14: structured map for Markdown/JSON/YAML/TOML/lock files (`server_noncode_map.go`); N15: `search_context` tool (search+signatures+best-symbol body in one call); N16: task-relevance inline in signatures/map modes | N14–N16 |
| `249ea3c` | N17: cold-start `overview` partial view — project markers + depth-2 fs tree + knowledge when index is empty | N17 |
| `e3efa07` | N20: `agent` coordination bus — `agents` + `agent_messages` tables; `announce`/`post`/`read`/`list` actions; TierStandard; REST at `/v1/projects/{id}/agent`; remote shim + streamable-HTTP MCP surface updated | N20 |

All gaps G1–G9, G11, G12 are done. G10 (streaming MCP responses) remains blocked on an SDK gap — `ServerSession` is not injectable into tool handler context in go-sdk v1.4.1. Second-wave gaps N1–N7 (lean-ctx 3.6.x) done as of `bc6d9d8`. Third-wave gaps N8–N13 (lean-ctx 3.6.13–3.7.x) done as of `944cbb8`. Fourth-wave gaps N14–N17 done as of `249ea3c`. N18, N21, N22 done as of `b1e4545`. N20 done as of `e3efa07`. N19 (activity-weighted nudge) remains open.

---

## 7. Priority Ranking

Ordered by impact-to-effort ratio.

### Original gaps (G-series)

| # | Gap | Impact | Effort | Status | Notes |
|---|-----|--------|--------|--------|-------|
| 1 | **G1: Local embedding via ollama auto-detect** | High | Low | ✅ done | `DEX_EMBED_URL` unset → probe localhost:11434, pick best embed model |
| 2 | **G4: `map` read mode + `overview` tool** | High | Low | ✅ done | Landed in `9f7da0e` |
| 3 | **G8: Local model discovery in status** | Medium | Low | ✅ done | `status` returns `OllamaEndpoint`, `OllamaEmbedModels`, `OllamaChatModels` |
| 4 | **G6: file_tree tool** | Medium | Low | ✅ done | `file_tree` MCP tool + `FileTree()` store method |
| 5 | **G2: Session memory** | High | Medium | ✅ done | `sessions`/`session_files` tables; `session` MCP tool |
| 6 | **G3: Knowledge base** | High | Medium | ✅ done | `knowledge_facts` table; `knowledge` MCP tool; top-K injected into `ask` |
| 7 | **G9: Tiered routing** | High | Medium | ✅ done | `DEX_CHAT_URL` unset → probe ollama for code model, else fallback |
| 8 | **G10: Streaming responses** | Medium | Medium | blocked | Requires `ServerSession` in tool handler ctx — SDK gap; revisit when SDK exposes session via context |
| 9 | **G5: Shell compression tool** | Medium | High | ✅ done | `compress_output` MCP tool; 13 command patterns + generic; POST /v1/compress REST endpoint |
| 10 | **G11: Embedding auto-pull** | Low | Low | ✅ done | `dex reindex --pull-model`; `index status` hints `ollama pull nomic-embed-text` when ollama is up but has no embed models |
| 11 | **G12: VRAM-aware batch sizing** | Low | Medium | ✅ done | `embed.FreeVRAMGB()` probes nvidia-smi/system_profiler; auto batch 8/64/256 for <4/4-16/>16 GB VRAM |

### Second-wave gaps (N-series, lean-ctx 3.6.x features not in original doc)

| # | Gap | Impact | Effort | Status | Notes |
|---|-----|--------|--------|--------|-------|
| N1 | **Graph proximity 3rd RRF lane** | High | Medium | ✅ done | `GraphNeighborFiles` + `HitsForFiles`; fused at 0.5× weight via `fuseWithGraphNeighbors` |
| N2 | **Graph-aware hints in file_view** | Medium | Low | ✅ done | `graphRelatedHint` appends `# Related (call graph): ...` in signatures + map modes |
| N3 | **More shell compression patterns** | Medium | Low | ✅ done | kubectl, make, gh, pip/uv, terraform, cmake/ninja added (7 → 13 handlers) |
| N4 | **Session survival snapshot** | Medium | Low | ✅ done | `session action=snapshot` emits file_view + search_semantic recovery calls |
| N5 | **Prefix-cache-friendly ordering** | Low | Low | ✅ done | `formatSignatures` emits types/structs/interfaces before functions/methods |
| N6 | **Progressive search throttling** | Low | Low | ✅ done | Repeated identical searches (≥4 in 5 min) surface a hint to use knowledge instead |
| N7 | **Knowledge export/import** | Low | Low | ✅ done | `knowledge action=export` → JSON; `action=import` ← `[{archetype,body,confidence},...]` |
| N8 | **`readOnlyHint` MCP annotations** | Medium | Low | ✅ done | 18 read-only tools annotated; enables Claude Code plan mode without prompts |
| N9 | **file_view mode hint** | Low | Low | ✅ done | >250-line files in full mode emit `⚠` hint suggesting signatures/map |
| N10 | **Expand knowledge archetypes** | Low | Low | ✅ done | Added Dependency (1.1×), Pattern (1.0×), Fact (1.0×) |
| N11 | **Cache-stable prompt ordering** | Low | Low | ✅ done | SESSION CONTEXT moved to end of `buildAnswerEvidence` — code prefix is stable for KV-cache |
| N12 | **Post-RRF local reranking** | High | Medium | ✅ done | `rerankLocal()`: noise 0.3×, definition boost 1.5×, coherence 1.15×, MMR decay 0.7× |
| N13 | **BM25 path-column weighting** | Medium | Low | ✅ done | `bm25(chunks_fts, 1.0, 2.0, 0.5)` — path column weighted 2× for path-aware queries |
| N14 | **Structured `map` mode for non-code files** | High | Low-Medium | ✅ done | `nonCodeMap()` in `server_noncode_map.go`: Markdown heading tree, JSON key structure, YAML hierarchy, TOML sections, lock-file dep counts. Early-returns before store.Open — no index needed. |
| N15 | **Composite `search_context` tool** | High | Medium | ✅ done | `search_context` MCP tool in `server_compose.go`. Embeds query, aggregates top-k files, returns signatures + best-symbol body in one call. TierStandard; REST at `/v1/projects/{id}/search/context`. |
| N16 | **Task-relevance inline in `signatures`/`map` mode** | Medium | Medium | ✅ done | `inlineTaskSymbol()` appended after `formatSignatures`/`formatMap`; word-overlap scoring via `tokenizeWords`+`symbolQueryScore`; body capped at 60 lines; no-op when no session task. |
| N17 | **Cold-start `overview` partial view** | Medium | Low | ✅ done | `overview` opens store before embedding, checks `len(lineCounts)==0`; `overviewPartial()` returns status="partial" with project markers, depth-2 fs tree, and knowledge facts. |
| N18 | **Knowledge revision tracking** | Low | Low | ✅ done | `revision_count INTEGER` added to `knowledge_facts`; incremented on ON CONFLICT UPDATE; "Confirmed (revision N)." response; `rev N` in `knowledge action=list`. Migrated on existing DBs via guarded `ALTER TABLE` + meta flag. (`73e652e`) |
| N19 | **Activity-weighted knowledge nudge** | Low | Medium | ☐ open | lean-ctx 3.6.24: tracks weighted activity per session — edits +4, shell test/build +3, shell +2, new file read +1. When `weighted_score ≥ 20` AND `significant_tools ≥ 5` AND no knowledge recorded in 8 minutes, surfaces a context-sensitive hint to record findings. dex current: no nudge mechanism. Fix: accumulate activity weights in the session layer (can be in-memory per server lifetime); append a hint to `search_semantic` or `file_view` responses when threshold is crossed. |
| N20 | **Multi-agent coordination bus** | Medium | Medium | ✅ done | `agents` + `agent_messages` tables; `agent` MCP tool (TierStandard) with actions `announce`/`post`/`read`/`list`; topic filtering + `since_id` pagination; REST at `/v1/projects/{id}/agent`; remote shim + streamable-HTTP surface updated. (`e3efa07`) |
| N21 | **Batch file reads (`ctx_multi_read`)** | Low | Low | ✅ done | `file_view` accepts optional `paths[]` (max 10, same mode); returns concatenated `## path\ncontent` sections in one call. Path-traversal check applied per entry. (`b1e4545`) |
| N22 | **Session-aware re-read cache (`ctx_read` caching)** | Medium | Low | ✅ done | `file_view` returns `etag` (sha256[:16]) on every response. On re-reads with matching etag, server checks in-memory `readCache` (sessionID→path→etag); returns `status=unchanged` only when this session previously received the file at that hash — prevents false hits from stale etags in new sessions or after context compression. (`8c7dc09`) |
| N22 | **Knowledge auto-consolidation** | Low | Low | ✅ done | `knowledge action=consolidate` reads session task + notes, asks chat LLM to extract a JSON array of facts, stores each via `KnowledgeAdd`. Requires `DEX_CHAT_URL`; no-op hint when chat is unconfigured. (`7fa3eda`) |

---

## 8. Design Constraints

- **No bundled inference.** dex doesn't link llama.cpp or ship ONNX models. `docs/vision.md` is clear.
- **No breaking API changes.** Existing tool names stay stable.
- **SQLite-first.** New storage (sessions, knowledge) goes into the existing per-project DB.
- **Graceful degradation.** Every GPU/local-model feature has a no-model fallback. dex works without any model endpoint for graph, symbol, and `file_view` signatures/lines modes.
- **Go stdlib and minimal deps.** No heavy ML frameworks in the binary.

---

## 9. What dex Can Do That lean-ctx Never Will

lean-ctx is built around **context compression** for an LLM session. It's a runtime layer on top of the model conversation.

dex is built around **indexed code intelligence**. It understands the repo's *structure* — call graphs, package topology, symbol relationships, PageRank centrality.

The combination of:
- Local GPU inference (G1, G8, G9)
- Session + knowledge persistence (G2, G3)
- Structural code intelligence (graphs, RRF, impact analysis)

...is a product lean-ctx can't build. lean-ctx compresses what's already in context. dex knows what's worth putting there in the first place.

The natural end state: dex does the retrieval and structural reasoning; lean-ctx (or dex's own session layer) does the compression and persistence. They're complementary, not competing.
