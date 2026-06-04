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
| Config | TOML | `.dex/config.yml` (index + endpoints/models/tools) |
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

**Done.** `ctx_shell` MCP tool executes shell commands with compressed output. Three-tier output policy: `passthrough` (auth flows, dev servers, interactive REPLs — output unchanged), `verbatim` (curl, jq, cat, git log — ANSI stripped, structure preserved), `compress` (build/test/lint — 56+ patterns, 60–99% reduction). Auth-flow detection guards against compressing OAuth device-code output. Heredoc-to-file writes blocked in addition to `>` redirect and `tee`. REST at `/v1/projects/{id}/shell`.

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
| `eb1b0b1` | G5: `ctx_shell` MCP tool — execute shell commands with compressed output; 56+ patterns; `raw` flag; exit code; line savings metadata; REST at `/v1/projects/{id}/shell` | G5 |
| `0523950` | G5+: `ctx_shell` output policy — `passthrough` (auth flows/dev servers/REPLs), `verbatim` (curl/jq/git log), `compress` (build/test/lint); auth-flow detection; heredoc-to-file block | G5 |
| `e3efa07` | N20: `agent` coordination bus — `agents` + `agent_messages` tables; `announce`/`post`/`read`/`list` actions; TierStandard; REST at `/v1/projects/{id}/agent`; remote shim + streamable-HTTP MCP surface updated | N20 |
| `42f22e7` | N19: activity-weighted knowledge nudge (in-memory per-project score, fires hint when score≥20 + calls≥5 + 8 min silence); N24: `search_grep` tool (literal/regex search over indexed files, falls back to fs walk) | N19, N24 |
| `6db98cd` | N25: `file_view` graceful fallback (chat-nil→map, chat-error→raw content+hint); N26: compact `formatSignatures` (⊛ exported marker, declaration line only, ~10× smaller); N27: `overview` task classification + per-kind strategy hint + knowledge wake-up | N25–N27 |
| `45e0151` | Fix `signatures` mode: removed package-level deps line (was showing whole-package imports, not file-level) | N26 fix |
| `25a7e36` | Fix `signatures` ⊛ marker: struct fields/imports/file nodes no longer marked exported; `SessionTrackFile` store method; `sessionAutoFile` async goroutine wired into all `file_view` success paths (N28) | N26 fix, N28 |
| `d4e1962` | N29: `file_tree` `text` field — compact aligned column view with totals header | N29 |
| `380338c` | N30: session metrics — `duration`, `file_count`, `note_count` in `session action=get` | N30 |
| `5b390f9` | go-sdk upgrade v1.4.1 → v1.6.1: `ServerRequest[P].Session` is a direct field — G10 SDK blocker resolved | SDK dep |
| `e9afeae` | G10: `chat.GenerateStream` (SSE `stream=true`); `ask` synthesis and `file_view` full-mode stream tokens via `req.Session.Log(level=debug, logger=dex/ask\|dex/file_view)` while the tool call is in flight; falls back to blocking `Generate` when no session (REST, batch, tests) | G10 |

