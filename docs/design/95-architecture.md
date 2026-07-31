# Design — #95 Architecture review: layers, primitives, and the road to the intelligence platform

Status: **design / architecture** · Grounds the #95 platform epic against the as-built code.
Decide the layering seam (§6.1) before any child-issue implementation.

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

Ground truth from the tree: **40 internal packages**, one fat `cmd/dex`, a SQLite
store with **15 tables**, an MCP verb surface of **~21 tools**. Fan-in (how many
internal packages import a package) ranks the load-bearing walls:

```
store 12 · graph 9 · embed 8 · tokens 7 · graphquery 7 · ignore 6 · gitenv 6 · proj 5
```

That fan-in profile *is* the architecture. `store` and `graph` are the gravity wells;
everything else orbits them.

## 2. The layers (as-built, bottom-up)

The 40 packages stratify cleanly into 5 layers. Key claim: **the layering is already
mostly correct** — dependencies point downward. The mess is at L4 (`cmd/dex` is a junk
drawer) and the *absence* of an explicit L3 contract.

| Layer | Packages | Role |
|---|---|---|
| **L0 — Foundation** | `logx tokens proj gitenv ignore backendhttp compress output lock redact throttle slo` | Pure utility. No domain knowledge. Import target for everyone; imports almost nobody. |
| **L1 — Substrate** | `store` · `embed rerank chat` (model backends) | Persistence + the three model I/O ports. `store` = SQLite/FTS5/BM25. Single source of truth. |
| **L1.5 — Ingest** | `source chunk symbols graph index watch graphrefresh gitrecency trigram` | Turn a repo into rows: parse → chunk → symbolize → build graph → embed → persist. Incremental via `watch`/`graphrefresh`. |
| **L2 — Retrieval** | `retrieve graphquery summarize heatmap cohesion codemap` | Query-time engine: hybrid search, RRF fusion, rerank, **intent routing**, graph walks, spreading activation. |
| **L3 — Derived knowledge** | `gotcha feedback review knowledge` *(+ store tables: `knowledge_facts`, `knowledge_relations`, `file_summaries`, `agents`, `agent_messages`)* | Facts source alone doesn't state. **Half-built** — tables exist, wiring is thin. |
| **L4 — Surface** | `mcp` (verbs) · `ctx` (ledger) · `cmd/dex` · `proxy` | Verb contracts + CLI. Cross-cutting: `eval bench rehearse shadow profiles`. |

### Dependency-graph health

- **Good:** L0/L1 have huge fan-in and near-zero fan-out — correct for a substrate.
  `graphquery` (fan-in 7) sits cleanly above `graph`+`store`.
- **Smell:** `cmd/dex` is enormous (~80+ files) and holds orchestration that belongs
  in L2/L3. `main_clients.go`, `doctor_deep.go`, `knowledge.go` are business logic
  wearing a CLI costume. `cmd/dex` — not any internal package — is the real god-module.
