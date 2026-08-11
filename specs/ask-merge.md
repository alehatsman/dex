# Spec: ask-merge — the intent-routed front door (progressive disclosure)

Status: proposed
Epic: #110 tool-surface cutover, step 4
Depends on: envelope + look/act/remember (done), router-accuracy harness #9 (done),
anti-accretion lint #10 (done)

## Goal

Make `ask` the single always-on front door for read/understand work, routing to
the lanes that `search`/`trace`/`locate`/`brief`/`repo_map`/`review_diff` serve
today — and demote those specialized tools to **expert-gated power lanes** rather
than deleting them. The agent sees four obvious verbs (ask/look/act/remember) by
default; the power lanes stay reachable behind `DEX_EXPERT` and via ask's explicit
`intent` override. This delivers the small surface the epic wants **without**
losing explicit control or capability (Aleh's call: progressive disclosure, not
full collapse).

Non-goal: reaching exactly four tools. The everyday surface keeps the primitives
an agent genuinely needs every session (`read`, `verify_change`, `shell`, `grep`).

## The structural finding this rests on

`ask` is **not in the everyday surface today**. `registerTools` (server.go:909)
registers `ask` only when `!embedAvailable || expert`; in the common case
(embedder present, non-expert) the de-facto front door is `brief`, and `ask` is
absent. So "make ask the front door" is a real change, not a relabel — and every
demotion below is blocked on it: you cannot move `search` to expert while the only
everyday semantic lane *is* `search` and `ask` isn't registered.

Current everyday lane (the accretion): `search, trace, locate, review_diff,
verify_change, notes, read, brief, look, remember`. Baseline (always): `shell,
act, grep`. Already expert: `repo_map, deps, routes, smells, clones, similar,
cohort, refs, clusters, check, plan_rename, rehearse_patch, status, session,
checkpoint`.

## Design

### Target everyday surface

Front door + primitives only:

| Tool | Why it stays everyday |
|------|----------------------|
| `ask` | the intent-routed front door (always-on) |
| `look` | exact-fetch verb (covers locate/read/grep/trace-callers by target shape) |
| `act` | run-a-command verb (baseline) |
| `remember` | memory verb (baseline of the memory lane) |
| `read` | primitive: exact source fetch by path |
| `verify_change` | change→verify loop; no verb covers it, high-value every session |
| `shell`, `grep` | baseline primitives |

Demoted to expert power lanes (ask routes their intent; still callable directly
under `DEX_EXPERT` and via `intent=`):

| Tool | Covered in everyday by | Route |
|------|------------------------|-------|
| `search` | `ask` | `behavior_search` intent |
| `trace` | `ask` / `look` | `callers`/`callees` intent; `look <symbol>`; `path`/`impact` via `intent=` or DEX_EXPERT |
| `locate` | `ask` / `look` | `orient`/`symbol_lookup`; `look path:line` |
| `brief` | `ask` | folded — `assemble` intent, **after** porting brief's `local_rules` into `ask` (slice-1 dogfood disproved the "pure duplicate" premise: brief uniquely carried project rules). Port-then-remove (#141). |
| `notes` | `remember` | everyday add/query via `remember`; admin/relate lanes stay on `notes` under DEX_EXPERT |
| `review_diff` | `ask` (once review-union lands) | `review` intent; **stays everyday until then** (delta-shaped, no intent yet) |

Result: everyday drops from ~11 to ~7, and the flagship verb is finally present.

### Why this is safe (the reliability guard)

- **Explicit override preserved.** `ask(question, intent="callers")` forces a lane
  exactly like `trace(direction=callers)` did — `ResolveIntent` honours an explicit
  `intent` before the regex. Demotion removes the *default* visibility, not the
  capability. This is the answer to "explicit > magic": the magic (regex routing)
  has an explicit escape hatch, and the power lane is still one `DEX_EXPERT` away.