All gaps G1–G12 are done. go-sdk upgraded to v1.6.1 (`5b390f9`) — `ServerSession` is now a direct field on every tool request, unblocking G10. G10 landed in `e9afeae`: `chat.GenerateStream` (SSE) streams tokens via `req.Session.Log` in `ask` and `file_view` full mode. Second-wave gaps N1–N7 (lean-ctx 3.6.x) done as of `bc6d9d8`. Third-wave gaps N8–N13 (lean-ctx 3.6.13–3.7.x) done as of `944cbb8`. Fourth-wave gaps N14–N17 done as of `249ea3c`. N18, N21, N22 done as of `b1e4545`. N20 done as of `e3efa07`. N19 (activity-weighted nudge) and N24 (search_grep) done as of `42f22e7`. N25–N27 done as of `6db98cd`. N28 and exported-field fix done as of `25a7e36`. N29 done as of `d4e1962`. N30 done as of `380338c`. N31 (Claude Code hook + plugin integration) done as of `3aafa97`/`497e228`/`a0f8343`. N32 (tool category-prefix naming) done as of `b51ec6f`. N33 (config YAML unification, TOML eliminated) done as of `e72659e`/`c7f0611`. N34 (graceful BM25 degradation + ollama auto-start) done as of `fcd4397`. N35 (`ctx_nav` tool) done as of `5ffeaac`/`4de3f5a`. N36 (`dex doctor`) done as of `35c68b5`. N37 (logx subsystem tags + canonical attrs + observability doc) done as of `f397695`/`bba44f9`. **All gaps G1–G12 and N1–N37 closed.**

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
| 8 | **G10: Streaming responses** | Medium | Medium | ✅ done | `chat.GenerateStream` (SSE) + `req.Session.Log` per token in `ask` and `file_view` full mode. go-sdk v1.6.1 exposes `req.Session` directly — SDK gap closed. (`e9afeae`) |
| 9 | **G5: Shell compression tool** | Medium | High | ✅ done | `ctx_shell` MCP tool; 3-tier output policy (passthrough/verbatim/compress); 56+ patterns; auth-flow protection; heredoc block; REST at `/v1/projects/{id}/shell` |
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
| N19 | **Activity-weighted knowledge nudge** | Low | Medium | ✅ done | In-memory `activityState` per project tracks `weightedScore` (shell test/build +3, shell +2, search/file_view +1) and `significantCalls`. When score ≥ 20, calls ≥ 5, and no knowledge recorded in 8 min, appends a contextual hint to `search_semantic` / `ask` responses suggesting the agent record its findings. `activityKnowledgeRecorded` resets the timer on any `knowledge action=add`. (`42f22e7`) |
| N20 | **Multi-agent coordination bus** | Medium | Medium | ✅ done | `agents` + `agent_messages` tables; `agent` MCP tool (TierStandard) with actions `announce`/`post`/`read`/`list`; topic filtering + `since_id` pagination; REST at `/v1/projects/{id}/agent`; remote shim + streamable-HTTP surface updated. (`e3efa07`) |
| N21 | **Batch file reads (`ctx_multi_read`)** | Low | Low | ✅ done | `file_view` accepts optional `paths[]` (max 10, same mode); returns concatenated `## path\ncontent` sections in one call. Path-traversal check applied per entry. (`b1e4545`) |
| N22 | **Session-aware re-read cache (`ctx_read` caching)** | Medium | Low | ✅ done | `file_view` returns `etag` (sha256[:16]) on every response. On re-reads with matching etag, server checks in-memory `readCache` (sessionID→path→etag); returns `status=unchanged` only when this session previously received the file at that hash — prevents false hits from stale etags in new sessions or after context compression. (`8c7dc09`) |
| N23 | **Knowledge auto-consolidation** | Low | Low | ✅ done | `knowledge action=consolidate` reads session task + notes, asks chat LLM to extract a JSON array of facts, stores each via `KnowledgeAdd`. Requires `DEX_CHAT_URL`; no-op hint when chat is unconfigured. (`7fa3eda`) |
| N24 | **`search_grep` tool** | Medium | Low | ✅ done | Literal/regex pattern search across all indexed files. Uses index `FileTree()` when available, falls back to `filepath.Walk` (skips `.git`/`vendor`/`node_modules`). `max_results` cap (default 50). TierStandard; REST at `/v1/projects/{id}/search/grep`. (`42f22e7`) |
| N25 | **`file_view` graceful fallback** | Medium | Low | ✅ done | `mode=full` with no chat client configured silently falls back to `mode=map` instead of erroring. Chat errors (model not found, service unreachable) return raw file content with a hint rather than a hard error status — agents always get something useful. (`6db98cd`) |
| N26 | **Compact `signatures` mode** | Medium | Low | ✅ done | Rewritten `formatSignatures`: compact header (`path NL (N symbols)`), `⊛` marker only on top-level exported symbols (func/type/var/const — not struct fields, imports, or file nodes), declaration line per exported symbol. Output ~10× smaller than before. (`6db98cd`, `25a7e36`) |
| N27 | **`overview` task classification + hints** | Medium | Low | ✅ done | Every successful `overview` call injects a `[TASK:kind SCOPE:scope]` classification (fix/generate/refactor/debug/test/docs/explore) with a per-kind read strategy hint. Also surfaces top-5 knowledge facts as a wake-up briefing. (`6db98cd`) |
| N28 | **Session auto-tracking on `file_view`** | Medium | Low | ✅ done | Every successful `file_view` call asynchronously records the file in the active session (if one with a declared task exists). Uses `SessionTrackFile` which is a no-op when no task-bearing session is active — never creates a spurious session. (`25a7e36`) |
| N29 | **`file_tree` compact text field** | Low | Low | ✅ done | `file_tree` response includes a `text` field: aligned column view (`filename   Nc`) with header showing total files and chunks. Structured `entries[]` array unchanged. (`d4e1962`) |
| N30 | **Session metrics** | Low | Low | ✅ done | `session action=get` now returns `duration` (human-readable: `1h03m`, `4m12s`, `38s`), `file_count`, and `note_count` alongside the existing fields. (`380338c`) |
| N31 | **Claude Code hook + plugin integration** | High | Medium | ✅ done | `dex hook {inject,rewrite,redirect,observe}` (`cmd/dex/hook.go`) wired via committed `.claude/settings.json` + `.claude-plugin/manifest.json`. `inject` (UserPromptSubmit) prepends `ask` context; `rewrite` (PreToolUse/Bash) maps `rg`→`search semantic` and pipes simple `grep -r` through `compress-stdin`; `redirect` (PreToolUse/Read) serves a graph-built signatures view for indexed code files >400 lines (~97% line reduction, head order kept) — replaces the old CompressText path which tail-truncated and dropped the file head; `observe` (PostToolUse/Stop/PreCompact) logs `{ts,tool_name,tokens}` to `hooks.jsonl`. All fail open. Mirrors lean-ctx's `lean-ctx hook {observe,rewrite,redirect}` design. (`3aafa97`, `497e228`, `a0f8343`) |
| N32 | **Tool category-prefix naming scheme** | Medium | Low | ✅ done | All MCP tools renamed to category-prefix convention: `search_*` (retrieval), `graph_*` (static graph), `file_*` (file access), `ctx_*` (cross-cutting agent context), `spec_*` (verification). `ask`/`status`/`compress_output` remain prefix-free. Agents can infer tool names from purpose without reading docs. `DEX_EXPOSE_RAW_TOOLS=1` kept as backward-compat alias for `DEX_TOOLS=power`. (`b51ec6f`) |
| N33 | **Config unified in `.dex/config.yml`; TOML parser eliminated** | Medium | Low | ✅ done | All `DEX_*` env vars (embed/chat/rerank/compress/draft/summary endpoints, models, tool tier, env passthrough) now live in `.dex/config.yml` alongside `index.include`/`index.ignore`. Priority: env > file > default. The hand-rolled TOML parser (`loadIndexConfig`/`parseArrayItems`/`stripComment`) is gone; `gopkg.in/yaml.v3` is the single config reader. No TOML fallback. (`e72659e`, `c7f0611`) |
| N34 | **Graceful startup without ollama; query-time BM25 degradation** | High | Medium | ✅ done | Three improvements: (1) Query-time: `store.scoreSemantic` treats nil embed vector as "no semantic leg" — BM25 + symbol + graph still run, never a hard error. CLI `dex search semantic` catches `embed.ErrUnreachable` and degrades to BM25-only with an actionable hint. (2) Auto-start: when `DEX_EMBED_URL`/`DEX_CHAT_URL` unset and ollama installed but not listening, dex attempts `ollama serve` (detached, polled, bounded); opt-out with `DEX_NO_AUTO_OLLAMA=1`. (3) Actionable error text in `dex env`. (`fcd4397`) |
| N35 | **`ctx_nav` tool — runtime tool-routing guide** | Medium | Low | ✅ done | `ctx_nav` (TierStandard, zero-arg, no index/embed required) returns a structured catalogue of all tools for the active tier: `name`, `tier` (`all`/`standard`/`power`), `purpose` (one-line), `when` (call guidance). Also returns a `guide` markdown narrative oriented around the active surface. Replaces the CLAUDE.md snippet approach: agents orient themselves at session start without reading docs. REST at `GET /v1/nav` (global, unauthenticated). (`5ffeaac`, `4de3f5a`) |
| N36 | **`dex doctor` — one-shot setup diagnostic** | High | Low | ✅ done | `dex doctor` runs all configured checks in one pass: index dir (exists, writable, project count), endpoint health (embed/chat/rerank/compress/draft/summary, 5 s timeout per probe), project config (`.dex/config.yml` presence + `index.include`), and MCP wiring (`settings.json` mcpServers or `.claude-plugin/manifest.json`). Prints a labelled pass/warn/skip/fail table with actionable `→ fix hint` lines. Exits 0 when all critical checks pass; exits 1 when embed is unreachable or index dir is unwritable. Chat/rerank/compress failures are non-critical warnings. (`35c68b5`) |
| N37 | **logx observability — subsystem tags + canonical attr set** | Low | Low | ✅ done | `subsystem=<name>` wired via `.With()` at construction in `index.New()` (tags `indexer` + `drain` from the same base), `graph.New()`, `watch.New()`, and `mcp.(*Server).RunHTTP()` — every log line carries the subsystem without per-call annotation. Canonical attr set finalized: `batch_total` (was `"of"`), `start_line` (was `"start"`), plus `path`, `root`, `dir`, `kind`, `count`, `batch`, `chunks`, `op`. `docs/observability.md` documents the full field table, subsystem roster, grep/LogQL/jq query examples, and the `logx` extension guide. (`f397695`, `bba44f9`) |

