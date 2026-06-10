# dex vision — the capability ladder

The frame behind the roadmap. Where `agent-roadmap.md` is a ranked checklist
of *what* to build next, this doc is the *why*: the single principle that ties
dex's three audiences together and tells you which rung any given piece of work
serves.

This is a strategy doc, not a spec. Specs (`specs/*.md`) state behavior held
true today; this states the direction. When a tension surfaces between rungs,
this doc is where the trade-off is argued.

---

## The one frame

dex today is three engines bolted together:

- a **retrieval core** — semantic + symbol + graph lanes, hybrid RRF fusion,
  cross-encoder rerank;
- a **context optimizer** — the compress engine, context profiles, the
  cross-turn dedup ledger;
- an **agent surface** — `ask`, the `ctx_*` tools, the coordination bus.

The constitution (`specs/constitution.md`) already commits to the principle
that unifies them: **fail soft, never break.** A degraded embedding backend
falls to BM25; a downed reranker falls to RRF order; a missing chat leg falls
to an evidence bundle. dex degrades down a ladder rather than off a cliff.

The three questions we keep asking — *best for Claude?*, *useful for a local
agent?*, *useful with no GPU?* — are **not three roadmaps. They are three rungs
of one ladder.** Each rung is a graceful degradation of the one above. The
vision is to make every rung *good*, not merely *not-broken*.

| Rung | Consumer | Inference | Bottleneck | dex's job |
|------|----------|-----------|------------|-----------|
| **A** | Claude Code (frontier) | full GPU stack hot | token economy over a long session | max task-success **per resident token** |
| **B** | local agent (Qwen / DeepSeek, one GPU) | hot, but the *model* is weak | window wall + KV-cache VRAM + hallucination | compress harder, anchor verbatim, fit the budget |
| **C** | any box / CI / air-gapped | **none** | no semantic lane at all | stay useful on CPU + lexical + structural |

Everything below hangs off this table.

---

## Rung A — maximize usefulness for Claude

Claude is not reasoning-bound. It is **token-economy-bound** over a long
session, and **precision-bound** — wrong retrieval forces an expensive
fan-out of `Read`s. Highest-leverage moves, ranked:

