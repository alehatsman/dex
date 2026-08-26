# Spec: verify — run the project's canonical test command, not a guessed `go test`

> **Note (#205, 2026-08-26):** dex's `record`/`notes` write verb and the whole knowledge subsystem (`knowledge_facts`/`knowledge_relations`/`scoped_notes`) were removed — the MCP surface is a single verb, `query`. Any mention of `record`/`notes`/`knowledge`/`remember` below is **historical**.


Status: proposed
Issue: #146
Epic tail: #110 (verify's re-promotion path — a runner the agent can trust earns
an everyday slot again; demoted to expert in slice 5c).

## Goal

`verify` (verify_change) resolves a change → the Go packages it implicates → a test
command, and runs it. Today that command is a hardcoded guess (`go test
{{packages}}`, or `$DEX_VERIFY_CMD`). In a repo whose tests need build tags — dex
itself: tests panic without `-tags sqlite_fts5`, canonical command is `mooncake
task test` — the guess is WRONG, so the agent bypasses verify and runs the project
task by hand. A verify the agent can't trust earns no everyday slot.

Make verify prefer the repo's **declared** test command, discovered by the same
`ExtractProjectCommands` path `orient` already uses (tasks.yml → Makefile →
package.json → language default). Fall back to the go-test heuristic only when no
runner is detected.

Non-goal: widening change→scope resolution beyond Go. verify still recognizes Go
files → packages (the "Go-only in v1" limitation stands for scope resolution); a
JS-only change still returns `no-tests`. Running a non-Go canonical command for a
non-Go change is future work (see Follow-ups).

## Current behavior (path)

`server_verify.go:84`: `cmd := testscope.SynthVerifyCommand(in.Command, pkgs)` →
`testscope.SynthVerifyCommand` (testscope.go:64) resolves the template:
`in.Command` override → `$DEX_VERIFY_CMD` → `"go test {{packages}}"`, substituting
`{{packages}}` with the space-joined package list (or appending it when the
placeholder is absent). No knowledge of the project's own task runner.

`ExtractProjectCommands(root)` (orient_commands.go:27) already returns the canonical
`test`-role command for the repo, verbatim (`mooncake task test`, `make test`, `npm
run test`, or the language fallback `go test ./...` / `cargo test`).

## Design

### Command precedence (new)

1. `in.Command` explicit override — user-authored template (`{{packages}}` semantics).
2. `$DEX_VERIFY_CMD` — user-authored env template (`{{packages}}` semantics).
3. **Detected canonical `test` command** (`ExtractProjectCommands` "test" role) — NEW.
4. `go test {{packages}}` — final fallback when no runner is detected.

Override and env stay ahead of detection: an explicit user choice beats
auto-discovery ("explicit > magic").

### Scoped vs verbatim (the one subtlety)

The canonical command is used as follows:

- A **whole-module `go test … ./...`** (or trailing `.`) form — the language
  fallback for a plain-Go repo with no task runner — is **re-scoped**: the trailing
  `./...`/`.` is replaced with the implicated package list, preserving every flag
  in between. So `go test -tags x ./...` + `[./internal/mcp]` → `go test -tags x
  ./internal/mcp`. This keeps today's fast, targeted Go behavior AND any tags the
  language fallback carried; no regression for the common plain-Go case.
- **Any other canonical command** (a task runner: `mooncake task test`, `make test`,
  `npm run test`) runs **verbatim** — the whole suite. Appending package paths to
  `mooncake task test` would be nonsense; the task runner owns its own scope/flags.

Override/env templates keep their existing `{{packages}}`-or-append semantics
unchanged (backward-compat, already tested).

### Interfaces

`testscope.SynthVerifyCommand` gains the detected command:

```go
// SynthVerifyCommand resolves the test command: override → $DEX_VERIFY_CMD →
// canonical (the repo's declared test command) → the go-test default. A
// whole-module `go test … ./...` canonical is re-scoped to pkgs; any other
// canonical runs verbatim.
func SynthVerifyCommand(override, canonical string, pkgs []string) string
```

New unexported helpers in testscope: `applyTemplate(tmpl, joined)` (the
substitute-or-append rule for user templates) and `scopeGoTest(cmd, joined)
(string, bool)` (re-scope a whole-module go-test form; ok=false otherwise).

mcp computes `canonical` — testscope stays pure (it cannot import mcp's
`ExtractProjectCommands`):

```go
// canonicalTestCommand returns the repo's declared test command (same source
// orient surfaces), or "" when no runner is detected.
func canonicalTestCommand(root string) string // scans ExtractProjectCommands for the "test" role
```

`server_verify.go`: `cmd := testscope.SynthVerifyCommand(in.Command,
canonicalTestCommand(p.Root), pkgs)`. When the resolved command runs verbatim
(whole suite), the `ok` output carries a Hint noting the canonical command ran the
full suite and `Packages` is the change's blast radius, not the executed scope — so
the field isn't misread.

`VerifyInput.Command` jsonschema updated to document the new precedence.

## Edge cases

- **dex itself** (acceptance): tasks.yml present → canonical `mooncake task test` →
  verbatim. No `$DEX_VERIFY_CMD` needed; the FTS5 tags come from mooncake.
- **Plain-Go repo, no runner**: `ExtractProjectCommands` language fallback `go test
  ./...` → re-scoped to implicated packages → identical to today's default.
- **`$DEX_VERIFY_CMD` set**: still wins over detection (explicit choice preserved).
- **Non-Go change** (only .md/.ts changed): `GoPackagesForFiles` empty → `no-tests`
  as today. Canonical detection does not change the Go-scope gate.
- **No changes / symbol not found / no graph**: unchanged terminal statuses.
- **Whole-module canonical with no implicated pkgs**: shouldn't occur (empty pkgs is
  gated to `no-tests` before command synthesis), but `scopeGoTest` leaves the
  command whole-module rather than emitting a dangling `go test` with no target.

## Validation

- `testscope` unit tests: existing 4 cases (signature migrated with `canonical=""`)
  + new — canonical task runner runs verbatim; whole-module go-test canonical
  re-scoped with tags preserved; override/env still beat canonical.
- mcp: a `canonicalTestCommand` test over a temp repo with a `tasks.yml` test target
  (→ `mooncake task test`) and a bare go.mod repo (→ `go test ./...`).
- Live dogfood in dex's own tree: `verify_change` (worktree mode) shows
  `command: "mooncake task test"`, not `go test …`. Acceptance criterion.
- `mooncake task ci-fast` green; golden/router/anti-accretion unaffected (no
  tool-surface change — verify stays expert-gated).

## Rollback

Pure logic change in `testscope` + one call site + one helper; no index/state/
tool-surface migration. Revert the commit.

## Follow-ups (out of scope, note don't build)

- Non-Go change→scope resolution so verify runs `npm run test` / `cargo test` for a
  JS/Rust change (today `no-tests`). File as a separate issue if wanted.
- Re-promotion of `verify_change` toward the everyday surface, once trust is
  established — the #110 tail decision, not this issue.
