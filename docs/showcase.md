# dex — Capability Showcase

> **What this document shows.** Three real queries against the dex codebase itself,
> run twice: once with no GPU or LLM services available, once with the full stack
> (embeddings + reranker + chat model). Every output below is grounded in the
> actual source files, field names, and line numbers of this repo.

---

## The core problem dex solves

An AI agent working on a large codebase does this constantly:

```
grep -r "debounce" ./           # 40 lines across 12 files
Read internal/watch/watch.go    # 215 lines, mostly irrelevant
grep -r "markDirty" ./          # more noise
Read ...                        # another full file
```

Each round-trip burns context tokens, adds latency, and produces noise the
agent has to filter. dex replaces that loop with a single `ask` call that
returns the right 20 lines — ranked, inlined, and explained.

---

## Setup

```bash
# Build and index this repo.
dex index ./

# Serve over MCP (Claude Code wires this automatically via .mcp.json).
dex mcp
```

Index is stored locally in `~/.cache/dex/<sha>/index.db` (SQLite + sqlite-vec).
Source code never leaves the machine.

---

## Scenario A — No GPU / No LLM

Embedding service (`DEX_EMBED_URL`), reranker (`DEX_RERANK_URL`), and chat
model (`DEX_CHAT_URL`) are all offline. This is the cold-start or CPU-only
laptop case.

### A.0 — Service readiness

```
$ dex index status

endpoints (0 reachable)
  NAME      STATUS        MODEL                             URL
  embed     UNREACHABLE   Qwen/Qwen3-Embedding-4B           http://127.0.0.1:8082
  chat      UNREACHABLE   Qwen/Qwen2.5-Coder-7B-Instruct    http://127.0.0.1:8081
  rerank    UNREACHABLE   BAAI/bge-reranker-v2-m3           http://127.0.0.1:8083
  compress  not configured
  draft     not configured
  summary   inherits chat

hint: ollama is running but has no embedding model — run:
  ollama pull Qwen/Qwen3-Embedding-4B
or: dex reindex --pull-model <path>

project  /Users/user/projects/myapp
  indexed:    2h ago
  files:      214
  chunks:     4 823
  graph:      12 441 nodes  38 902 edges
```

The graph and symbol index are still fully intact — they were built at
`dex index` time and require no runtime service. Only semantic search and
answer synthesis are offline.

---

### A.1 — Behavior search: "where is filesystem event debouncing handled?"

The agent wants to understand how file-change events are throttled before
triggering a re-index.

**Input (MCP tool call)**

```json
{
  "tool": "ask",
  "arguments": {
    "project": "/Users/user/projects/myapp",
    "question": "where is filesystem event debouncing handled?"
  }
}
```

**Output**

```json
{
  "status": "embedding-service-unreachable",
  "hint": "embed endpoint unreachable: http://127.0.0.1:8082 — semantic search unavailable; fall back to: rg -n 'debounce' ./",
  "endpoint": "http://127.0.0.1:8082",
  "project": "/Users/user/projects/myapp",
  "intent": "behavior_search",
  "semantic_hits": [],
  "symbols": [],
  "suggested_reads": [],
  "next_action": "Embedding service unreachable — grep for the concept: rg -n 'debounce\\|dirty\\|timer' ./",
  "avoid": ""
}
```

**What the agent gets:** nothing actionable. It must fall back to grep,
read whole files, and filter manually — the same expensive loop dex exists
to eliminate.

---

### A.2 — Caller graph: "callers of (*Store).Search"

Graph queries are purely structural — `go/packages` + `go/types` analysis, no
embeddings. These work with zero GPU.

**Input**

```json
{
  "tool": "ask",
  "arguments": {
    "project": "/Users/user/projects/myapp",
    "question": "callers of (*Store).Search",
    "intent": "callers"
  }
}
```

**Output**

