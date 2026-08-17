---
id: tool-surface
status: accepted
last_verified: ceb7e33
owners: [aleh]
covers:
  - "internal/mcp/server.go"
  - "internal/mcp/verbs.go"
  - "internal/mcp/tool_desc.go"
  - "internal/retrieve/intent.go"
  - "internal/retrieve/policy.go"
  - "internal/retrieve/pack.go"
tracking: alehatsman/dex#110
---

# Tool surface — the agent-facing contract

> **Superseded going forward by [`two-verb-surface.md`](two-verb-surface.md)
> (#195).** This spec documents the **shipped** four-verb surface
> (`ask · look · act · remember`) and remains the accepted record of what is in
> the tree today. The forward direction collapses it to two verbs
> (`query · record`); read the successor for the target design.

## Goal

Redefine dex's agent-facing interface from scratch into the most token-efficient
shape possible: **four verbs over one universal envelope over a stateful
session.** The engine underneath is unchanged — dex already is an
intent→evidence→pack machine (`ResolveIntent` `intent.go:84` → `PolicyFor`
`policy.go:120` → `ContextPack` `pack.go:13`). This spec redraws the *front
door* to be a faithful, minimal projection of that engine.

Efficiency thesis, in priority order:
1. **The envelope and the session model save more tokens than the verb list** —
   trust in-band (no re-grep to verify), handles not bytes (fetch only what
   reasoning demands), session dedup (never re-send what the agent has).
2. **Fewer tools = a permanently smaller system prompt** — every tool schema
   sits in context on *every* turn; 26 tools ≈ 3–5k tokens of standing overhead,
   4 verbs ≈ ~800. Invisible in per-call metrics; largest cumulative win.
3. **One fuzzy verb, three exact verbs** — the agent never wonders whether a
   call will guess.

## Non-goals

- Re-architecting retrieval / graph / assembly. Lanes and `EvidencePolicy` stay;
  this is a surface + envelope redesign.
- Removing capability. Everything survives — as an intent of `ask`, a view of
  `look`, or the CLI/expert reporting overlay.
- Content-agnostic ingest or a knowledge-graph-of-everything (graphify's
  product). dex stays code-intel.

## The engine dex already has (ground truth, main @ 44b84cb)

- **Intent classification** — `ResolveIntent` (`intent.go:84`), 8 intents.
- **Per-intent evidence policy** — `evidencePolicies` (`policy.go:81`),
  declarative table: intent → `{GraphLane, InlineCaps, BodyFill, MaxReads,
  AnswerMaxTokens}`.
- **Typed pack** — `ContextPack` (`pack.go:13`): evidence lanes + accumulated
  knowledge + `Concerns` (covered/dropped) + a `Trust` envelope.
- **Trust envelope** — `Trust` (`pack.go:46`): freshness, confidence, claim
  provenance (type-resolved vs name-based). **Defined but not wired** —
  `pack.go:11`: "no caller builds a ContextPack yet."

The design is sound. The surface (10 flat tools, computed from 4 booleans at
`server.go:850`, four of them each claiming to be "primary") is where it leaks.

## Design — four verbs

The agent wants a thing in one of two modes: *reasoning about what it needs*
(fuzzy, inference) or *knowing exactly what it wants* (exact, deterministic).
Build on that split. **Exactly one verb is allowed to guess.**

| Verb | Kind | Input | Returns | Absorbs |
|------|------|-------|---------|---------|
| **`ask`** | fuzzy (the only inference verb) | NL goal + optional anchor: symbol \| path:line \| diff \| frame \| none | intent-shaped ContextPack | brief, ask, search, locate(fuzzy), review_diff, repo_map; the orient/review/debug intents |
| **`look`** | exact | target: path \| path:lines \| symbol \| regex; `view`: signatures/skeleton/full/map/callers/callees/impact | exact bytes or graph edges, no inference | read, grep, trace |
| **`act`** | exact | a command | compressed output + failure-signature | shell, verify |
| **`remember`** | exact | fact to write, or query to recall | durable memory | notes (add/list/relate) |

`ask` carries confidence (it infers); `look`/`act`/`remember` are always
provenance-exact. Graph navigation lives under `look` — naming a symbol yields
exact edges; that's a fetch, not reasoning — which keeps `ask` a single-purpose
brain. Review and orient are not tools: `ask("review", anchor=diff)` and
`ask("understand this repo")`. The anchor *type* deterministically selects the
evidence policy, and the response echoes the chosen shape, so a polymorphic
`ask` stays predictable.

### Why this is not the `notes` mode-enum anti-pattern

`notes`' `action` selects between unrelated operations with disjoint params (a
namespace in a tool). `ask` takes one kind of input (a goal, optionally
anchored) and *infers* the shape; the `shape` override is optional, not a
required mode selector. Auto-routing ≠ mode discriminator.

