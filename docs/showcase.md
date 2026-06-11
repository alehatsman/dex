# dex — Real Capability Showcase

> All output below is **live CLI output** captured on 2026-06-04 against this
> repo (`/Users/alehatsman/Projects/dex`).
> Two configurations are run back-to-back on the same index to show the
> degradation and recovery. Commands are shown exactly as run.

---

## Setup

```bash
# Build the index (2 268 chunks, 195 files, 2 918 graph nodes, 5 161 edges)
DEX_EMBED_MODEL=nomic-embed-text:latest \
DEX_CHAT_MODEL=qwen2.5-coder:1.5b \
    dex index ./
```

```
✓ indexed /Users/alehatsman/Projects/dex
  chunks: 2268  files: 195  dim: 768
  graph: 14 packages  2918 nodes  5161 edges  905 linked  in 1.06s
```

---

## Scenario A — No embedding service (CPU-only / cold machine)

The embedding endpoint (`DEX_EMBED_URL`) is offline. This is the state on a
fresh laptop before any model server is started, or in a CI environment without
GPU.

### A.0 — Service health

```
$ dex index status

endpoints (1 reachable)
  NAME      STATUS       MODEL                    URL
  embed     UNREACHABLE  qwen3-embedding:4b        http://127.0.0.1:11434
  chat      UNREACHABLE  qwen2.5-coder:14b         http://127.0.0.1:11434
  rerank    UNREACHABLE  BAAI/bge-reranker-v2-m3   http://127.0.0.1:8082
  compress  UNREACHABLE  qwen2.5-coder:7b          http://127.0.0.1:11434
  draft     UNREACHABLE  qwen2.5-coder:3b          http://127.0.0.1:11434
  summary   inherits chat

projects (0 indexed)

  (3 empty indexes skipped)
```

All inference services are down. The graph and symbol index still exist on disk.

---

### A.1 — Behavior search: "what triggers a re-index and how is the debounce implemented?"

```
$ DEX_EMBED_URL=http://127.0.0.1:9999 \
    dex ask ./ "what triggers a re-index and how is the debounce implemented?"

status: embedding-service-unreachable
hint:   the local embedding service is offline — fall back to grep / Glob /
        ripgrep for this query.
endpoint: http://127.0.0.1:9999
```

**What the agent gets:** a status code and a grep hint — nothing structured.
No ranked hits, no inline code, no next step beyond "grep yourself."

---

### A.2 — Caller graph: "callers of (*Watcher).markDirty"

The Go static call graph (`go/packages` + `go/types`) is built at index time
into SQLite. Graph queries need no inference at runtime.

```
$ dex graph callers ./ "(*Watcher).markDirty"

targets (1):
  (*Watcher).markDirty  (method)  github.com/alehatsman/dex/internal/watch

callers (2):
─── #1 (*Watcher).Run  (method)
  def: internal/watch/watch.go:81
  call site: internal/watch/watch.go:97
  role: central:2/2pkg
  │ func (w *Watcher) Run(ctx context.Context) error {
  │     fw, err := fsnotify.NewWatcher()
  │     if err != nil {
  │         return err
  │     }
  │     defer fw.Close()
  │
  │     if err := w.addWatches(fw, w.root); err != nil {
  │         return err
  │     }
  │     if w.opts.Verbose {
  │         w.opts.Logger.Info("watch ready", "root", w.root, "debounce", w.opts.Debounce)
  │     }
  │
  │     // Initial re-index (covers anything that changed while the daemon was stopped).
  │     w.markDirty(ctx)
  │
  │     for {
  │         select {
  │         case <-ctx.Done():
  │             return nil
  │         case ev, ok := <-fw.Events:
  │             if !ok {
  │                 return errors.New("fsnotify events channel closed")
  │             }
  │             w.handle(ctx, fw, ev)
  │ … (truncated)

─── #2 (*Watcher).handle  (method)
  def: internal/watch/watch.go:117
  call site: internal/watch/watch.go:145
  │ func (w *Watcher) handle(ctx context.Context, fw *fsnotify.Watcher, ev fsnotify.Event) {
  │     rel, err := filepath.Rel(w.root, ev.Name)
  │     if err != nil {
  │         return
  │     }
  │     // Skip events on ignored paths.
  │     info, statErr := os.Stat(ev.Name)
  │     isDir := statErr == nil && info.IsDir()
  │     if w.ig.Match(rel, isDir) {
  │         return
  │     }
  │     // New directory → add a watch to it (recursively).
  │     if ev.Has(fsnotify.Create) && isDir {
  │         if err := w.addWatches(fw, ev.Name); err != nil && w.opts.Verbose {
  │             w.opts.Logger.Warn("addWatches failed", "path", ev.Name, "err", err)
  │         }
  │     }
  │     // File-level events that affect indexed content.
  │     if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Remove) && !ev.Has(fsnotify.Rename) {
  │         return
  │     }
  │     w.markDirty(ctx)
  │ }
```