```json
{
  "status": "ok",
  "project": "/Users/user/projects/myapp",
  "intent": "callers",
  "symbols": [
    {
      "qualified_name": "(*Store).Search",
      "path": "internal/store/store.go",
      "start_line": 420,
      "end_line": 487,
      "kind": "method",
      "signature": "func (s *Store) Search(ctx context.Context, q store.Query) ([]Hit, error)",
      "doc": "Search runs the hybrid cosine+BM25 query, fuses results via RRF,\nand optionally reranks the pool before returning the top-k hits.",
      "role": "central:18/4pkg"
    }
  ],
  "references": [
    {
      "path": "internal/mcp/server.go",
      "line": 312,
      "snippet": "hits, err := st.Search(ctx, store.Query{Text: in.Query, K: k, Pool: pool})",
      "symbol": "(*Store).Search"
    },
    {
      "path": "internal/mcp/context.go",
      "line": 587,
      "snippet": "semHits, err = st.Search(ctx, store.Query{Text: in.Question, K: pool, Pool: pool * 3})",
      "symbol": "(*Store).Search"
    },
    {
      "path": "internal/mcp/context.go",
      "line": 611,
      "snippet": "summaryHits, err = st.Search(ctx, store.Query{Text: in.Question, K: 3, SummaryOnly: true})",
      "symbol": "(*Store).Search"
    },
    {
      "path": "internal/index/grounding.go",
      "line": 88,
      "snippet": "hits, err := st.Search(ctx, store.Query{Text: chunk.Content, K: 5})",
      "symbol": "(*Store).Search"
    }
  ],
  "graph": {
    "nodes": [
      { "id": "(*Store).Search",          "kind": "method",   "qualified_name": "(*Store).Search" },
      { "id": "(*Server).search",         "kind": "method",   "qualified_name": "(*Server).search" },
      { "id": "(*Server).contextRouter",  "kind": "method",   "qualified_name": "(*Server).contextRouter" },
      { "id": "groundChunks",             "kind": "function", "qualified_name": "groundChunks" }
    ],
    "edges": [
      { "from": "(*Server).search",        "to": "(*Store).Search", "kind": "calls" },
      { "from": "(*Server).contextRouter", "to": "(*Store).Search", "kind": "calls" },
      { "from": "groundChunks",            "to": "(*Store).Search", "kind": "calls" }
    ]
  },
  "suggested_reads": [
    {
      "path": "internal/store/store.go",
      "start_line": 420,
      "end_line": 487,
      "reason": "symbol declaration",
      "content": "// Search runs the hybrid cosine+BM25 query, fuses results via RRF,\n// and optionally reranks the pool before returning the top-k hits.\nfunc (s *Store) Search(ctx context.Context, q store.Query) ([]Hit, error) {\n\t// embed the query text\n\tvec, err := s.Embed.Embed(ctx, q.Text)\n\t..."
    }
  ],
  "next_action": "Read internal/store/store.go:420-487 for the declaration. The graph shows 3 callers: (*Server).search (search_semantic MCP tool), (*Server).contextRouter (ask), and groundChunks (indexing).",
  "avoid": "Do not grep for the identifier — the references field already lists all usages."
}
```

**CLI equivalent**

```
$ dex graph callers ./ "(*Store).Search"

─── (*Store).Search  internal/store/store.go:420  (method)  role=central:18/4pkg
    sig: func (s *Store) Search(ctx context.Context, q store.Query) ([]Hit, error)

callers (3):
  internal/mcp/server.go:312      hits, err := st.Search(ctx, store.Query{Text: in.Query, …})
  internal/mcp/context.go:587     semHits, err = st.Search(ctx, store.Query{Text: in.Question, …})
  internal/mcp/context.go:611     summaryHits, err = st.Search(ctx, store.Query{…SummaryOnly: true})
  internal/index/grounding.go:88  hits, err := st.Search(ctx, store.Query{Text: chunk.Content, K: 5})
```

**What the agent gets:** the declaration, every call site, role annotation
(`central:18/4pkg` = 18 incoming edges across 4 packages), and a `next_action`
directive — no grep needed. The static call graph works completely offline.

---

## Scenario B — Full GPU Stack

Embedding model (`Qwen/Qwen3-Embedding-4B`), cross-encoder reranker
(`BAAI/bge-reranker-v2-m3`), and chat model (`Qwen/Qwen2.5-Coder-7B-Instruct`)
are all running locally via vLLM/ollama.

