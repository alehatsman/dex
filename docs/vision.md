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

## Two cross-cutting forces

Before the rungs, two things shape *all* of them.

### The prioritization lens: GPU scarcity

The fleet is GPU-starved — boxes struggle even to reindex or answer agent
questions. That is not just an operational footnote; it **re-ranks the
backlog**. Deterministic, zero-GPU work weights *up*, because it reduces
dependence on the scarce resource. Concretely:

- The proxy's history pruning is deterministic / zero-LLM — it serves Rung A
  *and* Rung C and costs no inference, so it leads.
- The lean rung (C) is the literal no-GPU path and is **prioritized ahead of
  the local-agent rung (B)** — a local agent still needs a GPU to run its
  model; a lean deployment needs none.
- Heavy model upgrades (a 30B chat) are **deferred** — they
  would worsen the exact contention we're hitting. Only the cheap reranker swap
  (~3 GB, ~2× on code retrieval) lands now.

### Measurement — the ruler for every rung

You cannot tune a rung you cannot measure, and the wrong instrument hides real
effects (the graph γ-sweep came back a null result because the retrieval eval
can't probe what graph expansion is *for*). dex extends one measurement family
— `dex bench {eval | compress | perf}`, all deterministic and zero-inference
by default so they run on a starved box — across the ladder:

- **eval** (NDCG/Recall/MRR over a golden set) → gates retrieval quality, all
  rungs. *Landed.*
- **compress** (ratio × fidelity × anchor-preservation, per target_model) →
  gates Rung A token-economy and Rung B weak-model hardening.
- **perf** (local-compute latency + memory, GPU-bound paths report-only) →
  gates Rung C and justifies the vector-layer work (int8, ANN).

GPU-dependent metrics (embedding-fidelity, LLM task-success) are opt-in tiers,
deferred until there is GPU headroom. Build the ruler before tuning the rung.

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

**The framing — now shipped (#290).** dex used to read as an *infra project* —
it needed ollama, a GPU, config. The "lean" intuition is that dex should *also*
be a **drop-in tool you `go install` and it Just Works** with zero inference
stack. That is the adoption lever, and it is now a documented, first-class
deployment mode — see **[docs/lean-profile.md](lean-profile.md)**. Two forms:
CPU-ONNX (`-tags onnx`, semantic lane on CPU) and BM25-only
(`DEX_EMBED_ENGINE=none`, no embedder). The tool surface is **capability-derived**
(#283): with no embedder wired, the embedding-backed tools are not advertised at
all; `ask` degrades to the symbol + graph lanes. The default build stays
dep-free per #67 (ONNX behind the build tag, like `sqlite_fts5`); the lean
profile is a documented build, never the silent default. Remaining: pure
no-embedder *indexing* (#306) and the lean-rung quality eval (#305).

---

## Cross-cutting: do we need tool profiles? (#283)

The skepticism in #283 is partly right. Today the tiers (`ask` / `standard` /
`power`) split on **tool count** — a thin surface so Claude sees one tool, not
thirty. That rationale is sound (the tool-description block costs tokens, and a
long menu induces choice paralysis). But splitting on *count* is the wrong
axis. The right axis is **the capability ladder itself**:

- A no-GPU deployment (Rung C) physically *cannot* offer `find` with
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

The execution backlog and live ordering are tracked in the capability-ladder
epic (#294); this is the frame behind it. **Already landed** (the brainstorm
pre-dated these): the retrieval ruler #247, graph k-hop #248, embedder +
Matryoshka #249, Contextual Retrieval #250, the `target_model` profile #204,
cache-layout #205, progressive disclosure #206, and the multi-repo corpus #278.
The ruler and the top retrieval gains are done — the critical path moved.

What remains, in order:

1. **Extend the ruler (#295)** — `dex bench compress` (#296) + `dex bench perf`
   (#297). Deterministic, zero-inference, runs on a starved box. Each is a
   just-in-time gate: #297 before the vector-layer work, #296 before the local
   track.
2. **The proxy (#232)** — the largest open Claude token win and the one lever
   the engine can't otherwise pull. Spike (#235) and measure first; the history
   pruning (#237) is deterministic and zero-GPU.
3. **Lean / zero-infra Rung C (#290)** — prioritized ahead of Rung B under the
   GPU-scarcity lens; builds on the ONNX embedder (#180).
4. **Activate the local track Rung B (#158)** — anchor-verbatim safety (#291),
   tokenizer-gated rules (#292), symmap efficacy (#293); gated by #296.
5. **Cheap and independent, pull early:** the Qwen3 reranker (#256) and the
   tool-profile collapse (#283). Heavy model bumps (#298) stay deferred.

---

## Open strategic questions

1. **Proxy posture (#232).** The proxy is middleware in the request path — it
   sees API keys and must fail open. Do we want dex to go there, or stay a pure
   MCP tool and accept that old `tool_results` remain uncompressed? This is the
   one compression lever the engine cannot otherwise pull. *(Still open — the
   one decision that gates Phase 1.)*

_Resolved since the first draft:_ the lean / zero-infra epic now exists (#290),
and the GPU-scarcity lens settled the priority question — proxy first (largest
Claude win, partly deterministic), then the lean Rung C ahead of the local Rung
B. See "Two cross-cutting forces" above and the #294 epic.

_Source: tool-vision brainstorm, 2026-06-11 (issue #288), refreshed the same day
(#299) to track the filed plan. Cross-references the capability-ladder epic
(#294), the measurement layer (#295), the SOTA retrieval epic (#246), the
compress tracks (#157 CLAUDE / #158 LOCAL), the proxy epic (#232), and the
degradation/ONNX epic (#174)._