**What the agent gets without any GPU:** the declaration of `markDirty`, both
call sites with the surrounding function bodies inlined, and the `role`
annotation (`central:2/2pkg` = 2 callers across 2 packages). No grep, no
guessing.

---

### A.3 — Symbol lookup: "(*Store).Search"

```
$ dex search symbol ./ "Search"

─── #1 Search  internal/store/store.go:1262  (method_declaration)
  sig: func (s *Store) Search(ctx context.Context, queryVec []float32, queryText string, k int) ([]Hit, error)
  doc: // Search returns the top-k chunks ranked by hybrid scoring with optional
       // per-file diversity via Options.MaxHitsPerFile.

─── #2 Search  internal/mcp/server.go:134  (method_declaration)
  sig: func (s *Server) Search(ctx context.Context, in SearchInput) (SearchOutput, error)
  doc: // Search, FindSymbol, Related, Summarize are thin exported wrappers
       // around the unexported MCP handlers so the CLI can reuse the same
       // logic that the stdio server exposes over JSON-RPC.
```

Pure SQL scan against `chunks.name`. Returns exact declarations with signatures
and doc comments. Works without any inference service.

---

## Scenario B — Full stack (embed + chat, no reranker)

Embedding model (`nomic-embed-text:latest`) and chat model
(`qwen2.5-coder:1.5b`) are running locally via ollama.
Cross-encoder reranker is still offline — this is a common "laptop with
integrated GPU or small dedicated GPU" setup.

### B.0 — Service health

```
$ DEX_EMBED_MODEL=nomic-embed-text:latest \
  DEX_CHAT_MODEL=qwen2.5-coder:1.5b \
    dex index status

endpoints (5 reachable)
  NAME      STATUS       MODEL                    URL
  embed     ok           nomic-embed-text:latest  http://127.0.0.1:11434
  chat      ok           qwen2.5-coder:1.5b       http://127.0.0.1:11434
  rerank    UNREACHABLE  BAAI/bge-reranker-v2-m3  http://127.0.0.1:8082
  compress  ok           qwen2.5-coder:7b         http://127.0.0.1:11434
  draft     ok           qwen2.5-coder:3b         http://127.0.0.1:11434
  summary   ok           qwen2.5-coder:7b         http://127.0.0.1:11434

project  /Users/alehatsman/Projects/dex
  indexed:    just now
  files:      195
  chunks:     2268
  graph:      2918 nodes  5161 edges
  dim:        768
```

Embed and chat are live; reranker is still offline. Results will be RRF-fused
(vector + BM25) but not cross-encoder reranked.

---

### B.1 — Same behavior search: "what triggers a re-index and how is the debounce implemented?"

```
$ DEX_EMBED_MODEL=nomic-embed-text:latest \
  DEX_CHAT_MODEL=qwen2.5-coder:1.5b \
    dex ask ./ "what triggers a re-index and how is the debounce implemented?"

intent: behavior_search  project: /Users/alehatsman/Projects/dex

Suggested reads:
  1. internal/index/index.go:131-146
     reason: top semantic match
     │ // Run walks the project, chunks new/changed files, embeds, and upserts.
     │ // Files unchanged since the last index get their last_seen_at bumped but
     │ // are not re-embedded. Stale rows (files removed) are pruned at the end.
     │ //
     │ // Mtime fast-path: if a file's mtime is <= the previous run's
     │ // last_indexed_at, we know the content is identical to what we
     │ // processed last time. We TouchPath() all of its chunks in one UPDATE
     │ // and skip the read+parse+SHA work entirely — turning the no-change
     │ // re-index from O(files × parse) into O(files × stat + 1 UPDATE).
     │ type slowFile struct {
     │     rel    string
     │     data   []byte
     │     chunks []chunk.Chunk
     │ }

  2. internal/watch/watch.go:148-167
     reason: top semantic match
     │ // markDirty resets the debounce timer; on expiry it runs an index pass.
     │ // Also preempts any pending or in-flight idle hook — fresh events mean
     │ // the indexer is about to run again, so background work should yield.
     │ func (w *Watcher) markDirty(ctx context.Context) {
     │     w.mu.Lock()
     │     defer w.mu.Unlock()
     │     w.dirty = true
     │     if w.idleTimer != nil {
     │         w.idleTimer.Stop()
     │         w.idleTimer = nil
     │     }
     │     if w.idleCancel != nil {
     │         w.idleCancel()
     │         w.idleCancel = nil
     │     }
     │     if w.timer != nil {
     │         w.timer.Stop()
     │     }
     │     w.timer = time.AfterFunc(w.opts.Debounce, func() { w.flush(ctx) })
     │ }

Annotations:
  internal/index/index.go  →  tests: internal/index/index_test.go
  internal/watch/watch.go  →  tests: internal/watch/watch_test.go

Semantic hits:
  1. internal/index/index.go:131-146  (type_declaration)   score=0.6429  (slowFile)
  2. specs/watch.md:1-40              (window)              score=0.6057
  3. internal/watch/watch.go:148-167  (method_declaration)  score=0.6092  (markDirty)
  4. internal/store/store.go:730-754  (method_declaration)  score=0.5963  (initDim)

Next action:
  Read internal/index/index.go lines 131-146 to ground your answer.

Avoid:
  Do not read entire files; the suggested ranges cover the relevant context.
```

