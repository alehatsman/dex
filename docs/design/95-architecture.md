# Design — #95 Architecture review: layers, primitives, and the road to the intelligence platform

Status: **design / architecture — largely LANDED** · Grounds the #95 platform epic against the as-built code.

> **Update (2026-08-26, #205):** the L3 **knowledge** subsystem is GONE — the
> `record` verb, `dex notes`, `knowledge_facts`/`knowledge_relations` and the
> `recallFacts` read path were all removed. dex is deterministic retrieval over
> the codebase, not agent memory; durable findings are the harness's file-based
> memory. Every reference below to `knowledge`/`notes`/`remember`/`recallFacts`
> and the "persistent repo memory" / §6.2 "one knowledge store" discussion is
> **historical** — read it as the as-of-design analysis, not the current shape.

> **Update (2026-08-06):** the §6.1 seam decision shipped. Assembly now lives in L2
> (`retrieve/assembler.go`, `pack.go`, `inline_assemble.go`); `mcp/context.go` holds only
> the JSON-tagged projection + transport concerns — exactly the §6.1 target. **6 of 7
> child issues shipped** (#95a–#95e, #95g). The 7th, **#95f** (multi-agent surface), is
> **closed as not-planned** (issue #106): superseded by the #110 four-verb redesign, which
> retires `session`/`checkpoint` as tools and makes session an implicit envelope spine.
> The forward-looking framing below (§2 inversion
> "risk", §5 "Step 0", §7 "proposed") is kept as the original as-of-design analysis —
> read it as history, not a to-do list. Live per-issue status in §7.

## TL;DR — #95 is 60% consolidation, not a greenfield build

The epic reads as ten new capabilities. The code says otherwise: the primitives and
most of the storage already exist. A fan-in and package-layer audit shows #95 is
really three moves:

1. **Name the context-pack contract** (WS3) — assembly already happens (`IntentAssemble`,
   #687), but there is no stable schema.
2. **Stop discarding confidence signals** (WS7) — the facts exist (`recall:partial`,
   type-resolved vs name-based edges), nothing surfaces them as a first-class field.
3. **Move the assembly seam down one layer** — from `internal/mcp` (L4) into the
   retrieval layer (L2) — plus fill the two genuinely empty rooms (trust envelope,
   multi-agent surface).

Scorecard across the 10 workstreams: **3 exist, 5 half-built, 2 empty.** Start with
**#95a → #95b → #95c**; that trio alone satisfies three of the seven acceptance
criteria and de-risks the rest.

## 1. What dex actually is today

Ground truth from the tree: **57 internal packages** (`go list ./internal/...`),
one fat `cmd/dex`, a SQLite store with **15 tables**, an MCP verb surface of
**~21 tools**. Fan-in (how many internal packages import a package) ranks the
load-bearing walls:

```
store 12 · graph 9 · embed 9 · gitenv 7 · graphquery 7 · tokens 7 · ignore 6 · proj 5
```

That fan-in profile *is* the architecture. `store` and `graph` are the gravity wells;
everything else orbits them. The ranking is now re-derivable on the agent surface —
`ask(intent=package_topology)` carries `in_degree`/`out_degree`/`page_rank` per
package (#190), so this table stays self-checking instead of drifting.

## 2. The layers (as-built, bottom-up)

They stratify cleanly into 5 layers (the table names the load-bearing packages per
layer, not every leaf). Key claim: **the layering is already mostly correct** —
dependencies point downward. The mess is at L4 (`cmd/dex` is a junk drawer) and the
*absence* of an explicit L3 contract.

| Layer | Packages | Role |
|---|---|---|
| **L0 — Foundation** | `logx tokens proj gitenv ignore backendhttp compress output lock redact throttle slo` | Pure utility. No domain knowledge. Import target for everyone; imports almost nobody. |
| **L1 — Substrate** | `store` · `embed rerank chat` (model backends) | Persistence + the three model I/O ports. `store` = SQLite/FTS5/BM25. Single source of truth. |
| **L1.5 — Ingest** | `source chunk symbols graph index watch graphrefresh gitrecency trigram` | Turn a repo into rows: parse → chunk → symbolize → build graph → embed → persist. Incremental via `watch`/`graphrefresh`. |
| **L2 — Retrieval** | `retrieve graphquery summarize heatmap cohesion codemap` | Query-time engine: hybrid search, RRF fusion, rerank, **intent routing**, graph walks, spreading activation. |
| **L3 — Derived knowledge** | `feedback review` *(+ store table `file_summaries`)* | Facts source alone doesn't state. **The `knowledge` subsystem (the `record` verb, `knowledge_facts`/`knowledge_relations` tables, `dex notes`) was REMOVED in #205** — dex is retrieval over the codebase, not agent memory. The DDL survives dormant/unreferenced (idempotent migrations); `feedback` (lane reweighting) and `review` (change-scoped findings) remain. |
| **L4 — Surface** | `mcp` (verbs) · `ctx` (ledger) · `cmd/dex` · `proxy` | Verb contracts + CLI. Cross-cutting: `eval bench rehearse shadow profiles`. |

### Dependency-graph health

- **Good:** L0/L1 have huge fan-in and near-zero fan-out — correct for a substrate.
  `graphquery` (fan-in 7) sits cleanly above `graph`+`store`.
- **Smell:** `cmd/dex` is enormous (~80+ files) and holds orchestration that belongs
  in L2/L3. `main_clients.go`, `doctor_deep.go`, `knowledge.go` are business logic
  wearing a CLI costume. `cmd/dex` — not any internal package — is the real god-module.
- **Latent inversion risk — RESOLVED (#95a):** at design time `mcp` was starting to
  hold assembly logic. It has since moved down: `retrieve.Assembler.Assemble` (in
  `retrieve/assembler.go`) produces a domain `ContextPack`, and `mcp/context.go` only
  projects it onto the wire response. The `AssembleConcerns` type remaining in `mcp` is
  now the JSON-tagged *wire* twin, not assembly logic — the intended §6.1 end state.

## 3. The primitives dex *must* have (the canonical set)

Stripped to its irreducible verbs, everything in #95 is a *composition* of these — not
a new primitive. This is the most important framing for the epic.

**Retrieval primitives (proven, stable):**
1. `search` — hybrid semantic + BM25 + symbol fusion
2. `read` — index-scoped file views (signatures/skeleton/map/slice)
3. `locate` — symbol → definition
4. `trace` — call-graph walk (callers/callees/path/**impact**)
5. `grep` — exact RE2

**State primitives:**
6. `notes` — durable scoped memory (write + recall)
7. `store` — row-level truth (chunks, nodes, edges, summaries, facts)

**Model ports (interchangeable, per the #77 finding):**
8. `embed` · `rerank` · `chat` — three swappable backends behind `backendhttp`

**The composition layer that already exists but isn't named:**
9. `ResolveIntent` (`retrieve/intent.go`) — 8 intents already routed
10. `IntentAssemble` (#687) — the context-pack assembler

**Everything #95 asks for is verbs 9+10 hardened, plus L3 filled in.**

## 4. #95 gap analysis — what's built vs. what's missing

Each of the 10 workstreams checked against the actual code:

| WS | #95 workstream | Reality | Status |
|---|---|---|---|
| 1 | Intent-based retrieval | `ResolveIntent`, 8 intents, auto-routing | 🟢 exists |
| 2 | Repository knowledge graph | `graph_nodes/edges` + `knowledge_facts/relations` tables, all wired | 🟡 typed edges yes; typed *relations* thin |
| 3 | Task-specific context packs | `retrieve.ContextPack` + `pack_test.go` contract (#95b) | 🟢 **done** — schema frozen in L2 |
| 4 | Persistent repo memory | `knowledge_facts` (one store, written by `remember`/consolidate, read by `recallFacts`); `gotcha`/`review`/gate findings are a separate change-scoped lane | 🟢 one store, one reader (§6.2 decided) |
| 5 | Hierarchical summaries | pkg/subsystem rollups (#95e) | 🟢 **done** (`4fba1e0`) |
| 6 | Impact & risk | `trace impact` + `tests_to_run` (#654) | 🟢 exists, needs risk-score surfacing |
| 7 | Confidence/trust envelope | trust envelope threaded onto the pack (#95c) | 🟢 **done** (`61fa5a4`, `#116`) |
| 8 | Intent-specific policies | `EvidencePolicy` table per intent (#95d) | 🟢 **done** (`bba5257`) |
| 9 | Repo health intelligence | `smells clusters clones cohesion heatmap` | 🟢 exists (DEX_EXPERT-gated) |
| 10 | Multi-agent shared intel | `agents`/`agent_messages`/`sessions` tables | ⚫ **won't-do (now)** — #95f closed, superseded by #110 |

**Verdict (as of 2026-08-06):** the cheap-program thesis held. The three original gaps —
(WS3) pack schema, (WS7) trust envelope — have **landed** (#95b, #95c); only **(WS10)
surfacing the agent tables** (#95f) remains open, and WS4 (unified memory) is still
partial. Everything else is 🟢.

## 5. Evolution path

Reordered around the *architectural* dependency, not the feature list. Each step is a
narrow, measurable child issue.

**✅ Step 0 — Move the seam to the right layer (#95a, landed).**
Assembly moved out of `internal/mcp` into `internal/retrieve` (`assembler.go`, `pack.go`,
`inline_assemble.go`). L4 now only projects the pack onto the wire response. Shipped at
zero behavior change, as planned — the tail-fold in `2746531` gave `Assemble` the full
ask sequence.

**✅ Step 1 — Freeze the ContextPack schema (#95b, landed).**
`retrieve.ContextPack` is the frozen struct; `pack_test.go` is the golden contract
(the #93 pattern). Backbone for the metadata the other workstreams hang off.

**✅ Step 2 — Trust envelope (#95c, landed).**
Confidence/freshness now thread from `graphquery`/`retrieve` onto the pack (`61fa5a4`,
consolidated in `#116`). No new computation — surfaces what dex already knew.

**✅ Step 3 — Per-intent evidence policies (#95d, landed).**
`EvidencePolicy` (`bba5257`) is the one-table intent → lanes → pack-sections mapping over
`ResolveIntent`.

**✅ Step 4 — Hierarchical summaries (#95e, landed).**
`file_summaries` extended upward to package/subsystem rollups (`4fba1e0`) with the same
source-linked invalidation. Opt-in — default path stays fast.

**⚫ Step 5 — Multi-agent surface (#95f, CLOSED — not planned, issue #106).**
Not built. The store tables stay, but surfacing them *as verbs* is superseded by the #110
four-verb redesign (session becomes an implicit envelope spine; `session`/`checkpoint`
are retired), and #95f's own "needs a concrete multi-agent consumer first" precondition is
unmet. Reopen only when a real consumer exists — and then design it inside #110's envelope
model, not as standalone tools.

**Measurement gate (non-negotiable, per the #96/#97/#91 discipline):** every step ships
with a before/after on *tool-calls-per-task* and *tokens-per-task* via
`internal/eval` + `bench`. #95's acceptance criteria demand this; the harness exists.
**Landed** for the headline modify-symbol workflow as `internal/bench/pack`
(`dex bench pack`, issue #163): on dex's own repo the one-call `ask(intent=assemble)`
pack vs the primitive multi-call path (locate → read → trace callers → trace callees →
find tests → read each) measures **9.0 → 1.0 tool calls (−88.9%)** and
**~20.5k → ~3.0k tokens (−85.2%)** at **100% reach** and **~59% call-graph ripple
recall** — recall reported as the correctness floor (a cheap-but-thin pack shows up as
lower recall, not a hidden gain). That satisfies AC #2 (materially fewer calls) and
AC #6 (fewer tokens with correctness measured, not asserted); the ~59% recall is the
one honest caveat — headroom in the assemble intent's call-graph coverage.

## 6. Architectural tensions to decide now

### 6.1 Where does assembly live? — domain vs wire (decided → LANDED via #95a)

At design time the seam was *smeared*: assembly ran across both `internal/retrieve`
(intent routing, lane selection, keyword assembly) and `internal/mcp` (concern-tagging,
pool expansion, next-action hints on top of mcp-owned DTOs). The rule that untangles it
is **domain vs wire** — and the codebase already half-applies it: `retrieve` defines
domain `SemHit` (`service.go:47`) and `SuggestedRead` (`results.go:12`), while `mcp`
defines JSON-tagged twins it maps to. #95a **completed** that split for the pack +
`SymbolHit` (the SymHit→SymbolHit merge landed in #112):

| Thing | Owner | Why |
|---|---|---|
| `ContextPack` (domain: evidence + concerns + **trust envelope**), assembly logic (`assembleConcerns`, `expandAssemblePool`), domain `SymbolHit` | **L2 `retrieve`/`pack`** | Assembled *knowledge*. The assembly funcs are domain logic living at L4 by accident. |
| `ContextOutput` + its `json:"..."` tags, `pack → output` mapping | **L4 `mcp`** | It's the MCP tool wire contract (the schema #93 pins). Pulling it into L2 inverts the layering the other way — the retrieval engine would learn the wire format. |

So: **L2 owns the pack and the assembly; L4 keeps only the JSON-tagged projection.**
The `ContextPack` domain type *is* the #95b schema; `ContextOutput` becomes its
serialization; the #95c trust fields live natively in L2 where the facts are. See
[95-context-pack.md](95-context-pack.md) for the schema and the field-by-field mapping.

### 6.2 One knowledge store or three? — **decided 2026-08-13: one store, one reader**
The premise has already largely resolved itself. There is no separate `notes` table:
the durable-memory surface **is** `knowledge_facts`, written by the `remember` verb and
`knowledge consolidate`, and read by the **single** `recallFacts` path shared by `ask`,
`locate`, and `review`. So the durable lane is already one store behind one reader —
no schema unification is warranted (merging would be a lossy union for no agent-visible
gain; *reject unnecessary complexity*).

`gotcha`/`review` + the gate findings (`.gate/findings.jsonl`, #155) are **not** a
competing memory store — they are a deliberately separate, **change-scoped** advisory
lane attached per-file at review time, not durable repo knowledge. Keeping them separate
is correct, not a gap. Decision: **keep the three writers, one reader (`recallFacts`);
do not unify storage.** Surfacing review/gate findings through general recall is an
explicit non-goal — they age out with the change that produced them.

### 6.3 `cmd/dex` weight
It is the real god-module. A grooming pass (#95g) has started — `44b84cb` pushed the
embed backend-defaults heuristic down to `internal/embed` — but `cmd/dex` remains the
heaviest cluster (~90 symbols; `graph.go`/`knowledge.go`/`doctor.go` still fat). Ongoing,
out of the critical path.

## 7. Child issues — status (updated 2026-08-06)

- ✅ **#95a** refactor: extract context-pack assembly `mcp → retrieve/pack` — **done** (`2746531`, `876c5c6`; #111–#114 lifted the cores, #112 merged SymHit→SymbolHit)
- ✅ **#95b** feat: stable `ContextPack` schema + golden contract test — **done** (`46ca6e3`, `pack_test.go`)
- ✅ **#95c** feat: trust envelope threaded into pack — **done** (`61fa5a4`, consolidated in `#116`)
- ✅ **#95d** feat: per-intent evidence policies over `ResolveIntent` — **done** (`bba5257`, `EvidencePolicy`)
- ✅ **#95e** feat: hierarchical (pkg/subsystem) summaries — **done** (`4fba1e0`)
- ⚫ **#95f** feat: surface agent/session tables as shared-context verbs — **CLOSED, not planned** (issue #106); superseded by #110, precondition (a real multi-agent consumer) unmet
- 🟡 **#95g** chore: `cmd/dex` grooming (push logic to L2/L3) — **partial** (`44b84cb` pushed embed backend-defaults down; `cmd/dex` is still the heaviest cluster)
- ✅ **#163** bench: pack-efficiency lane — **done** (`internal/bench/pack`, `dex bench pack`); proves AC #2 (−88.9% calls) + AC #6 (−85.2% tokens) at ~59% ripple recall, closing the epic's measurement gate

## 8. Bottom line (updated 2026-08-13)

The cheap-program thesis held: dex already had the primitives, so #95 was mostly
"name the contract, stop throwing away confidence signals, move the assembly seam down
one layer." **That work landed** — assembly lives in L2, the pack schema is frozen and
contract-tested, and the trust envelope + evidence policies + hierarchical summaries all
shipped. The measurement gate — the epic's one "non-negotiable" that had lagged the
features — now has its instrument: `dex bench pack` (#163) demonstrates the modify-symbol
pack workflow at **−88.9% tool calls and −85.2% tokens** vs primitives, satisfying AC #2
and AC #6. **#95f** (multi-agent surface) is **closed as not-planned** — superseded by the
#110 four-verb redesign and lacking a real consumer, though #159 (cross-agent coordination)
may become that consumer. The only live tail is **#95g** (`cmd/dex` grooming), out of the
critical path. With all seven acceptance criteria now met, **the epic is ready to close.**
The original ordering was #95a → #95b → #95c; history followed it.