- **Latent inversion risk:** `mcp` is starting to hold assembly logic
  (`AssembleConcerns` lives in `internal/mcp/context.go`, not in `retrieve`). If
  context-pack assembly grows inside the verb layer, L4 depends on L4 and the seam #95
  needs calcifies in the wrong place.

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
| 3 | Task-specific context packs | `IntentAssemble` (#687), `AssembleConcerns` | 🟡 assembles, but **no stable schema** |
| 4 | Persistent repo memory | `notes`/`gotcha`/`review` (#87) + `knowledge_facts` | 🟡 exists, not unified |
| 5 | Hierarchical summaries | `file_summaries` table + `main_summarize.go` | 🟡 **file-level only**, not hierarchical |
| 6 | Impact & risk | `trace impact` + `tests_to_run` (#654) | 🟢 exists, needs risk-score surfacing |
| 7 | Confidence/trust envelope | `recall:partial` tags, type-resolved edges (#85/#604) | 🔴 **facts exist, no envelope field** |
| 8 | Intent-specific policies | intent routing selects lanes | 🟡 routing yes, per-intent *evidence policies* no |
| 9 | Repo health intelligence | `smells clusters clones cohesion heatmap` | 🟢 exists (DEX_EXPERT-gated) |
| 10 | Multi-agent shared intel | `agents`/`agent_messages`/`sessions` tables | 🔴 **store-ready, not surfaced** — vestigial |

**Verdict:** the primitives and even most of the storage already exist. The missing
40% is concentrated in three places: **(WS3) a stable pack schema**, **(WS7) the trust
envelope**, and **(WS10) surfacing the agent tables**. A much cheaper program than the
prose implies.

## 5. Evolution path

Reordered around the *architectural* dependency, not the feature list. Each step is a
narrow, measurable child issue.

**Step 0 — Move the seam to the right layer (pure refactor, no behavior change).**
Pull `AssembleConcerns`/assembly out of `internal/mcp` into `internal/retrieve` (or a
new `internal/pack`). Every WS3/WS8 child issue edits this code; it must live in L2,
not L4, or the inversion in §2 hardens. Unblocks everything, ships at zero risk.

**Step 1 — Freeze the ContextPack schema (WS3, satisfies acceptance criterion #1).**
One Go struct + a golden contract test — reuse the exact pattern from #93 (MCP
tool-schema contract). Fields: `sources[] · freshness · confidence · claims{proven|inferred}`.
This schema is the backbone the other workstreams hang metadata off.

**Step 2 — Trust envelope (WS7) — highest ROI, currently 🔴.**
The facts already exist (`recall:partial`, type-resolved vs name-based edges). Pure
plumbing: thread a `Confidence`/`Freshness` field from `graphquery`/`retrieve` up into
the Step-1 pack schema. No new computation — stop discarding what dex already knows.
Satisfies acceptance criterion #3.

**Step 3 — Per-intent evidence policies (WS8) on top of `ResolveIntent`.**
`ResolveIntent` already picks intents; add a policy table mapping intent → lanes → pack
sections. Bug-fix pulls recency+tests; security pulls input boundaries+crypto. Pure
composition of existing lanes.

**Step 4 — Hierarchical summaries (WS5).**
Extend `file_summaries` upward: package/subsystem rollups with the same source-linked
invalidation already present at file level. Cacheable, opt-in — keeps the default path
fast.

**Step 5 — Multi-agent surface (WS10).**
Only after 1–4. Tables exist; expose `agents`/`agent_messages` through the verb layer so
the pack becomes the shared context layer. Deferred deliberately — least-proven value.

**Measurement gate (non-negotiable, per the #96/#97/#91 discipline):** every step ships
with a before/after on *tool-calls-per-task* and *tokens-per-task* via
`internal/eval` + `bench`. #95's acceptance criteria demand this; the harness exists.

## 6. Architectural tensions to decide now

### 6.1 Where does assembly live? — domain vs wire (decided)

The seam isn't misplaced, it's *smeared*: assembly runs across both `internal/retrieve`
(intent routing, lane selection, keyword assembly) and `internal/mcp` (concern-tagging,
pool expansion, next-action hints on top of mcp-owned DTOs). The rule that untangles it
is **domain vs wire** — and the codebase already half-applies it: `retrieve` defines
domain `SemHit` (`service.go:47`) and `SuggestedRead` (`results.go:12`), while `mcp`
defines JSON-tagged twins it maps to. #95 finishes that split for the pack + `SymbolHit`:

| Thing | Owner | Why |
|---|---|---|
| `ContextPack` (domain: evidence + concerns + **trust envelope**), assembly logic (`assembleConcerns`, `expandAssemblePool`), domain `SymbolHit` | **L2 `retrieve`/`pack`** | Assembled *knowledge*. The assembly funcs are domain logic living at L4 by accident. |
| `ContextOutput` + its `json:"..."` tags, `pack → output` mapping | **L4 `mcp`** | It's the MCP tool wire contract (the schema #93 pins). Pulling it into L2 inverts the layering the other way — the retrieval engine would learn the wire format. |

So: **L2 owns the pack and the assembly; L4 keeps only the JSON-tagged projection.**
The `ContextPack` domain type *is* the #95b schema; `ContextOutput` becomes its
serialization; the #95c trust fields live natively in L2 where the facts are. See
[95-context-pack.md](95-context-pack.md) for the schema and the field-by-field mapping.

### 6.2 One knowledge store or three?
`notes` + `gotcha`/`review` + `knowledge_facts` are three memory systems. WS4 wants
"persistent repo memory" as one thing. Unify behind one read path, or keep separate
writers with one reader?

### 6.3 `cmd/dex` weight
It is the real god-module. Worth a grooming pass to push logic down into L2/L3, but out
of #95 scope unless it blocks a child issue.

## 7. Proposed child issues (narrow, measurable)

- **#95a** refactor: extract context-pack assembly `mcp → retrieve/pack` (no behavior change)
- **#95b** feat: stable `ContextPack` schema + golden contract test
- **#95c** feat: trust envelope (confidence/freshness) threaded into pack — *start here for value*
- **#95d** feat: per-intent evidence policies over `ResolveIntent`
- **#95e** feat: hierarchical (pkg/subsystem) summaries
- **#95f** feat: surface agent/session tables as shared-context verbs
- **#95g** chore: `cmd/dex` grooming (push logic to L2/L3) — independent

## 8. Bottom line

dex already has the primitives and most of the storage for #95. The epic is really
"name the context-pack contract, stop throwing away confidence signals, and move the
assembly seam down one layer" — plus filling the two genuinely empty rooms (trust
envelope, multi-agent surface). Start with **#95a → #95b → #95c**.