- **Router measured, not faith.** The 50-case harness (#9, 92%, floor 0.88) gates
  this; demoting a tool requires its intent to be in the corpus and routing
  correctly. Any demotion that would drop accuracy fails CI.
- **`look` catches the shape-routable cases** the regex router is weakest on
  (a bare symbol, a `path:line`) — so the two lanes back each other up.

### brief↔ask: not a pure duplicate (corrected by the slice-1 dogfood)

The original premise — "brief is a genuine duplicate of ask(assemble)" — was
**wrong**, and the slice-1 dogfood caught it: `ask --intent assemble` structurally
lacked `brief`'s `local_rules` (`ContextOutput` had no rules field at all). `brief`
(task→working-set **with the rules that govern it**) and `ask` (question→evidence)
are different jobs. So folding is **port-then-remove**, not delete:

- **2a (done, #141):** add `ContextOutput.Rules`, populated by the existing
  `collectLocalRules(root)` on `intent=assemble`. Dogfood confirms byte-identical
  rule sets between `dex brief` and `dex ask --intent assemble`. Additive; brief
  untouched.
- **2b (done, #141):** with parity proven, removed `brief` — MCP tool +
  `BriefInput`/`BriefOutput`/`BriefReview` + handler + `dex brief` CLI + the
  `toolSurface`/noop/remote/http plumbing + the "brief START HERE"
  MCP-instructions (now `ask(question)`) + tests. `collectLocalRules` moved to
  `local_rules.go` (ask depends on it); the brief-only review-inline
  (`isReviewIntent`/`briefReviewPack`) retired with it — review lives in
  `review_diff` and, later, ask's review intent (step 5). Dropped
  `antiAccretionCeiling` 1→0. Task-start now routes through `ask(assemble)`.

The correctness bar for any future fold is the same: the demoted tool's distinctive
output must exist on `ask` *before* the tool is removed — proven by dogfood, not
assumed.

### review output-union (deferred, own slice)

review is delta-shaped; every other ask intent is state-shaped (`ContextOutput`).
Routing `ask("review my changes")` into the review composition needs a
discriminated-union `review{}` field on the response (or a distinct shape) so a
delta-shaped result isn't crammed into state-shaped `ContextOutput`. This is the
crux the #83 `isReviewIntent` NL-half retirement waits on. Its own slice; until
then `review_diff` stays an everyday tool.

## Slice sequencing (each additive, shippable, alias-retaining)

1. **ask always-on.** Drop the `!embedAvailable || expert` gate — register `ask` in
   the everyday profile always (it already BM25-falls-back with no embedder).
   Purely additive: nothing demoted yet. Golden contract regen. This alone makes
   the flagship verb present and is the prerequisite for every later slice.
2. **Fold brief → ask(assemble), port-then-remove (#141).** 2a: port brief's
   `local_rules` into `ask(assemble)` (`ContextOutput.Rules`), dogfood parity. 2b:
   remove `brief` once parity holds. Resolves the anti-accretion offense → drop
   `antiAccretionCeiling` 1→0.
3. **Demote search → expert (done, #142).** Dogfooded `ask`/`ask(intent=behavior_search)`
   parity (same-or-superset ranked hits); moved `search` into `registerExpertTools`
   (embed+expert gated, like `clones`/`similar`). Everyday concept-search → `ask`;
   raw scoring breakdown stays a power lane. Discussed keeping `search` as a
   fundamental primitive alongside `grep`/`read` (search is dex's core capability);
   Aleh chose demotion — `ask` is strictly richer for the everyday path and `grep`
   remains the literal-search primitive. Instructions + golden updated; `dex search`
   CLI unaffected.
4. **Demote trace + locate → expert.** Confirm callers/callees/symbol_lookup/orient
   coverage + `look` shape-routing; keep `path`/`impact` reachable via `intent=`.
5. **review output-union.** Discriminated `review{}` shape; route `ask("review …")`;
   retire the #83 `isReviewIntent` NL half; demote `review_diff`.
6. **Checkpoint.** Re-run both gates (router #9, anti-accretion #10 at ceiling 0),
   dogfood the four-verb surface end-to-end, then decide on final alias cleanup.

## Edge cases

- **No embedder.** ask already registers today in that mode and BM25-falls-back;
  slice 1 only *widens* when it registers, never narrows. Demoted lanes that need
  the embedder (`search`) degrade exactly as ask does.
- **Weak model.** `registerEverydayTools` is skipped entirely for weak models
  (server.go:902); this spec does not change that branch.
- **DEX_EXPERT users.** See a superset (front door + all power lanes) — unchanged.
- **CLI parity.** `dex search`/`trace`/`locate` CLI subcommands are unaffected by
  MCP profile gating; demotion is an MCP-surface change only. (`dex brief` was
  removed outright in slice 2b — its job is `dex ask --intent assemble`.)

## Validation

- Per slice: `mooncake task ci-fast` green; tool-schema golden regenerated and
  reviewed; router-accuracy harness ≥ floor; anti-accretion lint green.
- Slices 2-4: a dogfood check that ask(intent) matches the demoted tool's output
  quality on ≥3 real queries before the tool leaves the everyday surface.
- Envelope-uniformity / one-fuzzy-verb tests stay green throughout.

## Rollback

Each slice is a profile-registration change + golden regen; revert the slice's
commit. No index/state migration. brief removal (slice 2) is the only
hard-to-reverse step — keep it as a demotion-to-expert first if fold quality is
unproven, and delete only once ask-assemble parity is demonstrated.
