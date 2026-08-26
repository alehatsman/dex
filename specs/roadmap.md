# Roadmap — dex collapses to one composable verb

Status: **active plan** · 2026-08-26 · One linear arc, strict dependency order.

## The thesis

dex's identity is deterministic, provenance-labeled retrieval over a codebase.
The surface should be as small as that identity: **one verb, `query`, that
composes.** The "jQuery of code intelligence" — every agent knows one entry
point, and chains primitives over a uniform result set.

We get there in four ordered phases. Each phase is independently shippable and
leaves `main` green; each unlocks the next. **Do them in order** — the
dependencies are real, not cosmetic.

```
Phase 0 ─────────► Phase 1 ──► Phase 2 ──► Phase 3
 de-scope to core   spine       compose     grow
 (#205 + #208)      (#207)       (#206)     (#206 ph2/3)
```

**The reframe:** dex accreted **three separate products** in one binary —
code-intel [CORE], agent-memory [`record`/L3], and an agent-cost-proxy
[`internal/proxy`]. Phase 0 cuts the latter two, leaving a single-purpose tool
the pipe experiment can layer onto clean abstractions.

---

## Phase 0 — De-scope to core  ·  #205 + #208  (closes #204)

Two independent cuts, either order. Both shrink the surface before any new work.

### 0a — Cut `record` + L3  ·  #205  (closes #204)

**Goal:** remove the `record` verb and the entire L3 derived-knowledge
subsystem. dex becomes single-verb on the write side.

**Why:** deletes the biggest god-file (`store_knowledge.go`, 1404 LOC), removes
the one true SRP violation, and **rips `KnowledgeFacts` + the recall path out of
`ContextOutput`** — cutting the god-struct open for Phase 1.

**Scope:** write side (`record.go` + registration), the read-side leak into
`query`'s envelope (`retrieve/pack.go`, `context_envelope.go`), store god-file +
table retirement, `dex notes` CLI, the abandoned swarm `agent_messages` spine.

### 0b — Cut the LLM API proxy  ·  #208

**Goal:** remove `dex proxy` + `internal/proxy` (~8,160 LOC) — an Anthropic API
cost middlebox whose differentiators the platform has obsoleted (auto-compaction,
native caching, native tool-search, 1M context). Orthogonal to code intelligence.

**Scope:** `internal/proxy/`, `cmd/dex/proxy.go`, proxy dispatch + doctor checks,
the CCR bridge in `internal/mcp/server_summarize_helpers.go` (its only leak into
the query surface), `specs/proxy.md`, usage/env docs. **Keep** `internal/tokens`
(token counting), `internal/slo`, `internal/chat`, `internal/compress`, and
`dex hook redirect` — all used elsewhere.

**Done when:** MCP surface is `query` only; `query` envelope has no dead-table
reads; `store` carries no L3 logic; no `internal/proxy`; `mooncake task ci`
green; #204 closed as superseded.

**Kept, deliberately:** `dex serve` + the remote MCP shim — needed so container
agents (moongit) can reach a shared dex. This carries a **requirement into Phase
1** (below).

---

## Phase 1 — Give `query` a spine  ·  #207  (blocked by #205)

**Goal:** collapse the `look`/`ask` facades into flat lanes that all emit **one
uniform result currency** — `Selection` (a set of `Ref`s + trust + budget).

