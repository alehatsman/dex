---
id: two-verb-surface
status: proposed
supersedes: tool-surface
last_verified: 93c2d98
owners: [aleh]
covers:
  - "internal/mcp/server.go"
  - "internal/mcp/verbs.go"
  - "internal/mcp/tool_desc.go"
  - "internal/retrieve/intent.go"
  - "internal/retrieve/policy.go"
  - "internal/retrieve/pack.go"
tracking: alehatsman/dex#195
---

# Two-verb surface — `query` · `record`

## Goal

Collapse the agent-facing surface to **two verbs**:

- **`query`** — read the intelligence (code structure, call graph, semantics,
  and accumulated findings), routed by input shape, provenance-graded per item.
- **`record`** — add to the intelligence: persist a durable finding the code
  itself can't tell you (a decision, a gotcha, a dead end), or correct a stale
  one.

This is the terminus of the arc `26 tools → 4 verbs (#110, shipped) → 2 verbs`.
It is a **façade + classifier collapse over a shipped engine**, not a rebuild.
The engine is unchanged: dex already is an `intent → evidence → pack` machine
(`ResolveIntent` `intent.go:125` → `PolicyFor` `policy.go:132` → the assembly
lanes → `ContextPack` `pack.go:13`). `query` merges the two read verbs
(`ask` + `look`) into one classifier over that engine; `record` renames
`remember`. `act` and every non-code-intelligence surface are removed.

### The one principle

**dex is a pure advisory code-intelligence layer. It reads and knows; it never
acts on the world.** Editing source, running tests, git, cross-agent
coordination — the harness's job, not dex's. A pure advisory intelligence layer
has exactly two effects: *read the intelligence* and *add to the intelligence*.
Everything that is not one of those two is out of scope, by construction.

## Non-goals

- Re-architecting retrieval / graph / assembly. The lanes, `evidencePolicies`,
  and the `ContextPack`/`Trust` types stay. This is a surface change.
- Removing *capability*. Every read job survives as a lane of `query`; every
  write job as `record`. What's removed is *effects that aren't code
  intelligence* (exec, swarm, session admin), not retrieval power.
- A new retrieval model. No new intents, no new store tables. `query`'s
  classifier is `look`'s target-classifier ∪ `ask`'s `ResolveIntent`, wearing
  one name.

## Why two — not four, not one

**Not four.** The shipped four (`ask · look · act · remember`) carry two
accidental verbs. `ask` and `look` are the *same act* — read the intelligence —
split only by *how precise the input is*. An agent naming a symbol and an agent
describing a behavior both want the best dex knows about that thing; forcing them
to pre-decide "am I being precise or fuzzy?" is a routing burden dex can carry
better than the agent (dex sees the input shape; the agent has to guess which
tool the shape belongs to). Merge them: one verb, precision inferred from input.
And `act` (run a command) touches the *world*, not the intelligence —
retrieval-irrelevant, and the harness runs commands better. It goes.