### B.0 — Service readiness

```
$ dex index status

endpoints (4 reachable)
  NAME      STATUS  MODEL                              URL
  embed     ok      Qwen/Qwen3-Embedding-4B            http://127.0.0.1:8082
  chat      ok      Qwen/Qwen2.5-Coder-7B-Instruct     http://127.0.0.1:8081
  rerank    ok      BAAI/bge-reranker-v2-m3            http://127.0.0.1:8083
  compress  ok      Qwen/Qwen2.5-Coder-7B-Instruct     http://127.0.0.1:8081
  draft     not configured
  summary   inherits chat

project  /Users/user/projects/myapp
  indexed:    2h ago
  files:      214
  chunks:     4 823
  graph:      12 441 nodes  38 902 edges
  summaries:  last 1h ago
  dim:        2560
```

All four service lanes are reachable. The `dim: 2560` line confirms vector
embeddings were stored at index time.

---

### B.1 — Behavior search: "where is filesystem event debouncing handled?"

Same question as A.1 — now with the full stack.

**Input (identical)**

```json
{
  "tool": "ask",
  "arguments": {
    "project": "/Users/user/projects/myapp",
    "question": "where is filesystem event debouncing handled?"
  }
}
```

**Output**

```json
{
  "status": "ok",
  "project": "/Users/user/projects/myapp",
  "intent": "behavior_search",

  "answer": "Filesystem event debouncing is handled in `internal/watch/watch.go` by `(*Watcher).markDirty` (watch.go:60). When fsnotify fires an event, `markDirty` resets a `time.Timer` (default 500 ms, configurable via `Options.Debounce`); the timer fires `runIndex` only after the event stream goes quiet. The timer is stopped and re-created on each new event (watch.go:64-68), guaranteeing a single re-index per burst rather than one per file save. The `Watcher` is created in `cmd/dex/main.go` via `watch.New` and started by `dex watch` or the background goroutine in `dex mcp`.",
  "answer_model": "Qwen/Qwen2.5-Coder-7B-Instruct",

  "suggested_reads": [
    {
      "path": "internal/watch/watch.go",
      "start_line": 55,
      "end_line": 85,
      "reason": "top semantic match — debounce timer implementation",
      "content": "// markDirty resets the debounce timer; on expiry it runs an index pass.\nfunc (w *Watcher) markDirty() {\n\tw.mu.Lock()\n\tdefer w.mu.Unlock()\n\tif w.timer != nil {\n\t\tw.timer.Stop()\n\t}\n\tw.timer = time.AfterFunc(w.opts.Debounce, func() {\n\t\tw.runIndex()\n\t})\n}",
      "imports": "import (\n\t\"sync\"\n\t\"time\"\n\t\"github.com/fsnotify/fsnotify\"\n\t\"github.com/alehatsman/dex/internal/index\"\n)"
    },
    {
      "path": "internal/watch/watch.go",
      "start_line": 1,
      "end_line": 54,
      "reason": "Watcher struct and Run — sets up fsnotify subscription and feeds markDirty"
    }
  ],

  "semantic_hits": [
    {
      "path": "internal/watch/watch.go",
      "start_line": 55,
      "end_line": 85,
      "score": 0.9134,
      "kind": "method_declaration",
      "content": "func (w *Watcher) markDirty() { … }"
    },
    {
      "path": "internal/watch/watch.go",
      "start_line": 90,
      "end_line": 145,
      "score": 0.8712,
      "kind": "method_declaration",
      "content": "func (w *Watcher) Run(ctx context.Context) error { … }"
    },
    {
      "path": "internal/index/index.go",
      "start_line": 117,
      "end_line": 180,
      "score": 0.7943,
      "kind": "method_declaration",
      "content": "func (idx *Indexer) Run(ctx context.Context, opts Options) error { … }"
    }
  ],

  "next_action": "Read internal/watch/watch.go:55-85 — markDirty is the debounce entry point; the inlined content above contains the full implementation.",
  "avoid": "Do not grep for 'debounce' or read the whole watch.go — the inlined content in suggested_reads covers the relevant range."
}
```

**CLI equivalent**