---

## 8. Design Constraints

- **No bundled inference.** dex doesn't link llama.cpp or ship ONNX models. `docs/vision.md` is clear.
- **No breaking API changes.** Existing tool names stay stable.
- **SQLite-first.** New storage (sessions, knowledge) goes into the existing per-project DB.
- **Graceful degradation.** Every GPU/local-model feature has a no-model fallback. dex works without any model endpoint for graph, symbol, and `file_view` signatures/lines modes.
- **Go stdlib and minimal deps.** No heavy ML frameworks in the binary.

---

## 9. Reference Hardware & Recommended Models

Development machine: Apple Silicon, **64 GB unified memory**.

| Role | Model | Size (Q4) | Fits in 64 GB | Notes |
|------|-------|-----------|---------------|-------|
| Embeddings (`DEX_EMBED_URL`) | `nomic-embed-text:latest` | 274 MB | ✅ | Fast, good quality; already pulled |
| Code summarization / `file_view` (`DEX_CHAT_URL`) | `qwen2.5-coder:32b` | ~20 GB | ✅ | Best code comprehension at this size; pulled 2026-06-04 |
| General chat / `ask` synthesis | `qwen2.5-coder:32b` | — | ✅ | Same model for both roles via ollama |
| Previously attempted | `qwen2.5-coder:1.5b` | 986 MB | ✅ | Too small for useful summarization |
| Previously attempted | `qwen2.5-coder:14b` | ~9 GB | ✅ | Good but not pulled; 32b preferred on 64 GB |

**Ollama endpoint:** `http://localhost:11434` (default).  
**dex env:** `DEX_EMBED_URL=http://localhost:11434` + `DEX_CHAT_URL=http://localhost:11434` + model auto-detected from `OLLAMA_CHAT_MODEL` or ollama list.

With 64 GB unified memory, `qwen2.5-coder:32b` runs fully in-memory at ~20 GB, leaving 44 GB for the OS, Claude Code, and the index. No swapping, full token throughput.

---

## 10. What dex Can Do That lean-ctx Never Will

lean-ctx is built around **context compression** for an LLM session. It's a runtime layer on top of the model conversation.

dex is built around **indexed code intelligence**. It understands the repo's *structure* — call graphs, package topology, symbol relationships, PageRank centrality.

The combination of:
- Local GPU inference (G1, G8, G9)
- Session + knowledge persistence (G2, G3)
- Structural code intelligence (graphs, RRF, impact analysis)

...is a product lean-ctx can't build. lean-ctx compresses what's already in context. dex knows what's worth putting there in the first place.

The natural end state: dex does the retrieval and structural reasoning; lean-ctx (or dex's own session layer) does the compression and persistence. They're complementary, not competing.
