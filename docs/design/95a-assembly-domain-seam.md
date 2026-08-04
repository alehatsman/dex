# 95a — Context-pack assembly: domain core + three thin transports

Child of #95 · issue #103 · builds on #101 (`retrieve.ContextPack`) and the
#95c trust envelope. Supersedes the framing in #103 with what the code actually
shows.

## TL;DR

`ask` already has three transports — CLI (`cmd/dex/main_ask.go` → `s.ContextRouterStream`),
MCP stdio (`registerTools(srv, s, …)` → `s.contextRouter`), and HTTP
(`newMCPHandler` → `registerTools(srv, projectScoped{s,root}, …)` → `s.contextRouter`).
They already converge on **one** implementation. The problem is *where* that
implementation lives: `*Server.contextRouterStream` fuses the **domain assembly**
(intent → lanes → graph → suggested reads → confidence) with **transport
concerns** (session dedup, throttle, activity nudge, MCP handle stamping, HTTP
project scoping, streaming answer synthesis). `*Server` is a god-object.

The fix: lift the domain assembly into L2 (`retrieve.Assembler` → `ContextPack`),
so `*Server.contextRouter` becomes a *thin adapter* — exactly like
`projectScoped.contextRouter` already is. Then the general shape holds:

```
CLI ─┐
HTTP ─┼─▶ thin transport adapter ─▶ retrieve.Assembler.Assemble() ─▶ ContextPack
MCP ─┘                                     (L2 domain core)              │
                                                                         ▼
                                             project to the transport's output type
```

## The general principle (per user)

> In general we should have three identical transport interfaces for the core
> domain: cli / http / mcp.

A transport adapter's only jobs are: (1) decode its wire input into the domain
request, (2) call the domain core, (3) project the domain result into its wire
output, (4) apply transport-only concerns (auth/scoping/session/streaming). No
transport owns domain logic. This doc applies the principle to `ask`; the same
shape is the target for every other verb (see "Wrong place — noted for later").

## What is domain vs transport (evidence from the code)

`contextRouterStream` body, classified:

| Phase | Step | Home |
|---|---|---|
| **Edge prep** | empty→orient, resolveProject, throttle guard, DB stat/no-index, k clamp, openStore, stale check, loadContextFacts | transport (stays) |
| **Domain assembly** | ResolveIntent, ExpandQuery, symbol lane (+Role +non-impl demotion), semantic lane, enrichGraph, pickSuggestedReads, references/related, Concerns, ConfidenceLevel | **→ L2 `retrieve.Assembler`** |
| **Wire/session/presentation post** | feedback reweight (A/B), inline byte-budget, next_action/avoid prose, activity nudge, loop throttle, answer synthesis (streaming), handle stamping, seen-dedup, clamp envelope | transport (stays) |

The domain helpers are already stateless free functions coupled only through
mcp's wire types (`formatRole` role.go, `isNonImplPath`/`pathTags` path_tags.go,
`enrichGraph` context_graph.go), and `pickSuggestedReads`/`ConfidenceLevel` are
already in L2. None touch `*Server`. That is why they *can* move; it is also why
they were easy to misfile.

## Design decision: inject, don't move (surfaced from the code)

`formatRole` (role.go) and `isNonImplPath`/`pathTags` (path_tags.go) look like
domain logic to relocate, but service.go:66-68 and context_lanes.go document a
*deliberate* choice: the transport owns the role-display vocabulary and path
classification because they are shared across the search/symbol/graph tools, and
`SymHit` carries raw centrality columns precisely so retrieve stays display-free.
`retrieve.PickSuggestedReads`/`InlineContentKeyed` already take these as injected
`func` params. So the Assembler **injects** `FormatRole` and `IsNonImpl` rather
than moving them — consistent with the existing seam, and it keeps the diff off
the four other tools that share those helpers. Feedback reweight (server A/B
state) is injected the same way, bridged to the domain `SemHit` in
`feedback_bridge.go` (the reweight is a pure permutation, recovered losslessly).

## Delivery: staged, each stage behavior-neutral