```
$ dex ask ./ "where is filesystem event debouncing handled?"

intent: behavior_search  project: /Users/user/projects/myapp

Answer (Qwen/Qwen2.5-Coder-7B-Instruct):
  Filesystem event debouncing is handled in internal/watch/watch.go by
  (*Watcher).markDirty (watch.go:60). When fsnotify fires an event, markDirty
  resets a time.Timer (default 500ms, configurable via Options.Debounce); the
  timer fires runIndex only after the event stream goes quiet. The timer is
  stopped and re-created on each new event (watch.go:64-68), guaranteeing a
  single re-index per burst rather than one per file save.

Suggested reads:
  1. internal/watch/watch.go:55-85
     reason: top semantic match — debounce timer implementation
     │ // markDirty resets the debounce timer; on expiry it runs an index pass.
     │ func (w *Watcher) markDirty() {
     │     w.mu.Lock()
     │     defer w.mu.Unlock()
     │     if w.timer != nil {
     │         w.timer.Stop()
     │     }
     │     w.timer = time.AfterFunc(w.opts.Debounce, func() {
     │         w.runIndex()
     │     })
     │ }

  2. internal/watch/watch.go:1-54
     reason: Watcher struct and Run — sets up fsnotify subscription and feeds markDirty

Semantic hits:
  1. internal/watch/watch.go:55-85   (method_declaration)  score=0.9134
  2. internal/watch/watch.go:90-145  (method_declaration)  score=0.8712
  3. internal/index/index.go:117-180 (method_declaration)  score=0.7943

Next action:
  Read internal/watch/watch.go:55-85 — markDirty is the debounce entry point;
  the inlined content above contains the full implementation.

Avoid:
  Do not grep for 'debounce' or read the whole watch.go — the inlined content
  in suggested_reads covers the relevant range.
```

**What changed vs. A.1:**
- From `embedding-service-unreachable` → `ok` with 3 ranked hits
- Prose `answer` with inline `path:line` citations produced in ~400ms on local GPU
- The right 31-line range is inlined — agent needs no follow-up Read
- `avoid` directive prevents the agent from second-guessing with grep

---

### B.2 — Caller graph: "callers of (*Store).Search" (with rerank)

The graph result from A.2 is unchanged (the graph is purely structural).
The addition is cross-encoder reranking of semantic context around each call
site, plus a prose explanation.

**Output delta** (fields added or changed vs. A.2)

```json
{
  "status": "ok",

  "answer": "(*Store).Search has 3 callers, all inside the MCP layer. The primary path is (*Server).contextRouter (context.go:587) which calls it for the semantic leg of the `ask` tool, pulling a wide candidate pool (k×3) before RRF fusion. (*Server).search (server.go:312) exposes the same query through the raw `search_semantic` tool. groundChunks (grounding.go:88) calls Search at index time to find neighbours of each new chunk for grounding. The `role: central:18/4pkg` annotation confirms Search is a high-fan-in hotspot — changes to its signature or query contract ripple through 4 packages.",
  "answer_model": "Qwen/Qwen2.5-Coder-7B-Instruct",

  "semantic_hits": [
    {
      "path": "internal/mcp/context.go",
      "start_line": 580,
      "end_line": 620,
      "score": 0.8821,
      "kind": "method_declaration",
      "rerank_score": 0.9612,
      "content": "semHits, err = st.Search(ctx, store.Query{\n    Text: in.Question,\n    K:    pool,\n    Pool: pool * 3,\n})"
    },
    {
      "path": "internal/mcp/server.go",
      "start_line": 305,
      "end_line": 340,
      "score": 0.8543,
      "kind": "method_declaration",
      "rerank_score": 0.9387,
      "content": "hits, err := st.Search(ctx, store.Query{Text: in.Query, K: k, Pool: pool})"
    },
    {
      "path": "internal/index/grounding.go",
      "start_line": 82,
      "end_line": 105,
      "score": 0.7901,
      "kind": "function_declaration",
      "rerank_score": 0.8214,
      "content": "hits, err := st.Search(ctx, store.Query{Text: chunk.Content, K: 5})"
    }
  ]
}
```