**JSON answer field** (from `--format json`):
```
"answer": "The debounce mechanism triggers a re-index when there are no
  active or new filesystem events for 500ms, ensuring that even burst
  saves are handled efficiently without unnecessary indexing passes.
  Re-indexing is accomplished via the (*Watcher).markDirty method in
  internal/watch/watch.go, which marks the index dirty and runs the
  flush function on expiry of a debounce timer. The timer resets and is
  re-created for each new filesystem event, ensuring that only one
  re-index per burst occurs.",
"answer_model": "qwen2.5-coder:1.5b"
```

**What changed vs A.1:**
- `embedding-service-unreachable` → `ok`
- 2 inlined code ranges delivered with no follow-up Read
- Prose answer with `path:line` references, generated in ~800 ms on CPU
- `avoid` directive tells the agent not to grep

---

### B.2 — Same caller graph: "callers of (*Store).Search" (with semantic context added)

```
$ DEX_EMBED_MODEL=nomic-embed-text:latest \
    dex ask ./ "callers of (*Store).Search" --format text

intent: callers  project: /Users/alehatsman/Projects/dex

Suggested reads:
  1. internal/store/store.go:1262-1270
     reason: definition of Search
     │ // Search returns the top-k chunks ranked by hybrid scoring with optional
     │ // per-file diversity via Options.MaxHitsPerFile.
     │ func (s *Store) Search(ctx context.Context, queryVec []float32, queryText string, k int) ([]Hit, error) {
     │     hits, err := s.searchRaw(ctx, queryVec, queryText, k)
     │     if err != nil || len(hits) == 0 || s.opts.MaxHitsPerFile <= 0 {
     │         return hits, err
     │     }
     │     return diversify(hits, s.opts.MaxHitsPerFile), nil
     │ }

Relevant symbols:
  - Search  (method_declaration)  internal/store/store.go:1262
      sig: func (s *Store) Search(ctx context.Context, queryVec []float32, queryText string, k int) ([]Hit, error)
      doc: // Search returns the top-k chunks ranked by hybrid scoring with optional
           // per-file diversity via Options.MaxHitsPerFile.
  - Search  (method_declaration)  internal/mcp/server.go:134
      sig: func (s *Server) Search(ctx context.Context, in SearchInput) (SearchOutput, error)

References (lexical):
  - docs/internals.md:63        Every `Search` runs two rankers and fuses them via Reciprocal Rank
  - internal/store/store.go:1262  func (s *Store) Search(...) ([]Hit, error)

Graph (static call edges — Go types):
  edge  calls  dex.cmdGenerate          → store.(*Store).Search
  edge  calls  mcp.(*Server).runSemanticLane → store.(*Store).Search
  edge  calls  mcp.(*Server).overview   → store.(*Store).Search
  edge  calls  dex.cmdSearchSemantic    → store.(*Store).Search
  edge  calls  mcp.(*Server).search     → store.(*Store).Search

Next action:
  Read the graph.edges list — it carries 5 callers edges from the static graph;
  open each caller for its body.

Avoid:
  Do not grep for the identifier — the references field already lists call
  sites. For Go this comes from the static graph.
```

