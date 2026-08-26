# Spec: review-finding scoped notes (close the review→edit loop)

> **Note (#205, 2026-08-26):** dex's `record`/`notes` write verb and the whole knowledge subsystem (`knowledge_facts`/`knowledge_relations`/`scoped_notes`) were removed — the MCP surface is a single verb, `query`. Any mention of `record`/`notes`/`knowledge`/`remember` below is **historical**.


Issue: #87 — let a code review emit findings as scoped notes (gotcha-on-touch).

## Goal

A code review produces findings the *next* agent editing a file most needs
("this is a 1058-line god-step, decomposition is in flight"; "networks are
written into the XML by two paths, don't add a third"). dex already surfaces any
note whose `scope` binds a file when that file is touched (`read.scoped_notes`,
`locate` related notes, `review.scoped_notes` — #645/#649/#650). What is missing
is an ergonomic, *distinguishable* authoring path from "I just reviewed this and
found X" to "persist X as a scoped note so the next toucher sees it".

Close the loop inside dex — where the code lives — instead of leaking findings
into chat or an out-of-band memory store.

## Scope

- Add a first-class knowledge archetype `ReviewFinding`.
- A review finding is authored via the existing `notes` verb:
  `notes(action=add, archetype=ReviewFinding, scope=<file|glob|package>, body=…)`.
- Nudge the loop where reviews happen: the `review` verb hint prompts persisting
  confirmed findings as `ReviewFinding` notes.

Non-goals (explicitly out of scope, boring wins):

- **No new MCP tool.** A `review_finding()` wrapper would add a whole tool
  surface for what is one `notes(action=add, …)` call. The archetype *is* the
  convention.
- **No new store column / migration.** Archetype is already an arbitrary string
  column with app-level validation; `ReviewFinding` rides the existing rails.
- **No line-range scoping.** `scope` matches at file / glob / package grain
  (`KnowledgeByScope`: glob, dir-prefix, exact path). A `path:Lstart-Lend` scope
  would silently never match on touch — a trap. Line ranges belong in the body,
  not the scope. This is the one place the issue's proposed API (`scope=path:range`)
  is deliberately narrowed.

## Interfaces

Author (existing verb, new archetype value):

```
notes(action=add,
      archetype=ReviewFinding,
      scope=internal/mcp/server.go,     # file | glob | package prefix
      body="[god-object] registerTools is 442 lines; decomposition in flight (#NN)")
```

Recall / filter (existing verbs, unchanged):

- `read(path)` / `locate(ref)` / `review(staged)` — surface it in `scoped_notes`
  when the bound file is touched. **No code change on the surfacing side.**
- `notes(action=list, archetype=ReviewFinding)` — all open findings by salience.
- `notes(action=list, scope=<path>, archetype=…)` — findings that bind a path.

### Severity / kind taxonomy (optional per issue → body convention)

Lead the body with a bracketed kind tag so findings are greppable and skimmable
without a new schema field:

`[god-object]` · `[duplication]` · `[layering-violation]` · `[injection-risk]`

This is a convention, not a validated enum — reviewers may use others.

### Salience & lifecycle

`ReviewFinding` is actionable but *point-in-time* — it describes an assessment of
code as it stood, and should age out as the code churns past it.

- `archetypeWeight("ReviewFinding") = 1.3` — actionable (above Decision 1.2,
  below Gotcha 1.4). Not so high it auto-injects into unrelated `ask` responses;
  it earns its keep by scope-surfacing on touch.
- `archetypeDecayRate("ReviewFinding") = 0.012` — ages faster than Gotcha
  (0.008): a stale finding evicts once nobody reaffirms it. A reviewer who wants
  a finding to persist can `pin` it or re-add with `evidence=true`.

## Edge cases

- Unknown-cased input (`reviewfinding`, `review-finding`, `review_finding`)
  canonicalises to `ReviewFinding` (CLI + MCP), same as every other archetype.
- Omitting `scope` on a `ReviewFinding` is legal (it becomes an unscoped,
  recall-only note) but defeats gotcha-on-touch; `scope_suggestion` (#658) still
  fires when the body names a real file.
- Decay/weight default fallback (1.0 / 0.010) must NOT apply — `ReviewFinding`
  must be present in both switch statements or salience silently regresses (#520).

## Validation

- Store: `archetypeWeight`/`archetypeDecayRate` return the tuned values for
  `ReviewFinding` (unit test).
- CLI: `canonicalArchetype` maps the three casings → `ReviewFinding`; `dex notes
  add --archetype ReviewFinding` succeeds; an invalid archetype still errors.
- MCP: `validArchetype("ReviewFinding")` is true; `notes(action=add,
  archetype=ReviewFinding, scope=X)` persists and a subsequent `read(X)` returns
  it in `scoped_notes` (integration test — the closed loop).
- `mooncake task ci-fast` green (build + test + vet + fmt-check).

## Files

- `internal/store/store_knowledge.go` — `ReviewFinding` in `archetypeWeight`,
  `archetypeDecayRate`, and the `Archetype` doc list.
- `internal/mcp/server_knowledge.go` — `validArchetype`, add-error message,
  `Archetype` jsonschema description.
- `cmd/dex/main_flags.go` — `canonicalArchetype` casings.
- `cmd/dex/knowledge.go` — flag help + error message + an example.
- `internal/mcp/server.go` — notes tool-description archetype list + loop note.
- `internal/mcp/server_review.go` — review hint nudge.
- `specs/mcp-server.md`, `specs/review.md` — doc the archetype + loop.
- Tests alongside each.