## Design — the universal envelope

Every response from every verb has the same top-level shape. The agent learns it
once.

```
{ result,                                     // verb-specific payload
  trust:  { fresh, indexed_at, confidence,    // in-band; never re-grep to verify
            provenance: exact|semantic|name-based, caveat },
  cost:   { tokens_returned, budget_left },   // agent self-budgets
  next:   [ {verb, args, why} ],              // seeds the next move
  handles:[ ... ] }                           // pointers to more, NOT the bytes
```

Three fields carry the efficiency:

- **`trust` — always in-band.** dex's `Trust` (`pack.go:46`) promoted to the
  spine of every response. This is what lets an agent *believe the pack and stop
  re-verifying with grep* — the entire point of a code-intel tool. Mandatory,
  never optional.
- **`handles` — not bytes.** The default response is a map + expansion handles,
  not file dumps. The agent expands only what reasoning demands. Kills the
  dominant waste pattern (return N files hoping one is relevant). Progressive
  disclosure is the default interaction, not a mode.
- **`cost` + a `budget` request param.** Every `ask` takes `budget_tokens`; dex
  fits the pack and reports what it *dropped* (`Concerns` already computes
  covered/dropped). The agent controls spend explicitly.

## Design — session as the spine

The channel is stateless-per-call but the agent is stateful; the gap is pure
redundancy. Make session state mandatory on every response:

- Files already seen (dex has `SeenTurn`) return as "seen turn N, unchanged" —
  never re-inlined.
- A seen file that changed returns as a **delta**, not a re-dump (etag deltas
  exist).
- Packs dedupe against already-known facts.

dex has the pieces (`session`, `SeenTurn`, etag); the redesign makes them the
spine, not an opt-in.

## Design — close the engine's job gaps

Two of the most valuable agent jobs are missing from the intent/policy table.
Add them as first-class intents (new rows in `evidencePolicies`, same assembly
machinery), reached via `ask`:

- **`orient`** (`ask("understand this repo")`, no anchor) — operational
  orientation, not graph structure: purpose (README/doc.go) + **build/run/test
  commands** (tasks.yml/Makefile/package.json/CI) + entrypoints + top subsystems
  (clustering) + conventions (rules files) + centrality-ranked start-here.
  Progressive via #95e rollups. Renderable to a durable on-disk artifact
  (`dex orient --write` → seeds cold sessions; the graphify GRAPH_REPORT lesson).
