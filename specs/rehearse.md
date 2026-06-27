---
id: rehearse
status: draft
owners: [aleh]
covers:
  - "internal/rehearse/**"
  - "cmd/dex/rehearse.go"
  - "internal/mcp/server_rehearse.go"
---
# Rehearse

## Intent

Type-check a hypothetical edit in-memory and return new type errors, broken files,
and tests to run — without writing anything to the working tree. Closes the chain:

  `refactor` (plan) → `rehearse` (prove it compiles) → `Edit` (apply) → `verify` (test)

dex is read-only by design (#551): `rehearse` never writes files. Edits are
applied via `go/packages.Config.Overlay` (an in-memory `map[string][]byte`),
the module is re-type-checked, and only *new* errors (not pre-existing ones) are
returned. v1 is Go-only (on-demand `packages.Load`, no index needed).

## Behavior

- WHEN `op` is not supplied or when no `go.mod` is present, returns
  `status=unsupported-language` with a descriptive hint.
- WHEN neither `edits` nor `files` are supplied, returns `status=no-edits`.
- WHEN `files` and `edits` address the same path, `files` wins (whole-file
  takes precedence).
- `edits` are applied highest-offset-first per file so prior offsets stay valid
  (same convention as `refactor` edit triples).
- A *baseline* type-check of the real working tree is run first; new diagnostics
  are the set difference `hypo − baseline` so pre-existing errors don't pollute
  the result.
- `compiles: true` when the new-error set is empty (the hypothetical introduces
  no additional type errors over the baseline).
- `broken_files` lists the unique paths that have new errors, sorted.
- `tests_to_run` lists the sibling `_test.go` files for each broken file.
- `overlay_etag` is the first 12 hex chars of a SHA-256 over the overlay
  contents+paths; useful for caching and stale-plan detection.

**Read-only invariant**: the working tree is never written. An invariant test
(`TestRehearse_DiskUntouched`) confirms the file on disk is byte-identical after
a destructive rehearsal.

## Checklist

- [x] `rehearse` registered as a default-lane MCP tool + CLI verb (parity test green)
- [x] Overlay-based type-check returns NEW diagnostics only (baseline-diffed)
- [x] `broken_files` and `tests_to_run` populated
- [x] Disk-untouched invariant test
- [x] `status=unsupported-language` for non-Go roots
- [x] `status=no-edits` when no edits/files supplied
- [x] spec under specs/ + docs/tools.md row