**Not a green-field IR — the universalization of an existing seam.** The
domain/wire seam already exists: `internal/retrieve/ContextPack` (transport-free
domain result + `Trust` envelope + `Concerns`, epic #95 / #101 / #111 CLOSED).
But it is wired for **only one of three lane families**:

| Lane family | Domain type | Wire type | Seam |
|---|---|---|---|
| semantic / assemble | `retrieve.ContextPack` | `mcp.ContextOutput` | ✅ clean |
| orient / review | none | `mcp.ContextOutput` (inline) | ❌ bypassed |
| read / grep / trace / locate | none | bespoke `*Output` | ❌ absent |

`Selection` is `ContextPack` generalized to a thin universal currency. Extend a
proven, tested seam to the other lanes — don't invent.

**The smells are blockers (full collapse, decided) — all are symptoms of the
partial seam, all fixed here:**
- **Partial seam** → every lane emits a domain result; mcp becomes pure projection.
- **Facade-over-facade** (`QueryResult{Look, Ask}` union-in-union) → one currency.
- **`normalizeNext`** (`query.go:393`, leaves emit dead verb names) → deleted.
- **Vocab drift** (`symbol`=`trace`=`traceVerb`) → one name across layers.
- **`ContextOutput` god-struct** (35 fields, intent-gated) → per-intent projections.
- **Stale seam docs** (`pack.go` comment + `95-context-pack.md` predate #111) → truthed up.

**Scope:**
- `Ref`/`Selection` in `internal/retrieve`. **Settle first:** `Selection` = thin
  currency; `ContextPack` *has-a* `Selection` + rich assemble lanes (embed, not
  subsume — zero regression on the rich projection).
- Every lane (read/grep/locate/symbol/orient/review/semantic) → `input →
  Selection`; old `*Output` structs become terminal projections *from* a Selection.
- Kill `normalizeNext`; unify `symbol`/`trace` vocab; split the god-struct.
- Add `route.stages` to `QueryRoute`.

**Serve requirement (from Phase 0b keeping `dex serve`):** today the wire
surface exposes the *old primitives* (`/ask`, `/lookup`, …) and the remote shim
composes `query` **client-side** — one `query` = several REST round-trips, chatty
over a container network. Phase 1 must make **`query` a first-class server-side
endpoint** (one round-trip per call; later one round-trip for a whole pipe), so
`Selection` must be wire-serializable (it already is by design).

**Guardrails:** behavior-preserving — identical wire shapes for every existing
single-lane call, locked by the current test suites. **Do not touch the
classifiers** (`classifyQuery`/`classifyLookTarget`) — they're clean.

**Done when:** every lane is `input → Selection`; mcp builds no wire struct
inline; `normalizeNext` gone; single vocabulary; god-struct split; seam docs
truthed-up; `query` is a first-class wire endpoint; CI green, no wire regressions.

---

## Phase 2 — Make `query` composable  ·  #206  (blocked by #207)

**Goal:** `query("internal/store | callers | impact")` — `|`-separated stages
run left-to-right in one call. The round-trip-collapse win.

**Why here:** once every lane speaks `Selection` (Phase 1), a pipe is just
*fold lanes over a Selection*. The MVP becomes mechanical: parse `|`, run stage
1 as a seed, interior stages as transforms (fan-out over refs), last as a
terminal.

**Scope (MVP):** top-level `|` parsing in `queryVerb`; 3 seeds (path / `/regex/`
/ prose); 2 transforms (`callers`, `impact`) with fan-out; 2 terminals
(`signatures`, `assemble:N`); 1 coercion (`file|chunk → enclosing symbol`);
weakest-link `trust`; budget-flow.

**Go/no-go (empirical):** token cost of `pkg | callers | impact` in one call vs
three separate `query` calls; agent lands the right context in one shot;
correctness == three manual round-trips on a fixture graph. If it beats N calls
→ Phase 3. If forced → a couple days spent, stop.

**Done when:** the two MVP pipes run end-to-end; `route.stages` populated; CI
green; the token-win measurement is recorded.

---

## Phase 3 — Grow the vocabulary  ·  #206 phase 2/3  (only if Phase 2 proves out)

Deferred until pipes earn it, then grown from **observed agent use**, not a
whiteboard:

- **Selector-grammar seeds** — `pkg:store func:*Handler calls:embed`. The
  "CSS of code": a terse deterministic compound selector as a seed.
- **More transforms / coercions** — added where agent errors cluster.
- **LLM-backed dynamic transforms** — `explain`/`summarize`/`classify`/`rerank`,
  the pluggable-local-model lanes, provenance `model`. The "a bit dynamic" half
  of the vision.

---

## Parallel / opportunistic (not on the critical path)

- **Redaction consolidation.** Secret patterns are triplicated across
  `redact` / `compress` / `ignore`, reconciled only by a test. Move the pattern
  set into `redact`; the other two consume it. Small, security-relevant,
  independent — slot in whenever. (dex memory finding id 8.)

## Dependency summary

| Phase | Issue | Blocked by | Unblocks |
|-------|-------|-----------|----------|
| 0a cut record | #205 | — | #207 |
| 0b cut proxy | #208 | — | — (independent) |
| 1 spine | #207 | #205 | #206 |
| 2 compose | #206 | #207 | Phase 3 |
| 3 grow | #206 ph2/3 | #206 MVP proving out | — |

**One agent, one phase at a time.** #205 and #208 are independent (either order,
even parallel). Do not start Phase 1 before #205 lands (they fight over
`ContextOutput`); do not start Phase 2 before Phase 1 lands (no `Selection` to
compose). Phase 3 is gated on Phase 2's measurement.