- **`review`** (`ask("review", anchor=diff)`) — input is the diff, not a sniffed
  task string (retires the `brief.Review` special-case #83): blast-radius of
  changed symbols (`trace impact` → risk tier + tests_to_run) + covering tests +
  scoped notes/gotchas on touched files (#645) + duplication/convention
  divergence (clones/similar over changed blocks) + prior review findings (#87).
  Trust envelope mandatory (proven vs name-based edges).
- **`debug`** (`ask(anchor=frame|failing test)`, phase 2) — suspect symbol +
  recent history + covering tests + gotchas.

## Verb contracts

Two rules the whole surface hinges on, shared by every verb:

**Type detection** — one deterministic classifier maps a string to a kind:
`path` (has `/` or a file ext), `path:N-M` (path + line range), `symbol`
(bare / receiver-qualified / pkg-tail), `regex` (`/.../` or `pattern:`), `diff`
(unified-diff text, or the literals `staged`/`HEAD`/`working`), `frame`
(stack-trace line or `Test…` name), `handle` (opaque token from a prior
response), `none`. Ambiguous input (both a path and a symbol) → the response
**echoes the interpretation chosen** and offers the other in `next[]`. No silent
guessing.

**The `ask`-vs-`look` boundary** (both accept `symbol`/`path:line`):
`ask` = *assemble understanding* (fuzzy, ranked, may synthesize, carries
confidence); `look` = *give me the exact artifact* (deterministic,
provenance-exact). Same target, verb encodes intent:
`ask("how is Foo used", anchor=Foo)` assembles a neighborhood pack;
`look(Foo, view=callers)` returns the exact edge list.

### `ask` — the only verb that reasons

```
ask(goal, anchor?, shape?, budget_tokens?, project_root?)
  shape: auto(default) | orient | find | edit | review | debug | answer
```

- Classify `anchor` → this **deterministically fixes the shape** (`diff`→review,
  `frame`→debug, `none`+"understand this repo"→orient, `symbol`/`path:line`→
  neighborhood, else→find). `goal` refines *within* the shape, never overrides
  it — the predictability guarantee for a polymorphic verb.
- `ResolveIntent(goal, anchorType)` → `EvidencePolicy[intent]` → `ContextPack`.
- Returns **map + handles, bodies unexpanded** by default (`BodyFill` governs
  inlined source). Cheap-first; agent expands via `look(handle=…)`.
- Prose synthesis (`answer`) is opt-in / auto only when the goal is a question.
  Don't pay for prose the agent can read from evidence.
- `result` is a discriminated union by shape: `orient{purpose, commands,
  entrypoints, subsystems[handle], conventions, start_here}` · `find{hits[handle]}`
  · `edit{working_set[handle], rules, sibling_tests, impact}` · `review{findings
  [{severity, claim, evidence_handle, provenance}], blast_radius, tests_to_run,
  gotchas}` · `debug{suspect[handle], recent_changes, related_tests, gotchas}` ·
  `answer{prose, citations[handle]}`.
- `review` puts the literal `act("<the tests>")` in `next[]` — preserves the
  change→verify loop.

### `look` — the exact fetch (no inference)

```
look(target, view?, budget_tokens?, project_root?)
  view (files/symbols): signatures | skeleton | map | full | lines
  view (symbols, graph): callers | callees | impact
```

- Pure fetch — no LLM, no ranking. Absorbs `read` + `grep` + `trace`.
- Default `view` is **contextual on target granularity**: whole file →
  `signatures`; `path:N-M` or a single symbol → `full`; regex → matches;
  `handle` → expand that exact range (supersedes all).
- Graph nav is a `view`: `look(Foo, view=impact)` → blast-radius + `tests_to_run`.
- Session-aware: seen+unchanged → "seen turn N" marker; changed → delta.
- Never guesses *what you want* (the invariant). A graph edge may be
  `name-based`/`recall_partial` for non-Go — a fact-quality flag in
  `trust.provenance`, not inference. Budget too small for `full` → downgrade to
  `signatures` and say so. Power features (`ref`, `ccr_hash`, `json_path`) live
  behind an `advanced` object, off the primary schema.

### `act` — run it (the only exec side-effect)

```
act(command, cwd?, timeout_secs?, raw?)
```

- Run via bash; compress output (build noise, dedup logs, ANSI, go-test/git/npm/
  docker summaries); report `cost.saved_pct`.
- Non-zero exit matching a known signature → attach `gotcha_candidate`, surfaced
  in `next[]` as a `remember` prompt.
- Write-guard on `>`/`>>`/`tee` unless allowed; `raw:true` skips compression.
- **No magic verify.** `act` is dumb exec; it does not decide what to run. Test
  selection = `ask("review")` → `tests_to_run`; running = `act`. `ask` hands the
  exact `act(…)` in `next[]`, so the flow is two clean calls, not a blurred verb.

### `remember` — durable memory (write-primary)

```
remember(fact?, query?, scope?, archetype?, outcome?, relate?)
```

- `fact` → **write** a note; `scope` binds it to a path/glob (surfaces on touch,
  #645); `archetype` classifies (Gotcha/Decision/Convention/ReviewFinding…).
- `query` → **explicit recall** (top-k). But **ambient recall is automatic in
  `ask`** — every pack injects relevant facts — so recall is rarely called
  explicitly; `remember` is mostly for persisting.
- `outcome={id, useful|dead_end|corrected}` → outcome-driven promotion/decay and
  "known dead ends" the agent won't re-derive (the graphify-reflect lesson).
- `relate={from,to,kind}` → typed edge.
- Clean put/get duality (2 ops, not `notes`' 11 — acceptable, unlike a
  mode-enum). Admin (`gc`/`export`/`import`/`consolidate`/`pin`/`relations`) →
  CLI-only, off the agent surface.

### Envelope per verb

| | `ask` | `look` | `act` | `remember` |
|---|---|---|---|---|
| `trust.provenance` | exact/semantic/name-based (+confidence) | exact (graph: name-based flag) | exact | exact |
| `handles` | yes (bodies deferred) | expands them | — | — |
| `cost` | tokens + dropped | tokens (+downgrade note) | saved_pct | — |
| session dedup | yes | yes | — | — |
| `next[]` | shape follow-up | rarely | remember-gotcha? | — |

## What dies (as agent-facing tools)

`repo_map`, `search`, `locate`, `brief`, `review_diff`, `verify_change`, `deps`,
`routes`, `check`, `plan_rename`, `rehearse_patch`, `status`, `session`,
`checkpoint`, `similar`, `cohort`, `refs`, and the 11-action `notes` surface.
They become: intents/anchors of `ask`, views of `look`, `act`/`remember`, or the
**CLI/expert reporting overlay** (`smells`/`clones`/`clusters` are analyst
reports a human runs, or evidence `ask("review")` inlines — not everyday agent
surface). `notes` admin verbs (gc/export/import/consolidate/pin) → CLI only.

## The router gate

The whole design rests on `ResolveIntent` picking the right shape and the anchor
type disambiguating cleanly. If the router misfires, the agent gets the wrong
pack. **Do not collapse to four verbs on faith** — gate the cutover on a
measured router-accuracy harness (labelled goal→intent set, target accuracy
threshold). This is the one thing to harden first. Tracked separately.

## Fixed surface per profile

Replace the 4-boolean matrix with named profiles; each → one predictable set.
`DEX_EXPERT` stays an additive *reporting* overlay, never a different shape of
the everyday surface.

| Profile | Backends | Surface |
|---------|----------|---------|
| `full` | embed + chat | ask · look · act · remember |
| `bm25-only` | no embed | same 4; `ask` degrades to lexical, no synthesis |
| `lean` | minimal/weak model | ask · look · act · remember (ask returns hits-only) |

The four verbs are constant across profiles; only `ask`'s internal capability
degrades. The agent's mental model never changes with deployment.

## Migration plan (maps to alehatsman/dex#110)

Incremental, additive-first, each step shippable. Destination = four verbs;
route = collapse toward it while keeping old names as aliases through one
deprecation window.

1. **Wire the Trust envelope** through existing packs (finish #95c/#101) — the
   envelope spine; prerequisite for everything, valuable alone.
2. **Universal envelope + handles + cost** — standardize every response;
   make handle-based progressive disclosure the default.
3. **Session spine** — make seen/delta/dedup mandatory on every response.
4. **`ask` merge** — intent-routed; brief/ask/search/locate/repo_map/review_diff
   become aliases → anchors/shapes. Composition already lives in `brief`.
5. **`orient` + `review` intents** — new policies; build/run/test extraction;
   on-disk render; diff-anchored review; retire the #83 string-sniff.
6. **`look` merge** — read/grep/trace under one exact fetch verb with `view`.
7. **`act` / `remember` merges**; `notes` admin → CLI.
8. **Profiles** replace the 4-flag matrix. **(done, #148)** `registerTools` no
   longer branches the verb set on `weakModel`: the four verbs (ask·look·act·
   remember) register in every profile — this fixed a drift where a weak local
   model got ask·look·act but not `remember` (stale from before #147 folded it in).
   DEX_EXPERT is an additive overlay, orthogonal to the profile. Invariant locked by
   `TestFourVerbsConstantAcrossProfiles` (in-memory transport drives the otherwise
   process-global weak profile). Vestigial `registerTools` params (weakModel,
   chatAvailable) left for a separate cleanup.
9. **Router-accuracy harness** — gates the final alias removal (four-verb cutover). **(done, #9)**
10. **Anti-accretion lint** — standing guard. **(done, #10)**

## Edge cases

- No chat + fuzzy synthesis requested → `ask` degrades to a pack, `hint` names
  the missing backend.
- No embed → `bm25-only`; `ask` routes to lexical hits.
- `orient` with no tasks.yml/Makefile → omit commands, don't fabricate;
  `Concerns.Dropped` names it.
- `review` on a diff touching unindexed files → partial pack + `RecallPartial`.
- `ResolveIntent` low confidence → `ask` defaults to the safe pack shape, never
  guesses synthesis.
- Ambiguous anchor (a string that is both a path and a symbol) → response echoes
  the interpretation it chose and offers the other in `next`.
- Alias called during deprecation → identical behavior + one-line deprecation
  `hint`.

## Validation

- **System-prompt budget** — assert the four default tool schemas total under a
  ceiling (target: <1k tokens vs today's ~3–5k).
- **One-fuzzy-verb invariant** — only `ask` may return `confidence`/`semantic`
  provenance; `look`/`act`/`remember` responses assert `provenance:exact`.
- **Trust-envelope coverage** — every response populates `trust`; asserted.
- **Envelope uniformity** — every verb returns the same top-level shape (golden).
- **Surface-contract golden per profile** — `full`/`bm25-only`/`lean` assert an
  exact tool-name set (extends `internal/mcp/testdata/tool_schema_contract.json`).
- **Router accuracy** — labelled goal→intent harness meets threshold before the
  four-verb cutover.
- **Intent-policy completeness** — every job resolves to an `EvidencePolicy`
  row; no string-sniffed intents.
- **Anti-accretion lint** — description contains no sibling-negotiation phrase
  ("use X instead", "when Y is overkill", "primary entry point" on >1 tool).
- **No capability regression** — every pre-redesign capability reachable via an
  intent, a `look` view, `act`/`remember`, an `advanced` field, or the expert
  overlay.

## Open questions

- `debug` as a first-class intent now or phase 2? (Leaning: phase 2.)
- `orient` on-disk artifact: single `DEX_MAP.md` (greppable, one file) vs a
  crawlable wiki? (Leaning: single file.)
- Does `look` graph navigation (`view:impact`) return `tests_to_run` inline, or
  is that `ask("review")` only? (Leaning: both — it's deterministic from the
  changed set.)
- Deprecation window length before the four-verb cutover — one release vs two.
