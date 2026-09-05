---
id: query-unification
status: accepted
owners: [aleh]
covers:
  - "internal/mcp/query.go"
  - "internal/mcp/http.go"
  - "cmd/dex/main_ask.go"
  - "cmd/dex/main_search.go"
  - "cmd/dex/verbs.go"
tracking: alehatsman/dex#848
supersedes: tool-surface (CLI/REST halves only — the MCP half of that arc stands)
---

# Query unification — one interface, three transports

## Problem

MCP, CLI, and REST are believed to be "the same dex" wearing three transports.
They are not. Audited against the current tree (2026-09-06):

- **MCP** is the one transport that actually collapsed. The default tool
  (`query`) runs a single classifier (`classifyQuery`, `internal/mcp/query.go:257`)
  through a single dispatcher (`(*Server).Query`, `query.go:349`). This is real,
  shipped, and router-accuracy-gated (`specs/two-verb-surface.md`).
- **REST never got the collapse.** `internal/mcp/http.go:249-280` registers
  **28 endpoints** — one per legacy capability (`/trace`, `/locate`, `/find`,
  `/grep`, `/cohort`, `/refs`, `/smells`, `/clones`, `/routes`, `/deps`,
  `/callers`, `/callees`, `/impact`, `/clusters`, …). Exactly **one**
  (`POST /v1/projects/{id}/query`, `http.go:255`) goes through `Query()`. REST
  is the widest, least-unified surface dex has.
- **CLI never got it either, and has drifted from itself.** `cmdAsk`
  (`cmd/dex/main_ask.go:16`) imports `internal/mcp` but drives it through a
  **different, older** vocabulary — `--intent
  auto|behavior_search|symbol_lookup|callers|callees|architecture|
  package_topology|editing_context|assemble` — not `query`'s `kind=` ladder
  (`read|grep|locate|symbol|callers|callees|impact|path|search|editing|
  assemble|architecture|packages|orient|review`). `cmdSearchSemantic`
  (`main_search.go:18`, the `search` verb) doesn't touch `internal/mcp` at
  all — it reimplements semantic search directly against `internal/embed` +
  `internal/store`. Two competing classifiers coexist in one binary, and one
  verb bypasses both.

Net effect: three interfaces, three different shapes of "ask dex a thing",
one of which (CLI) contains two more shapes internally. This is a large slice
of why the project reads as unshaped — not the retrieval engine (which is
tested and calibrated), the *doors into it*.

## Goal

**One request shape. One dispatcher. Three transports thin enough that none
of them can drift again.**

- The request shape is `(input, kind?, want?, to?, budget?, project_root?)` —
  already `QueryInput` (`query.go:39-61`). No transport gets its own
  parameter vocabulary.
- The dispatcher is `(*Server).Query`. Every transport's only job is:
  parse-my-format-in → call `Query` → format-my-format-out. No transport
  reimplements routing, and no transport calls a lower-level internal
  (`internal/embed`, `internal/store`, `internal/graph`, …) directly.
