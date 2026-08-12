# Spec: the quality gate speaks agent (#155)

Status: proposed
Issue: #155
Relates: #153 (structural ratchet — the enforcement this makes legible),
go-quality#1/#2 (the shared gate this extends), [[tool-surface-consolidation]]

## Goal

Make the quality gate emit **structured, machine-readable findings** so an agent
that trips it can act on the exact `path:line:rule:message` without re-running
tools and scraping prose — and so those findings can flow both ways: into GitHub
code scanning (SARIF) and into dex's own evidence surface, where dex's `smells` /
`clones` / `similar` analysis can emit the *same* schema and plug back into the
gate. The gate feeds the agent; the agent's tooling feeds the gate.

Non-goal: replacing the human output. Prose stays the default and unchanged.
Structured emission is strictly additive, behind a flag.

## Why now

#153 made the gate *enforce* structure (god-file/gocyclo/dupl/deadcode ratchet),
and go-quality#1 upstreamed it. But every step still only prints text. An agent
that fails the gate must re-run `budget-status`/`dupl`/`structure-ratchet` and
parse formatted output to recover *what* grew and *where*. The scripts already
compute `path:line: rule: message` internally (`ai-lint.sh` literally documents
that line format) — we throw the structure away at the print boundary. This spec
keeps it.

## The one boring artifact — a JSONL finding stream

One finding per line, newline-delimited JSON (JSONL). No new dependency, greppable,
appendable, diffable across runs:

```json
{"tool":"structure-ratchet","rule":"god_files","level":"error","path":"internal/mcp/server.go","line":1,"message":"god_files grew 34 -> 35 (+1): internal/mcp/server.go","fingerprint":"god_files:internal/mcp/server.go"}
```

Schema (fields; unknown consumers ignore extras):

| field | req | meaning |
|---|---|---|
| `tool` | yes | emitting step (`ai-lint`, `structure-ratchet`, `dupl`, `budget`, `gocyclo`, `deadcode`, `smells`, `clones`) |
| `rule` | yes | stable rule id within the tool |
| `level` | yes | `error` \| `warning` \| `note` (error = gate-failing) |
| `path` | yes | repo-relative file |
| `line` | yes | 1-based; `1` when the finding is file- not line-scoped |
| `col` | no | 1-based column when known |
| `message` | yes | one-line human description |
| `fingerprint` | no | stable id for dedup/suppression across runs (defaults to `rule:path:line`) |

`level` is the single source of gate pass/fail: any `error` fails. This lets a
ratchet-improved run emit `note`s (offenders that shrank) without failing.

## Phases (one PR each; each independently useful)

### P0 — schema + JSONL contract, piloted on ai-lint (go-quality)
`ai-lint.sh --format jsonl` emits the schema above instead of its
`path:line: rule: message` text. Pure adapter over the existing scan — proves the
schema against the tool already closest to structured. Human output unchanged
when the flag is absent.

### P1 — retrofit the gate steps (go-quality)
`budget-status`, `dupl`, `structure-ratchet`, `gocyclo`, `deadcode` each grow
`--format jsonl` behind the flag. `ci/full.sh` gains `GATE_FORMAT=jsonl` which
aggregates every step's findings into `.gate/findings.jsonl` (gitignored) while
still printing the human summary. Findings are the boundary; the human text is a
view. Gate pass/fail is unchanged (derived from `level:error` count == the
existing exit semantics).

### P2 — SARIF 2.1.0 adapter (go-quality)
Thin `scripts/findings-to-sarif.sh` (jsonl → SARIF 2.1.0). No new logic — a pure
projection so findings upload to GitHub code scanning and render in IDEs. Runs on
demand / in CI, never in the local fast path.

### P3 — the dex flywheel (dex)
Two directions, both reusing the schema:
- **Ingest:** `ask("review my changes")` reads `.gate/findings.jsonl` when present
  and folds gate findings into the review evidence pack — the agent sees the gate
  verdict as first-class evidence next to the diff, not as scraped terminal text.
- **Emit:** dex's `smells` / `clones` / `similar` lanes gain the same finding
  schema, so dex's semantic analysis is gate-pluggable — a project can add
  `dex smells --format jsonl` as a gate step and it aggregates like any other.
  This is where dex eats its own dogfood: the richest findings (vector clones,
  cohesion smells) come from dex itself.

## Design constraints

- **Additive, flag-gated.** No flag → byte-identical human output. Nothing in the
  default local loop changes; JSONL/SARIF are opt-in.
- **Boring format.** JSONL, not a bespoke binary or a heavy framework. SARIF only
  as a leaf projection, never the internal representation.
- **One schema, both repos.** go-quality steps and dex verbs emit the identical
  shape; the schema lives documented in this spec and (P1) in go-quality's README.
- **Level drives the gate.** Exit semantics are re-expressed as "any `error`
  finding fails", matching today's behavior exactly — no policy change smuggled in.

## Edge cases

- **Improved metrics.** A ratchet run where a count shrank emits `note`-level
  findings (visible, non-failing) so the agent can see what to `--refresh`.
- **Tool missing.** A skipped step emits no findings (not a synthetic error);
  `--ci` still fails on a missing tool via existing exit code, independent of the
  stream.
- **No baseline (opt-in ratchet).** Skips → zero findings, consistent with P0 exit 0.
- **Large `.gate/findings.jsonl`.** Gitignored, truncated per run (not appended
  across runs); dedup is the consumer's job via `fingerprint`.

## Interfaces

- go-quality: `--format jsonl` on the five step scripts + `ai-lint.sh`;
  `GATE_FORMAT` env honored by `ci/full.sh`; `scripts/findings-to-sarif.sh`.
- dex: `--format jsonl` on `smells`/`clones`/`similar`; `ask` review lane reads
  `.gate/findings.jsonl`. No wire-schema change to the four everyday verbs.

## Validation

- P0: `ai-lint.sh --format jsonl` output parses as JSONL; every line validates
  against the schema; text mode byte-identical to today.
- P1: `GATE_FORMAT=jsonl mooncake task ci` produces `.gate/findings.jsonl` whose
  `error` count matches the gate's pass/fail; default run unchanged.
- P2: emitted SARIF validates against the 2.1.0 schema and uploads to GitHub code
  scanning on a scratch repo.
- P3: `ask("review my changes")` on a tree with a seeded gate finding surfaces it
  in the evidence pack; `dex smells --format jsonl` round-trips through the P1
  aggregator.

## Rollback

Per phase: each is a flag or a new script. Remove the flag / delete the script;
default behavior is unaffected because it never depended on the new path.