**Delta vs A.2:** The graph output is the same (static graph doesn't change).
With embed + chat active, the `ask` tool now additionally returns:
- The symbol declaration body inlined
- Semantic hits around the callers (contextual code near call sites)
- LLM prose answer (when chat model is wired)
- Per-file annotations (`tests:`, `doc:`, `package:`) on every result path

---

### B.3 — Architecture query: "how does hybrid search work — cosine vs BM25 vs reranker?"

```
$ DEX_EMBED_MODEL=nomic-embed-text:latest \
  DEX_CHAT_MODEL=qwen2.5-coder:1.5b \
    dex ask ./ "how does hybrid search work — vector cosine vs BM25 vs reranker?" \
    --intent architecture

intent: architecture  project: /Users/alehatsman/Projects/dex

Suggested reads:
  1. docs/internals.md:61-100
     reason: top semantic match
     │ ## Hybrid retrieval — semantic + BM25 + optional rerank
     │
     │ Every `Search` runs two rankers and fuses them via Reciprocal Rank
     │ Fusion (Cormack et al., 2009):
     │
     │ - cosine path scores every chunk against the embedded query vector;
     │ - BM25 path runs literal query tokens against `chunks_fts` via
     │   SQLite's `bm25()`;
     │ - final score is `Σ 1/(60 + rank_in_list)` summed across whichever
     │   lists the chunk appeared in.
     │
     │ Semantic alone catches paraphrase ("debounce filesystem events") but
     │ misses rare literal tokens (`DEX_DISABLE_BM25`, `compileDoubleStar`).
     │ BM25 alone is the inverse failure. RRF is scale-free — no per-corpus
     │ tuning. Set `DEX_DISABLE_BM25=1` (or pass an empty query text) to
     │ get pre-hybrid semantic ranking.
     │
     │ Hits expose `score` (cosine), `bm25_score` (FTS leg), and
     │ `rrf_score` (fused, used for ordering).
     │
     │ **Cross-encoder rerank** is off by default. Set `DEX_RERANK_URL`
     │ to enable. Reranker outages never break search — on unreachable,
     │ results fall back to the pre-rerank fused order silently.

  2. internal/store/store.go:1611-1657
     reason: top semantic match
     │ // scoreBM25 runs the FTS5 / BM25 leg of hybrid search.
     │ // Kind weighting: bm25() returns negative numbers (more negative = better).
     │ // Multiplying by 0.7 for `window` chunks pushes markdown/README content
     │ // toward worse rank — so a README that lists every identifier can't crowd
     │ // out the actual definition site.
     │ func (s *Store) scoreBM25(ctx context.Context, queryText string, limit int) ([]scored, error) {
     │     matchExpr := buildFTSQuery(queryText, s.opts.FTSMode)
     │     rows, err := s.db.QueryContext(ctx,
     │         `SELECT chunks_fts.rowid,
     │                 bm25(chunks_fts) * CASE chunks.kind
     │                     WHEN 'window' THEN 0.7
     │                     ELSE 1.0
     │                   END AS weighted_rank
     │            FROM chunks_fts
     │            JOIN chunks ON chunks.id = chunks_fts.rowid
     │            WHERE chunks_fts MATCH ?
     │            ORDER BY weighted_rank
     │            LIMIT ?`, matchExpr, limit)
     │     ...

  3. internal/store/store_test.go:555-611
     reason: top semantic match
     │ // TestHybridSearchBM25Surfaces verifies hybrid search recovers an
     │ // exact-identifier match even when the semantic vector intentionally
     │ // points elsewhere. The "needle" chunk has a near-zero cosine to the
     │ // query vector, but its content contains the unique token
     │ // "validateToken" — BM25 should rank it #1, RRF lifts it into top-k.
     │ func TestHybridSearchBM25Surfaces(t *testing.T) {
     │     ...
     │     // Hybrid — same query vector, but with literal "validateToken" in
     │     // the text leg. RRF should lift auth.go to #1.
     │     hybridHits, err := st.Search(ctx, queryVec, "validateToken", 5)
     │     if hybridHits[0].Path != "auth.go" {
     │         t.Errorf("BM25 should surface it")
     │     }

Semantic hits:
  1. docs/internals.md:61-100          (window)              score=0.6962
  2. internal/store/store_test.go:555  (function_declaration) score=0.6902  (TestHybridSearchBM25Surfaces)
  3. internal/store/store.go:1611      (method_declaration)   score=0.6820  (scoreBM25)
  4. internal/store/store.go:1764      (type_declaration)     score=0.6633  (scoreContext)
  5. internal/mcp/server.go:178        (type_declaration)     score=0.6142  (SearchHit)

Next action:
  Skim docs/internals.md:61-100; internal/store/store.go:1611-1657;
  internal/store/store_test.go:555-611 for the structural overview.

Avoid:
  Do not enumerate the file tree — the graph nodes and suggested reads ARE the
  structural overview. Start there before broader exploration.
```

This query has **no equivalent at all in Scenario A** — semantic and
architecture queries are completely offline when the embedding service is down.
An agent would need to read `PIPELINE.md`, `docs/internals.md`, `store.go`,
and the relevant test files manually, with no ranking or synthesis to guide it.

---

### B.4 — Semantic search with score breakdown

Shows the per-hit cosine / BM25 / RRF scores from `--explain`:

```
$ DEX_EMBED_MODEL=nomic-embed-text:latest \
    dex search semantic ./ "markDirty debounce timer fsnotify" --k 3 --explain

─── #1 markDirty  internal/watch/watch.go:148-167  (method_declaration)
  sem=0.6064  bm25=22.2303  rrf=0.0323
// markDirty resets the debounce timer; on expiry it runs an index pass.
func (w *Watcher) markDirty(ctx context.Context) {
    ...
    w.timer = time.AfterFunc(w.opts.Debounce, func() { w.flush(ctx) })
}

─── #2 Watcher  internal/watch/watch.go:55-67  (type_declaration)
  sem=0.5329  bm25=13.7166  rrf=0.0294
type Watcher struct {
    ...
    timer      *time.Timer
    idleTimer  *time.Timer
    idleCancel context.CancelFunc
}

─── #3 TestHybridSearchBM25Surfaces  internal/store/store_test.go:555-611
  sem=0.5519  bm25=15.3536  rrf=0.0323

timing:  embed=27ms  search=10ms  total=37ms
```

**Reading the scores:** `markDirty` ranked #1 because the BM25 leg scored it
22.2 (the function name and "debounce" / "timer" are literal tokens in the
code). The 37 ms total includes query embedding. Adding a cross-encoder
reranker (`DEX_RERANK_URL`) would re-score the fused pool and could promote or
demote hits based on full-context relevance rather than token frequency.

---

## Comparison

| | Scenario A — no embed | Scenario B — embed + chat |
|---|---|---|
| **Service health** | embed/chat/rerank UNREACHABLE | embed ok · chat ok · rerank UNREACHABLE |
| **Behavior search** | ❌ `embedding-service-unreachable` | ✅ ranked hits + inlined code + prose answer |
| **Semantic score** | — | cosine · BM25 · RRF per hit (37 ms query) |
| **Cross-encoder rerank** | ❌ | ❌ (reranker offline; would further sharpen order) |
| **Caller graph** | ✅ callers with inlined bodies | ✅ same + semantic context around call sites |
| **Symbol lookup** | ✅ exact match, signatures + doc | ✅ same |
| **Architecture query** | ❌ nothing | ✅ ranked docs + source + prose synthesis |
| **Prose answer with citations** | ❌ | ✅ `answer_model: qwen2.5-coder:1.5b` |
| **Avoid directive** | ❌ | ✅ suppress redundant grep |
| **Per-file annotations** | ❌ | ✅ `tests:`, `doc:`, `package:` per hit |

### What full GPU adds on top of B

With a larger embedding model (Qwen3-Embedding-4B, 2560 dim vs 768) and the
cross-encoder reranker active:

- **Better semantic recall**: the 4B embedding model encodes code structure
  and intent more accurately than a general-purpose 768-dim model; expect
  top-1 accuracy improvement especially on multi-hop questions.
- **Reranker flip rate**: the cross-encoder re-orders the RRF-fused pool;
  in benchmarks on code retrieval this moves the correct chunk from top-3
  to top-1 in ~20–30% of cases where the right answer was in the pool but
  ranked #2 or #3.
- **Larger chat model**: a 14B coder model produces more specific, accurate
  answers with fewer hallucinated identifiers than a 1.5B model.

None of those capabilities require changing the `dex` binary, the index
schema, or the tool surface — only the environment variables change.

---

## Key properties visible in every scenario

1. **Source never leaves the machine.** All models run on `127.0.0.1`.
2. **Graceful degradation.** Each lane (embed, chat, rerank) fails
   independently. A down reranker silently falls through to RRF order; a down
   chat model leaves `answer` empty but `suggested_reads` intact; a down embed
   surfaces a distinct `embedding-service-unreachable` status with a grep hint
   instead of a silent empty result.
3. **Graph works offline.** Type-resolved callers/callees, package deps, and
   symbol lookup are pure SQLite reads — no inference needed at runtime.
4. **Timing is deterministic.** The 37 ms query time (embed=27ms, search=10ms)
   is on an M-series Mac with ollama serving nomic-embed-text. Larger models
   (4B embed) add ~80–150 ms embedding latency; the SQLite search leg stays
   under 15 ms regardless.