- CLI's target shape: `dex query <input> [--kind] [--want] [--to] [--budget]
  [--project-root]`, plus a short, fixed, non-retrieval lifecycle list (see
  below). Named verbs that duplicate a `kind=` value are deleted, not
  aliased — matching the project's existing no-deprecation-window norm
  (`specs/two-verb-surface.md` §Roadmap).
- REST's target shape: `POST /v1/projects/{id}/query` is the only
  project-scoped, capability-carrying route. `health`/`version`/
  `list-projects`/`status` remain (they aren't retrieval, see Non-goals).
- MCP's target shape: unchanged in spirit — default `query`, DEX_EXPERT
  overlay — but every expert tool is re-justified against §Classification
  below instead of grandfathered.

## The six use cases the shape must serve

Not a fresh guess: distilled from the ladder that's already shipped and
gated (`specs/two-verb-surface.md` §"How query works"), cross-checked against
usage telemetry (`dex feedback --verbs`, #812/#813 — every DEX_EXPERT power
lane is the *never-called-in-real-traffic* set) and nav-bench (#351, the one
place additional lanes were shown to earn their keep).

1. **Exact-fetch** — a file, a line range, a regex match. Zero inference,
   disk-authoritative. (`kind=read|grep|locate`)
2. **Symbol structure** — callers / callees / impact / shortest path off the
   call graph, keyed by a named symbol. (`kind=symbol|callers|callees|impact|path`)
3. **Behavior search** — prose naming no symbol → ranked semantic evidence.
   (`kind=search`)
4. **Task-start working set** — the assemble bundle: ranked files + bodies +
   governing rules, budget-bounded. (`kind=assemble`)
5. **Change review** — a diff in, blast radius + tests-to-run + gotchas out.
   (`kind=review`)
6. **Repo orientation** — first-touch map of an unfamiliar area: entrypoints,
   layers, clusters. (`kind=orient|architecture|packages`)

Everything a transport exposes that isn't one of these six needs a specific
justification in §Classification, not a "well, it's useful" wave.

## Classification — what happens to everything else

### CLI verbs (`cmd/dex/`, ~40 verbs today)

| Verb(s) | Disposition | Why |
|---|---|---|
| `ask`, `search`, `read`, `locate`, `trace`, `grep`, `review_diff`, `status`, `repo_map` | **Deleted → `dex query --kind=...`** | Each is a named front door to a use case `query` already serves. `ask`'s intent enum and `search`'s standalone reimplementation are the drift this spec exists to close. |
| `check` | **Deleted → `dex query --kind=check`** (new kind) | Read-only verification, fits the shape; needs a `claims` param — see §Open questions on non-scalar inputs. |
| `index`, `watch`, `reindex`, `nuke`, `clone`, `summarize` | **Stay separate.** | Mutating the index, not reading it. Folding a write into `query` is exactly the notes mode-enum anti-pattern `two-verb-surface.md` already rejected for `record`. |
| `setup`, `doctor`, `env`, `config`, `mcp`, `serve`, `hook`, `completion`, `version` | **Stay separate.** | Process lifecycle / configuration, not retrieval. |
| `bench *` (8 subcommands), `compact`, `compress` | **Stay separate, out of scope for this spec.** | Development/measurement tooling, not the agent- or human-facing product surface. Revisit under a future "trim the eval/bench footprint" pass (flagged in the parent conversation, not this issue). |
| `refs`, `cohort` | **Deleted → `dex query --kind=refs\|cohort`** | Corrected below — these fit the shape better than I first thought. |
| `plan_rename`, `rehearse_patch` | **Stay separate — different contract, see below.** | |
| `graph neighbors\|deps\|links\|backlinks\|tags\|export` | **`deps` folds into `kind=deps`; the rest stay CLI-only admin/debug, out of scope.** | `deps` is a plain read keyed by file/package — fits. The others are inspection/debug utilities with no measured usage. |

**Hidden consumer, not just human muscle memory:** `cmd/dex/hook_rewrite.go`
is a shipped `PreToolUse(Bash)` hook that rewrites `grep`/`rg` shell commands
to `dex search [PATH] "PATTERN"` **by that literal CLI verb name**. Deleting
`search` without accounting for this breaks the hook silently for every
project that has it installed — an internal caller with the same claim on
correctness as a test, not covered by "no human aliases needed."

Two ways to close this, and **drop is the better one**: transparently
rewriting the agent's own bash commands is exactly the "magic" the
project's `explicit>magic` bar argues against — the agent doesn't choose to
call dex, its command gets silently swapped underneath it. `dex query`'s own
MCP-primary framing (constitution.md: "MCP is the primary interface... other
entry points exist to support that use") means the honest fix is to retire
`hook_rewrite.go` outright as part of this slice, not migrate its rewrite
target to `dex query --kind=search`. Migrating keeps a magic behavior alive
through the collapse for no reason the six use cases require; dropping it is
one more piece of accreted complexity gone, consistent with what this whole
spec is for. Whichever way it goes, the implementation issue must say so
explicitly and account for `hook_inject.go` / `setup.go`-generated CLAUDE.md
snippets referencing the same hook in the same commit — not discovered
after.

**Standing CI gate that this plan currently fails:** `cmd/dex/verb_parity_test.go`
(`TestMCPToolCLIParity`, #494) asserts every MCP tool has a reachable
top-level CLI verb or `dex graph <sub>` subcommand. As written, deleting
`search`/`trace`/`locate`/`grep`/etc. as CLI verbs while they remain MCP
tools fails this test outright. The CLI implementation issue must either
redefine "reachable" to accept `dex query --kind=X` as a valid front door, or
retire this test in favor of a gate that gates ladder-coverage instead of
verb-name parity — a decision this spec should make explicit, not leave for
whoever writes the code to discover via a failing test.

### REST routes (`internal/mcp/http.go`, 28 today)

Every route whose handler has a CLI/MCP equivalent already covered by a
`kind=` value collapses into `POST /v1/projects/{id}/query` with that `kind`
in the body. That's `/ask`, `/find`, `/grep`, `/read`, `/ls`, `/trace`
(replacing `/callers`, `/callees`, `/impact`, `/path`), `/deps`,
`/graph/packages`, `/review`, `/diff`, `/cohort`, `/refs`. Kept separate:
`/healthz`, `/version`, `/projects`, `/status`, `/index-status` (not
retrieval), and `/refactor`→`plan_rename` (plus `rehearse_patch` needs a
route added if REST is meant to carry it at all — TBD, see Open questions).
`/routes`, `/smells`, `/clones`, `/similar`, `/clusters` fold into `query`
only if the "graph-wide report" question below resolves toward folding;
otherwise they're deleted from REST the same as the CLI power lanes, on the
same never-measured-usage basis.

### MCP expert tools (19 today) and the "different contract" two

**Correction from first draft:** I originally filed `refs` and `cohort`
alongside `plan_rename`/`rehearse_patch` as "different contract." On review
that's wrong — I let "Go-only, niche" substitute for "doesn't fit the
shape," and those aren't the same test. `refs(symbol, action)` maps directly
onto `query(input=<symbol>, kind=refs, want=references|implementations|
supertypes|subtypes)` — `action` is exactly what `want` already means for
the `callers`/`impact`/`path` facets. `cohort(interface)` is simpler still:
`query(input=<interface>, kind=cohort)`, no new field. Both fold in.

Only `plan_rename` and `rehearse_patch` genuinely don't fit
`(input) → precision-tracked read`:

- They return **edit plans** (byte-range splices) or **hypothetical
  type-check results** — not a read of the current state of the code, a
  computation *about* a proposed change. Different contract, different trust
  story (`etag`/staleness on a plan, not on a fact).
- `plan_rename` also needs a destination name (`to`) beyond what any read
  facet requires, and `rehearse_patch` needs an edit-plan payload
  (`edits[]`/`files[]`) — genuinely different input shape, not just a
  different `kind` value.

**Recommendation: these two stay separate tools/verbs/routes, unchanged.**
Don't force a fourth shape into one interface just to hit "one verb" as a
slogan — the constitution's own bar is "advisory, read the intelligence",
not "syntactically identical everywhere." The unification is about deleting
*duplicate* doors, not amputating tools with a genuinely different job.

`routes` / `smells` / `clones` / `similar` / `clusters` are whole-repo
structural reports, not lookups keyed by one input. They *could* become
`kind=` values if `QueryInput` grows report-specific optional params
(`threshold`, `min_lines`, …) — cheap for the type, but it starts eroding
"one shape, no bespoke params per door." Left as an open question rather than
resolved here (see below); either answer is coherent, but pick one instead
of drifting into the current default of "yes, as separate tools, because
that's how the last one was added."

## Non-goals

- **Rewriting the retrieval engine.** `classifyQuery`, the lanes, and
  `ContextPack` are unchanged. This is a transport/surface change over a
  calibrated engine, exactly like `two-verb-surface.md` was.
- **Touching `internal/eval` / `internal/bench` / the 8 `bench_*` CLI
  verbs.** Flagged in the parent conversation as its own oversized area;
  deliberately out of scope here to keep this slice reviewable.
- **A language rewrite.** Nothing here requires leaving Go; the fix is
  deleting duplicate transport code and pointing every door at the engine
  that already exists.
- **Backward compatibility.** Per the project's standing norm (single
  consumer, no deprecation window): deleted verbs/routes are deleted
  outright in the same slice that replaces them, not aliased or shimmed.

## Validation

- **No capability regression.** Every one of the six use cases, reachable
  today by at least one transport, remains reachable by all three after the
  slice that removes its old door.
- **Router-accuracy harness extended, not replaced.** The existing
  `input → lane` corpus (`specs/two-verb-surface.md` §S5) gains CLI-flag and
  REST-body variants of the same inputs, asserting identical `route.lane`
  regardless of transport.
- **Zero non-`query` internal calls from the CLI's retrieval verbs.** A grep
  gate: no `cmd/dex/*.go` file implementing a use-case-1-through-6 verb may
  import `internal/embed`, `internal/store`, or `internal/graph` directly —
  only `internal/mcp`.
- **REST route count.** A test asserting the registered route count for
  `/v1/projects/{id}/*` matches the post-collapse list exactly (today: 28;
  target: `query` + the kept different-contract handful + status/index-status).
- **Anti-accretion lint stays green** (`specs/anti-accretion-lint.md`) — this
  slice should *lower* the offense ceiling, not raise it, since fewer
  competing tools means less to negotiate between.
- **Standing re-accretion guard.** `anti-accretion-lint.md` set the precedent
  of a ratchet test so a fixed sprawl doesn't quietly regrow. This spec
  should add its own: a test enumerating registered CLI verbs, REST routes,
  and MCP tools against a named ceiling per transport (today's counts, once
  this slice lands, become the ceiling). Adding verb #N+1 later means either
  it's a lifecycle command (fine, ceiling for that category moves) or it's a
  new `kind=` value (no ceiling change) or someone has to consciously raise
  the retrieval-surface ceiling in the same PR — mirroring how
  `antiAccretionCeiling` is a single named constant, not a floor that
  silently grows. Without this, the CLI/REST collapse this spec earns is
  just a one-time reset, and the project is back here in a year.
- **hook_rewrite.go dropped (leaning) and verb_parity_test.go redefined, not
  orphaned.** The CLI slice's PR must show the hook's removal (or, if the
  sign-off goes the other way, its migrated rewrite target) and the parity
  test's new definition of "reachable" in the same diff as the verb
  deletions — not as a follow-up "oops" fix.

## Open questions — resolved

1. Zero-subject reports (`routes`/`smells`/`clusters`) stay separate;
   input-anchored (`clones`/`similar`) fold into `query`.
2. `check`'s `claims` array gets its own field on the request shape — one
   deliberate, documented exception, not a precedent for more.
3. `rehearse_patch` stays CLI/MCP-only; no REST route.
4. Sequencing is **value-first: CLI slice ships before REST and the MCP
   re-justification** — CLI collapse is the change actually asked for; REST
   and MCP re-justification follow it, not precede it.

<details><summary>Original open-questions text (for context)</summary>

1. **Do the whole-repo, zero-subject reports (`routes`/`smells`/`clusters` —
   no required input, they scan the whole graph) fold into `query` with
   `input` meaning "path or empty for whole-repo", or stay separate?** And
   separately: **do the input-anchored pair (`clones`/`similar` — path +
   threshold) fold in more naturally than the zero-subject three, since they
   at least have a real `input`?** These may not have the same answer —
   don't force one verdict across all five just because they were grouped
   together historically. Leaning on the zero-subject three: keep separate,
   since `input=""` meaning "the whole repo" is a special case that doesn't
   exist anywhere else in the ladder. Leaning on the input-anchored two:
   fold in, same reasoning as `refs`/`cohort` above.
2. **Does `check`'s `claims` array (non-scalar input) fit `QueryInput.Input`
   (a string) at all, or does it need its own field?** If it needs its own
   field, that's a crack in "one shape" worth resolving explicitly rather
   than accreting a second optional array param quietly.
3. **`rehearse_patch` over REST** — carry it at all, given it's the most
   niche of the four different-contract tools, or leave REST without it and
   accept CLI/MCP-only for that one capability?
4. **Sequencing** — one big slice per transport (mirrors how the MCP collapse
   shipped, S2–S4 each a clean break) or a longer-lived redirect shim
   *inside this codebase only* (not a public compat layer) to de-risk the
   CLI's higher usage surface? Leaning: match the project's own precedent —
   clean breaks, no shim — but flagging because CLI has real interactive
   users (you) where MCP's collapse had one.

</details>

## Rollout

Each transport becomes its own scoped issue/PR. **Order: CLI → REST → MCP
expert-tool re-justification** — value-first, not risk-first: CLI collapse is
the change actually asked for, so it ships before the two safer, lower-felt
slices rather than after them.

The CLI issue must list the audited hidden consumers as explicit checklist
items up front, not discovered mid-implementation:

- [ ] `hook_rewrite.go` dropped (rewrites `grep`/`rg` → `dex search` by
      literal name; per the decision above, remove rather than migrate)
- [ ] `verb_parity_test.go` (#494) redefined to gate `kind=` ladder coverage
      instead of top-level-verb-name parity
- [ ] `hook_inject.go` and `dex setup`'s generated CLAUDE.md snippets audited
      for stale verb-name references
- [ ] shell completion scripts (`completion_gen.go`) regenerated against the
      new verb set

This spec is `accepted`; the CLI issue can be filed and claimed now.