**Not one.** Folding `record` into `query(fact=…)` is exactly the `notes`
mode-enum anti-pattern the accepted spec already rejects (`tool-surface.md`
§"Why this is not the notes mode-enum anti-pattern"): a required mode selector
choosing between unrelated read/write operations with disjoint params. Keep them
separate. `record` earns its own verb because it touches dex's *own* knowledge
(not the world), it is retrieval-integrated (a scoped fact surfaces ranked next
to the code it's about — the harness cannot do that), and it captures agent
*judgment* that code-derivation never can. Killing it makes dex a read-only
index mirror that never gets smarter from use.

---

# How `query` works — the precision ladder

This is the core of the design: a single verb whose **output precision tracks
input precision**. The agent hands `query` one string. dex classifies the
*shape* of that string to a **lane**, and the lane determines what gets
retrieved and how precise the answer is. You cannot get a semantic ramble from a
literal path, or false precision from a vague question — the shape of what you
asked *is* the contract for what you get back.

The ladder runs from most-precise input (a literal artifact) to least-precise
(open prose). Every rung maps to a lane that **already exists** in the engine —
this section names the rung, its trigger shape, the engine path it routes to,
and the provenance it carries.

```
input shape ────────────────────────────────────► lane ─────────► provenance
path                         internal/watch/x.go   read            exact (bytes)
path:N-M                     x.go:120-140          locate/slice    exact (bytes)
/regex/  ·  pattern:         /flush\(/             grep            exact (matches)
handle                       h:3f9a…               expand          exact (bytes)
symbol                       (*Watcher).flush      graph           exact | name-based
symbol + want=callers|…      Foo want=impact       graph-directed  exact | name-based
prose, no symbol             "how are edits debounced"  semantic   semantic (+conf)
imperative edit verb         "refactor the flush path"  editing    semantic + graph
want=assemble                <task> want=assemble  assemble        semantic + graph
"how does X work" / arch     "architecture of watch"    architecture  graph (structural)
"packages" / topology        "package dependency graph" topology    graph (structural)
no subject / "orient me"      "understand this repo"     orient      structural (deterministic)
diff                         staged · HEAD · <patch>    review     mixed (per-finding)
```

### Rung 0 — Literal artifact fetch (exact lane, zero inference)

The most precise input is a thing you can already name. dex does not think; it
fetches.

- **`path`** (contains `/` or a known file extension) → read the file. Default
  facet for a whole file is `signatures` (~10× smaller than full; `want=full`
  for bodies, `want=map` for imports+exports, `want=skeleton` for structure).
- **`path:N-M`** → slice exact lines (locate). Default facet = the lines
  themselves.
- **`/regex/`** or **`pattern:`** → grep (RE2), exact matching lines.
- **`handle`** (opaque token from a prior response) → expand that exact range.

Provenance is **exact** — bytes off disk / matches from the index. No model, no
ranking, no confidence. This rung *is* today's `look` read/grep/locate handlers,
unchanged. It is the floor: naming a thing gets you the thing.

### Rung 1 — Named symbol → structural facts (graph lane)

A bare or qualified identifier (`Foo`, `(*Watcher).flush`, `mcp.NewServer`) is
still precise — it names a node in the call graph — but what you want is its
*structure*, not its bytes. **This is the narrow default that makes the merge
worth doing: a named symbol returns just its call graph, never a fused semantic
bundle.** The precision of "I named exactly this symbol" is honored with an exact
structural answer, not a probabilistic neighborhood.

- Default facet: the symbol's signature + immediate neighborhood (callers ∪
  callees, one hop). Routes to `IntentSymbolLookup` (`GraphLaneNeighborhood`,
  `BodyFillSymbols`).
- **`want=callers`** → inbound edges + risk tier (`graphCallers`).
- **`want=callees`** → outbound edges (`graphCallees`).
- **`want=impact`** → transitive blast radius + risk tier + `tests_to_run`
  (`graphImpact`).
- **`want=path` + `to=<sym>`** → shortest call route (`graphPath`).

Provenance is **exact** for Go (type-resolved edges, `Trust.GraphResolved`) and
carries a **`name-based`** flag for tree-sitter languages (`Trust.RecallPartial`
+ the grep-augmented `GrepHits` and `UnresolvedInbound` recall backstops already
in `traceVerb`). This rung is today's `trace` + symbol lane, reached by shape
instead of by a separate verb.

### Rung 2 — Behavior / concept search (semantic lane)