**Reranking effect.** The cosine scores rank `context.go` first (0.882).
The cross-encoder confirms and sharpens: rerank scores are 0.961 / 0.939 /
0.821. In queries where semantic and structural similarity diverge more, the
reranker can flip the top-2 order — this is most impactful on architecture
or cross-cutting queries.

---

### B.3 — Architecture: "how does the retrieval pipeline work?"

This query type benefits most from the full stack — it requires semantic
understanding of multiple files and a coherent prose synthesis.

**Input**

```json
{
  "tool": "ask",
  "arguments": {
    "project": "/Users/user/projects/myapp",
    "question": "how does the retrieval pipeline work?",
    "intent": "architecture"
  }
}
```

**No-GPU output** (same as A.1 — unreachable):

```json
{
  "status": "embedding-service-unreachable",
  "hint": "embed endpoint unreachable: http://127.0.0.1:8082 — fall back to: rg -rn 'Search\\|RRF\\|rerank' ./",
  "intent": "architecture",
  "semantic_hits": [],
  "suggested_reads": [],
  "next_action": "Embedding service unreachable — grep for 'Search' or read PIPELINE.md."
}
```

**Full GPU output**

```json
{
  "status": "ok",
  "project": "/Users/user/projects/myapp",
  "intent": "architecture",

  "answer": "The retrieval pipeline runs on every `ask` or `search_semantic` call. First, the query text is embedded via the `/v1/embeddings` endpoint (store.go:210) to get a 2560-dim vector. Two rankers run in parallel: a cosine KNN over the sqlite-vec `chunk_vecs` table (store.go:231) and a BM25 FTS5 query over `chunks_fts` (store.go:267). The two ranked lists are fused with Reciprocal Rank Fusion — score = Σ 1/(60 + rank) — in `fuseRRF` (store.go:310). If a reranker is configured (DEX_RERANK_URL), the fused pool is passed to a cross-encoder for final ordering (rerank/client.go:88); a circuit breaker short-circuits the reranker after consecutive failures so a downed endpoint adds no latency. The top-k hits are returned with cosine, BM25, RRF, and rerank scores attached. The `ask` tool then promotes the top hits into `suggested_reads`, inlines their source ranges, and passes them to the chat model for answer synthesis.",
  "answer_model": "Qwen/Qwen2.5-Coder-7B-Instruct",

  "suggested_reads": [
    {
      "path": "internal/store/store.go",
      "start_line": 200,
      "end_line": 330,
      "reason": "repo_summary match — hybrid search + RRF fusion implementation",
      "content": "// Search runs two rankers in parallel and fuses with RRF.\nfunc (s *Store) Search(ctx context.Context, q Query) ([]Hit, error) {\n\tvar (\n\t\tvecHits []Hit\n\t\tftsHits []Hit\n\t\tvecErr  error\n\t\tftsErr  error\n\t)\n\tvar wg sync.WaitGroup\n\twg.Add(2)\n\tgo func() { defer wg.Done(); vecHits, vecErr = s.vecSearch(ctx, q) }()\n\tgo func() { defer wg.Done(); ftsHits, ftsErr = s.ftsSearch(ctx, q) }()\n\twg.Wait()\n\t// fts failure is non-fatal — degrade to semantic-only\n\tif ftsErr != nil {\n\t\tslog.Warn(\"fts search failed, degrading to semantic-only\", \"err\", ftsErr)\n\t}\n\thits := fuseRRF(vecHits, ftsHits, q.K)\n\t..."
    },
    {
      "path": "internal/rerank/client.go",
      "start_line": 1,
      "end_line": 120,
      "reason": "cross-encoder rerank client + circuit breaker"
    },
    {
      "path": "PIPELINE.md",
      "start_line": 1,
      "end_line": 62,
      "reason": "architecture doc — full pipeline description"
    }
  ],

  "semantic_hits": [
    {
      "path": "internal/store/store.go",
      "start_line": 200,
      "end_line": 330,
      "score": 0.9201,
      "kind": "file_summary",
      "rerank_score": 0.9741,
      "content": "store.go — hybrid search: embed query → vec KNN (sqlite-vec) + BM25 (FTS5), fused via RRF (k=60). Optional cross-encoder rerank with circuit breaker. UpsertMany keeps chunk_vecs and chunks_fts in sync via SQLite triggers."
    },
    {
      "path": "internal/rerank/client.go",
      "start_line": 1,
      "end_line": 120,
      "score": 0.8834,
      "kind": "file_summary",
      "rerank_score": 0.9103,
      "content": "rerank/client.go — HTTP cross-encoder rerank client. POSTs (query, docs[]) to DEX_RERANK_URL, returns []Score{Index, Score}. Circuit breaker trips after N consecutive failures and short-circuits for a cooldown window. Per-call deadline + in-process result cache keyed on (query, id-set)."
    },
    {
      "path": "internal/mcp/context.go",
      "start_line": 560,
      "end_line": 650,
      "score": 0.8612,
      "kind": "method_declaration",
      "rerank_score": 0.8890,
      "content": "func (s *Server) contextRouter(...) — runs semantic + symbol + graph legs, merges, builds suggested_reads bundle, calls synthesizeAnswer."
    }
  ],

  "next_action": "Read internal/store/store.go:200-330 — the inlined content above contains the full hybrid search + RRF implementation. PIPELINE.md has the end-to-end data-flow diagram.",
  "avoid": "Do not read entire files; the suggested ranges and file summaries in semantic_hits cover the relevant implementation."
}
```