Full relocation is the destination; the path is staged so the tree is never
broken and drift is caught per stage (this continues the in-progress migration
service.go:18 already describes). Guard for every stage: the `TestContextRouter*`
suite + `mooncake task ci-fast`, byte-identical output (#93 tool-schema unchanged).

- **Stage 1 (this commit) — evidence core.** `retrieve.Assembler.Assemble`
  owns intent → expand → symbol lane (+injected role +demotion) → semantic lane
  → graph neighborhood → injected reweight → suggested reads, returning a
  `ContextPack`. `*Server.contextRouterStream` builds the request, projects the
  pack onto the wire `ContextOutput` (`context_project.go`), and continues the
  edge pipeline unchanged. Removes the now-dead `runSymbolLane` wrapper.
- **Stage 2a (done) — completeness + advice prose.** `AssembleConcerns`,
  `AssembleNextActionHint`, `firstInlinedAnchor` move to `retrieve`, beside the
  `BuildNextAction`/`BuildAvoid`/`ConfidenceLevel` prose already there; mcp keeps
  byte-neutral wrappers. `toNeutralSyms` now carries `Signature` (coverage is
  judged on name+signature). `ConfidenceLevel`/`NextAction`/`Avoid` were already
  L2 from a prior stage, so this closes the "confidence + concerns + prose" half.
- **Stage 2b (done — took path B) — the Enricher → L2 service.** The Enricher
  moved into `internal/retrieve` as a domain service on the neutral pack types
  (`PathMeta` twin + `ContextPack.Annotations` added); mcp keeps the thin
  `enrichWire` adapter in the `inlineContent` shape. All four verbs
  (ask/locate/review/graph_impact) now call one `retrieve.Enricher` — the shared
  usage was the argument *for* the move. `BareSymbolName` exported for the shared
  display path. Byte-neutral: the adapter round-trips exactly Signature/Doc +
  References/Annotations/RelatedFiles, index-aligned.

  Original framing (the fork that was weighed): the Enricher is **not `ask`-local**.
  Its **path-based legs**
  (`pairSiblingTests`, `findNearestDoc`, `enrichBlame`) are shared by
  locate/review/graph_impact on wire `map[string]*PathMeta`/paths. Only the
  top-level `Enrich(ctx, intent, k, *ContextOutput)` **orchestrator** is
  `ask`-only, and it is the sole thing welded to the wire `ContextOutput`. So
  moving "the Enricher" to L2 is a fork, not a given:
    - **(A) Injected enrich hook.** The Enricher stays a shared mcp component;
      the ask Assembler orchestrates it via an injected `Enrich` hook (like
      reweight/formatRole already are), so Assemble owns evidence→inline→enrich→
      prose and produces a complete pack. Requires inline to move into the
      sequence too (it must precede enrich — the Concerns/Signature ordering
      trap). Localized pack↔wire bridge glue in the ask router. In scope for ask.
    - **(B) Promote the Enricher to an L2 domain service.** Neutralize `PathMeta`
      + `RefHit`, move the Enricher to `retrieve`, adapt the three other verbs'
      local `map[string]*PathMeta` to the neutral twin. Structurally cleanest
      ("three identical transports" all the way down), but cross-verb — belongs
      in its own issue, not folded into the ask seam.
    - **(C) Stop at 2a.** Evidence core + advice/completeness are L2; the
      Enricher orchestration stays edge-side. Honest, minimal, defers A/B.
- **Stage 3 — inline byte-budget** relocation (presentation policy on the pack);
  subsumed into (A) if that path is taken, since inline must join the sequence.

Out of scope for all three: a distinct HTTP/CLI *direct* path that skips
`*Server` (unnecessary — all three transports already share the adapter; the
seam is what makes it possible later), and applying the pattern to other verbs.

## Wrong place — noted for later (not touched here)

- `*Server` god-object: also fuses session/throttle/feedback/cache state with
  every other tool handler. `ask` is the first verb to get the seam; the rest
  (search, brief, locate, trace, verify) still assemble inside `*Server`.
- `buildNextAction` / `buildAvoid` / `assembleNextActionHint` — presentation
  prose that reads the assembled evidence; arguably domain-advice, sits in mcp.
- `inlineWorkingSet` — byte-budget inlining is presentation policy on wire types.
- Doctor backend-readiness (already tracked as #109 → `internal/health`).

## Acceptance

- `retrieve.Assembler.Assemble` returns a populated `ContextPack` unit-tested in L2 without a `*Server`.
- `*Server.contextRouter*` output byte-identical (existing tests green).
- `mooncake task ci-fast` green.