Prose that names no symbol ("how are edits debounced", "where do we validate the
token") is the first genuinely fuzzy input. Now — and only now — dex reasons.

- Routes to `IntentBehaviorSearch` (the default): fused semantic + symbol +
  one-hop graph neighborhood, `capsTargeted`, no body fill. Returns a ranked
  evidence pack: hits with signatures, best symbol body, scoped facts.
- Provenance is **semantic**, carrying `Trust.TopScore` / `Trust.Confidence` /
  `Trust.LowConf`. The confidence is *in-band* so the agent believes the pack
  and stops re-grepping — the whole point of a code-intel tool.

### Rung 3 — Editing context (edit / assemble lanes)

An imperative edit phrasing ("refactor the flush path", "add a field to
Config") wants the same neighborhood *plus* the annotations you need to change
code safely.

- Auto: `IntentEditingContext` — neighborhood + per-file annotations (owners,
  sibling tests, last commit, nearest rules doc; `PathMeta`).
- **`want=assemble`** (explicit only, never auto-routed) → `IntentAssemble`: a
  budget-bounded working set — ranked files + coverage-ordered symbol bodies +
  spreading-activation related files + the rules that govern them
  (`capsAssembleDense`, `BodyFillCoverage`). This is the task-start pack.

Provenance is **semantic + graph**; `Concerns{Covered, Dropped}` reports what
the byte budget left out (an honest partial beats a false floor).

### Rung 4 — Architecture / topology / orientation (structural rollup lanes)

The least-precise inputs are questions about the *whole* — how a subsystem is
shaped, how packages depend, where to start. These do not want file bytes; they
want structure computed off the graph.

- **`IntentArchitecture`** ("how does X work", "design of X", "walk me
  through") → `GraphLaneArchitecture`, `capsDense`, 900 answer tokens.
- **`IntentPackageTopology`** ("packages", "dependency graph", "import graph")
  → `GraphLanePackageTopology` — the workspace-project DAG + fan-in, gated on
  `resolve.IsWorkspaceRoot`.
- **`IntentOrient`** (no subject / "understand this repo" / "orient me") → the
  deterministic L0/L1 map (Louvain communities + PageRank) + build/run/test
  commands + entrypoints + conventions. **No synthesis** — structural facts, not
  prose. This is today's `map` verb.

Provenance is **graph / structural** — deterministic for orient and topology
(no model), so it is trustworthy without confidence caveats.

### Rung 5 — Review (diff-anchored)

A diff input (`staged`, `HEAD`, `working`, or unified-diff text) anchors a
review: the input *is* the change, not a task string.

- Routes to `IntentReview`: per-hunk review + blast radius of changed symbols
  (`trace impact` → risk tier + `tests_to_run`) + covering tests + scoped
  gotchas on touched files + convention/duplication divergence.
- Provenance is **mixed, per-finding** (proven vs name-based edges flagged on
  each finding). Puts the literal `tests_to_run` in `next[]` — but as query
  inputs / suggested commands the *agent* runs, never as a dex-run step.

---

## The two dials

`query` is driven by input shape, but both routing decisions are overridable —
always, explicitly, never silently.

- **`kind`** — force the **lane** (the rung). Overrides shape detection:
  `kind=read|grep|locate|symbol|callers|callees|impact|path|search|
  editing|assemble|architecture|packages|orient|review|recall`. This is
  `look`'s target-type and `ask`'s `intent` unified under one name. When the
  same string is both a path and a symbol, `kind` is how the agent settles it.
- **`want`** — pick the **facet** within a lane: for a file
  `signatures|skeleton|map|full|lines`; for a symbol `callers|callees|impact|
  path`; for a pack `assemble|answer`. This is `look`'s `view` and `ask`'s
  `shape`.

Everything else is one signature:

```
query(input, kind?, want?, to?, budget_tokens?, project_root?)
```

`to` is only meaningful for `kind=path`/`want=path`. `budget_tokens` bounds the
pack and reports what it dropped. `project_root` is the worktree escape hatch
(the server can't see the shell's cwd).

`record` stays small and put/get-dual:

```
record(fact?, query?, scope?, archetype?, supersedes?, outcome?, relate?)
```

- **`fact`** → write a durable finding. `scope` binds it to a path/glob so it
  surfaces on touch; `archetype` classifies (Gotcha/Decision/Convention/
  ReviewFinding); `supersedes=<id>` corrects a stale one.
- **`query`** → explicit recall. But recall is *also* ambient: every `query`
  pack injects the relevant facts, so explicit recall is rare. (`query` the read
  verb and `record(query=…)` recall are distinct: the former reads *code*, the
  latter reads only *memory*. `query(kind=recall)` is the memory-only lane on the
  read verb for symmetry; `record(query=)` remains for write-adjacent recall.)
- **`outcome={id, useful|dead_end|corrected}`** → outcome-driven promotion/decay
  so the agent won't re-derive known dead ends.
- **`relate={from,to,kind}`** → typed edge.

Admin (gc / export / import / consolidate / pin / relations) is CLI-only, off
the agent surface — as it already is under `remember`.

---

## The universal envelope

Unchanged from the accepted spec (`tool-surface.md` §"the universal envelope") —
both verbs return one shape:

```
{ route,                                       // what dex decided and why
  result,                                      // lane-specific payload
  trust:  { fresh, indexed_at, confidence,     // in-band; never re-grep to verify
            provenance: exact|semantic|name-based|graph|memory, caveat },
  cost:   { tokens_returned, budget_left, saved_pct },
  next:   [ {query, why} ],                    // ready-to-run query inputs
  handles:[ ... ] }                            // pointers to more, NOT the bytes
```

Two things the two-verb model sharpens over the four-verb envelope:

- **`route` is legible.** It echoes the detected shape, the lane chosen, and the
  alternative it did *not* take: `route:{input:"flush", detected:"symbol",
  lane:"callers", alt:[{kind:"search", why:"treat as a behavior query"}]}`. No
  silent guessing — when input is ambiguous the response says which
  interpretation it took and offers the other in `next[]`.
- **`trust.provenance` is per-item, not per-response.** A single pack can mix an
  exact grep hit, a semantic hit, a name-based graph edge, and a memory fact.
  Each item carries its own provenance. This is the moat: deterministic,
  provenance-labeled retrieval the harness cannot fake.

---

## Honest failure taxonomy

Every `query` failure resolves to one of four distinct statuses — never a silent
empty result (the #161 empty-index trap: an empty index and a true no-match must
not look alike):

| status | meaning | agent's move |
|---|---|---|
| `no-index` | repo not indexed / no `index.include` match | run `dex index` (in `next[]`) |
| `no-match` | indexed, searched, found nothing | broaden, or try the `alt` lane in `route` |
| `ambiguous` | input fits >1 lane (path *and* symbol) | response took one, offers the other in `next[]` |
| `stale` | index behind the working tree | results valid-as-of `indexed_at`; grep is disk-authoritative |

`stale` is not a failure of retrieval — it is a *freshness caveat* carried in
`trust`. The exact lanes (read/grep) are disk-authoritative and never stale;
only the graph/semantic lanes can lag the tree.

---

## What is removed (and why it isn't a regression)

dex has a single consumer (this harness), so removal is **outright deletion** —
no aliases, no shims, no deprecation window, no migration path. Removed as
**agent-facing effects that are not code intelligence** — every one is either the
harness's job or a separate product:

- **`act` / shell / verify / checkpoint** — running commands touches the world.
  Test *selection* survives as `query` review/impact → `tests_to_run`; *running*
  is the agent's. (S3a)
- **The swarm surface** shipped by #169 — `dex agent` verb/CLI,
  `agent_msg_vecs` / `peer_claims` / `share_cache` tables, `foldPeerFindings` in
  `ask()`, `warm_cache`, `DEX_AGENT_ID` / `DEX_SWARM_WARMCACHE`. Cross-agent
  identity and coordination are not single-repo code intelligence. Removed from
  the tree, not parked in place. The *ideas* may live in their own focused
  agent-coordination tool someday — a separate product — but must never re-enter
  dex's scope. (S3b)
- **`session` / `checkpoint` admin** as agent-facing tools — session dedup stays
  as an *internal* envelope mechanism (seen/delta), not a verb.
- **`notes` 11-action admin surface** — folded to `record`'s put/get; admin →
  CLI.

No *read capability* is lost: every retrieval job is a lane of `query`; the
DEX_EXPERT power lanes (deps/clusters/smells/clones/similar/refs/routes) remain
as an **additive reporting overlay**, unchanged, orthogonal to the two verbs.

---

## Roadmap (maps to alehatsman/dex#195)

Surface collapse over a shipped engine. **No backward compatibility, no aliases,
no deprecation window** — dex is a single-consumer tool (this harness), so old
verb names are deleted outright in the same slice that replaces them. Every
slice is a clean break.

- [ ] **S1 — this spec.** Two verbs, advisory-only principle, the precision
      ladder, per-item provenance, failure taxonomy, removals scoped. Gates the
      rest. *(this document)*
- [ ] **S2 — `query` = merge `ask` + `look`.** One classifier = `look`'s
      target-type detector ∪ `ResolveIntent`; narrow-default routing (symbol →
      graph, not fusion); `kind` / `want` overrides; the legible `route`.
      `ask`/`look` collapse into `query`'s internal handlers and their tool
      registrations are **deleted** — not aliased.
- [ ] **S3 — Remove what isn't code-intelligence.** (a) drop `act`/shell/verify/
      checkpoint; (b) remove the swarm surface from the tree (confirm exact
      extent first). Both are deletions, not refactors.
- [ ] **S4 — `remember` → `record`.** Rename; recall stays folded into `query`;
      drop the `notes`-admin and `session` agent surfaces.
- [ ] **S5 — Cutover gate.** Reuse #110's router-accuracy harness (labelled
      input→lane set, threshold) — the *same* gate, now scoring shape→lane.
      Assert the two-verb system-prompt budget (target: well under the four-verb
      ~800 tokens) and that **only two tools register** (no old verb reachable).
      No alias step — the old names were deleted in S2–S4.

---

## Validation

- **Router accuracy (the gate).** A labelled `input → lane` corpus spanning
  every rung of the ladder (literal path, `path:N-M`, regex, bare symbol,
  qualified symbol, behavior prose, edit phrasing, architecture question,
  topology question, orient, diff) meets a threshold before the cutover. This is
  #110's harness, re-pointed. **Do not collapse to two verbs on faith.**
- **Precision-tracks-input invariant.** A literal `path`/`path:N-M`/regex input
  returns `provenance:exact` and never a synthesized answer. A bare symbol
  returns a graph result, not a fused pack (the narrow-default guarantee).
- **Per-item provenance coverage.** Every result item carries a provenance tag;
  asserted. No response-level-only provenance.
- **Failure-taxonomy distinctness.** Empty index → `no-index`, not `no-match`
  (regression-locks #161).
- **System-prompt budget.** Two tool schemas total under the four-verb ceiling.
- **No read-capability regression.** Every pre-collapse read job (the 11 intents
  + every `look` view + trace direction) resolves to a `query` lane; golden
  test enumerates them.
- **Removal completeness.** After S3, no `act`/swarm symbol remains reachable
  from the agent surface; `grep` for `DEX_AGENT_ID` / `foldPeerFindings` /
  `agent_msg_vecs` returns nothing in the shipped surface.
- **Anti-accretion lint.** Neither verb description contains a
  sibling-negotiation phrase; standing guard (already shipped, #155).

## Open questions

- Does `query(kind=recall)` (memory-only read on the read verb) earn its place,
  or is `record(query=)` the single recall entry point? (Leaning: keep both —
  symmetry is cheap, and a memory-only read from the read verb is intuitive.)

*(Resolved by the "no backward compatibility" directive: no deprecation window —
old verbs deleted in the slice that replaces them; the removed swarm surface is a
clean break, no migration note.)*