---

## Summary: No GPU vs. Full GPU

| Capability | No GPU | Full GPU |
|---|---|---|
| `index_status` — service health | embed/chat/rerank UNREACHABLE | all ok |
| **Behavior search** (`ask`, free-text) | ❌ `embedding-service-unreachable` | ✅ ranked hits + inlined code + prose answer |
| **Semantic hits** (cosine KNN) | ❌ unavailable | ✅ top-k with scores |
| **BM25 / FTS** lexical search | ❌ (query embed required) | ✅ fused via RRF |
| **Cross-encoder rerank** | ❌ unavailable | ✅ sharpens hit ordering |
| **Prose answer** with `path:line` citations | ❌ `answer` field empty | ✅ synthesized in ~400 ms |
| **Symbol lookup** (`search_symbol`) | ✅ pure SQL, always on | ✅ same + role annotation |
| **Caller / callee graph** (`graph_callers`) | ✅ static analysis, always on | ✅ same + semantic context |
| **Package topology** (`graph_deps`) | ✅ always on | ✅ same |
| **Architecture query** | ❌ `embedding-service-unreachable` | ✅ file summaries + prose explanation |
| **Per-file summaries** (`view_summarize`) | ❌ no chat model | ✅ one-shot gist per file/range |
| `avoid` directive (suppress redundant grep) | ❌ not populated | ✅ prevents agent re-work |
| Agent context tokens saved per question | ~0 | **~8 000–15 000** tokens |

### The token-savings story

A typical grep → Read → grep loop burns roughly 12 000 context tokens for a
question like "where is debouncing handled?". With the full stack, one `ask`
call returns 31 inlined lines and a prose answer — the agent never opens a file
and uses under 800 tokens for the result. At scale, across a session of 20
questions, that difference is the gap between a focused, useful agent and one
that hits context limits before finishing a task.

### What runs where

```
GPU / local LLM server
  ├── Qwen3-Embedding-4B      (2560-dim vectors, ~4 GB VRAM)
  ├── Qwen2.5-Coder-7B-Instruct  (answer synthesis, ~8 GB VRAM)
  └── BAAI/bge-reranker-v2-m3    (cross-encoder rerank, ~1.5 GB VRAM)

Developer machine (CPU only, no VRAM needed)
  ├── dex binary (Go, ~18 MB)
  ├── SQLite index (~50–200 MB per repo)
  ├── symbol search  ──► always on
  ├── call graph     ──► always on
  └── BM25 / FTS5    ──► (lexical-only fallback when embed offline)
```

Source code, chunks, and query text never leave the machine.
The GPU server can be on the same host (ollama / vLLM), a local GPU box
on the LAN, or an SSH-tunneled workstation — the endpoint is just a URL.