1. **Ship the proxy (#232).** The single biggest unshipped win. Hooks can
   compress *new* tool outputs at the `PreToolUse` redirect, but they
   structurally **cannot touch accumulated old `tool_results`** — which is
   where tokens actually pile up in a long session. The `ANTHROPIC_BASE_URL`
   proxy is the only seam that can deterministically rewrite in-flight history.
   lean-ctx already proves the pattern. Bonus: type-aware history pruning is
   **deterministic, zero LLM calls** — so it *also* serves Rung C. This is a
   posture change worth naming: dex moves from "a tool the agent calls" to
   "middleware in the request path" (sees API keys → loopback-only, no body
   logging, fail open). See the open question below.

2. **Land the eval harness (#247) — it gates everything.** Every
   retrieval-quality change in the SOTA epic (#246) is unsafe to adopt without
   a regression gate and a labeled golden set. The multi-repo corpus (#278)
   generalizes it past dex-only as an overfitting guard. **Nothing in #246
   lands before this.** It is also the only way to measure the *lower* rungs —
   how good is BM25-only? ONNX-only? Build the ruler first.

3. **Graph k-hop context expansion.** The highest *measured* retrieval gain in
   our own research, and dex already owns the graph. Pull a hit, expand along
   call/import edges with hop-decay weighting — the right neighbors without
   reading them blindly. (Honesty caveat from #246: graph absolute numbers are
   largely single-paper / own-benchmark; directional, not settled — hence #247
   first.)

4. **Prompt-cache-aware layout (#205) + progressive disclosure (#206).** A
   stable prefix → Anthropic prompt-cache hits → cheaper and faster for free.
   Skeleton-first reads with on-demand body expansion → Claude pulls bodies
   only when it decides it needs them. Both exploit Claude's strengths and are
   pure token-economy wins.

**Defer for Claude:** agentic deep mode (#255 — tens of seconds per completion,
near-zero net correctness gain) and multi-vector / MUVERA (#254 — storage tax,
gains measured on *text* IR, not code). Both correctly parked.

---

## Rung B — useful for local-only agents

This is the **LOCAL track (#158)**, currently parked, and its analysis is
right. A weak model is bound by four constraints at once that Claude is not:

1. **window size** — 8k–32k, a hard wall;
2. **KV-cache VRAM** — context length *is* GPU memory;
3. **weak reasoning** — lost-in-the-middle and noise-distraction far worse;
   ambiguity → hallucinated paths / APIs;
4. **prefill latency** — tokens = wall-clock on local hardware.

These change *what* you preserve and *how hard* you compress:

- **Anchor-verbatim safety is non-negotiable.** Paths, identifiers, line
  numbers, and types the model is about to edit must never be lossy — weak
  models substitute plausible-wrong tokens when these are compressed. Promote
  this the moment the track activates.
- **Use the local model's actual tokenizer.** `token_reduce.go` substitution
  rules are o200k-specific; a swap that saves on o200k can *cost* tokens on
  Qwen / DeepSeek / Llama BPE. The `target_model` profile dimension (#204) is
  the enabler that makes this possible — land it before anything else here.
- **Deterministic beats clever.** The symbol-map legend (`symmap.go`)
  indirection probably *confuses* a 7B — measure, then likely disable for weak
  targets.

The compelling story: **dex + a local Qwen3 stack (#256) = a fully offline
coding agent**, no cloud in the loop at all. A genuinely differentiated
position. The Qwen3 reranker upgrade alone (41 → 81 MTEB-Code, ~2× on code
retrieval, same VRAM slot) is the highest-ROI model change and helps Rungs A
*and* B.

---

## Rung C — useful with no GPU

The rung with the most upside and the least coverage. Today BM25 + symbol +
graph all work with zero inference (the constitution guarantees it); the
semantic lane simply dies to BM25. The pieces to make Rung C *good* exist but
**no epic frames them as a deployment mode**:

- **ONNX in-process embedder (#180) is unblocked.** The `Embedder` interface
  (#179) landed (`internal/embed/client.go:31`). A `-tags onnx` build embeds on
  CPU (e.g. bge-small, 384-dim, or a small code embedder), restoring the
  semantic lane everywhere — laptops, CI, containers, air-gapped boxes. The
  namespace hazard is already solved: ONNX reports a distinct `ModelName()`
  (`onnx:bge-small:384`), which trips the existing `ErrEmbedModelMismatch` gate,
  so vectors never mix across engines. No hot-swap mid-index (incoherent —
  different vector space); it is a *static, configured* engine choice, which is
  correct.
- **Matryoshka dim truncation (#246 item 3) pairs perfectly** — small dims mean
  cheap CPU inference and small sqlite-vec storage. Composes with int8
  quantization (#215).
- **The proxy's history pruning (#232) is GPU-free** — deterministic structural
  compression. Rung C still gets context optimization, not just retrieval.

**The framing that's missing.** dex today reads as an *infra project* — it
needs ollama, a GPU, config. The "lean" intuition is that dex should *also* be
a **drop-in tool you `go install` and it Just Works** with zero inference
stack. That is the adoption lever. Proposed: a new epic — **"lean profile —
zero-infra dex"** — treating CPU/ONNX + BM25 + graph as a *first-class
deployment target*, not a fallback. Honesty caveat: the default build stays
dep-free per #67 (ONNX stays behind the build tag, like `sqlite_fts5`); the
lean profile is a documented build, never the silent default. If we cannot keep
the default build dep-free, we stop and re-open #67 explicitly.

---

## Cross-cutting: do we need tool profiles? (#283)

The skepticism in #283 is partly right. Today the tiers (`ask` / `standard` /
`power`) split on **tool count** — a thin surface so Claude sees one tool, not
thirty. That rationale is sound (the tool-description block costs tokens, and a
long menu induces choice paralysis). But splitting on *count* is the wrong
axis. The right axis is **the capability ladder itself**:

- A no-GPU deployment (Rung C) physically *cannot* offer `search_semantic` with
  no embedder wired — the surface should reflect what's actually reachable.
- A weak local model (Rung B) wants *fewer* tools because it can't handle
  choice — a `target_model` concern, not a separate tier system.
- Claude (Rung A) wants the thin `ask`-first surface.

Direction: **the concept is right, the three-tier taxonomy is
over-engineered.** Tool exposure should be *derived* from (a) which services
are reachable and (b) the `target_model` profile — not a hand-maintained
ask/standard/power split. That deletes a knob and makes the surface honest
about the rung it's running on. Check telemetry first: is anyone running
`power`?

---

## Recommended sequencing

The critical path is short and the dependencies are real:

1. **#247 eval harness** (+ #278 corpus) — gates all of #246, and is the only
   way to measure each rung. Build the ruler first.
2. **#204 `target_model` profile** — the enabler for Rung B *and* the honest
   basis for resolving #283.
3. **#232 proxy spike (#235)** — measure tokens-saved on a real session before
   investing past the spike. Serves Rungs A and C at once.
4. **#256 Qwen3 reranker** — cheap, highest-ROI quality bump; helps A and B.
5. Then **graph k-hop expansion** and a **"lean profile" epic** (#180 ONNX +
   Matryoshka + lean docs) in parallel.

---

## Open strategic questions

1. **Proxy posture (#232).** The proxy is middleware in the request path — it
   sees API keys and must fail open. Do we want dex to go there, or stay a pure
   MCP tool and accept that old `tool_results` remain uncompressed? This is the
   one compression lever the engine cannot otherwise pull.
2. **Lean / zero-infra epic.** It does not exist yet. Is "dex you `go install`
   and it Just Works with no GPU" the thread to pull (the lean instinct), or is
   the token-economy work for Claude the priority?

_Source: tool-vision brainstorm, 2026-06-11 (issue #288). Cross-references the
SOTA retrieval epic (#246), the compress tracks (#157 CLAUDE / #158 LOCAL), the
proxy epic (#232), and the degradation/ONNX epic (#174)._
