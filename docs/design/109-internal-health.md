# 109 — extract backend-readiness diagnosis into internal/health

Refactor, behavior-neutral. Spun out of #107 (#95g groom). Tracks the
big-bang move the incremental groom couldn't make.

## Goal

`cmd/dex/doctor_deep.go` (and its neighbours in `doctor.go`) hold real domain
logic — mapping a backend capability-probe outcome (usable / model-not-ready /
unreachable / cold-timeout) into a classified, remediation-hinted result —
welded to cmd-local presentation types. Extract the *diagnosis* subsystem into a
new `internal/health` package so `doctor` / `status` / `setup` / MCP can share
one answer to "is this backend able to serve, and if not what do I tell the
operator". `cmd/dex` keeps flag parsing + terminal rendering.

## Scope

**Moves to `internal/health`:**

- The check/status model — `Status` (`OK`/`Warn`/`Fail`/`Skip`, was
  `doctorStatus` + `docOK…`) and `Check` (was `doctorCheck`), with **exported
  fields** (`Name`, `Status`, `Detail`, `Hints`, `Critical`) so the command
  layer can build them.
- The probe descriptor — `Probe` (was `endpointProbe`), exported fields
  (`Name`, `URL`, `Model`, `Health`, `Deep`, `Status`).
- The classification + orchestration:
  - `CheckEndpoints(ctx, []Probe) []Check` — plain liveness fan-out (was
    `checkEndpoints`), plus private `classifyEndpoint` (was `endpointCheck`)
    and `endpointHints`.
  - `CheckEndpointsDeep(ctx, []Probe) []Check` (was `checkEndpointsDeep`),
    `ClassifyDeep` (was `classifyDeep`, **exported** — cmd integration tests
    call it), private `deepNotReadyHints`, `deepTimeouts`.
  - `DeepProbeText` const (was `deepProbeText`, **exported** — the command's
    probe closures use it as the request payload).

**Stays in `cmd/dex`:**

- `collectEndpoints() []health.Probe` — env wiring + client construction
  (`newEmbedClient`/`newChatClient`/`newRerankClient`). This is the injection
  seam: the command builds the probe list (with `Health`/`Deep` closures) and
  hands it to `health`. Lives in `render.go`.
- Rendering — `docSym(health.Status)`, the doctor/setup print loops,
  `printEndpoints` (the `status` table).
- The non-backend checks that construct `health.Check`: `checkIndexDir`,
  `checkProjectConfig`, `checkProxy`, `checkRulesWiring`, `checkMCPWiring`.

`doctor_deep.go` is **deleted** — all its contents move; the `--deep` flag
wiring already lives in `cmdDoctor` (doctor.go), and the rendering is the shared
print loop, so no thin wrapper remains.

## Key decision: inject probes, don't move the wiring

`collectEndpoints` reads env and constructs backend clients — that is command-
layer wiring, not diagnosis. So the orchestration functions take `[]Probe`
rather than calling `collectEndpoints` themselves. `health` depends only on the
backend sentinel/error packages (`backendhttp`, `chat`, `embed`, `rerank`), not
on cmd-local env plumbing.

## Interfaces touched

| Symbol (was) | Becomes | Where |
|---|---|---|
| `doctorStatus`, `docOK/Warn/Fail/Skip` | `health.Status`, `health.OK/Warn/Fail/Skip` | health |
| `doctorCheck` | `health.Check` (exported fields) | health |
| `endpointProbe` | `health.Probe` (exported fields) | health |
| `checkEndpoints(ctx)` | `health.CheckEndpoints(ctx, probes)` | health |
| `endpointCheck`, `endpointHints` | `health.classifyEndpoint`, `health.endpointHints` (private) | health |
| `checkEndpointsDeep(ctx)` | `health.CheckEndpointsDeep(ctx, probes)` | health |
| `classifyDeep` | `health.ClassifyDeep` (exported) | health |
| `deepNotReadyHints`, `deepTimeouts` | private in health | health |
| `deepProbeText` | `health.DeepProbeText` (exported const) | health |
| `collectEndpoints`, `printEndpoints`, `docSym` | unchanged names, retyped to `health.*` | cmd/dex |

## Import direction

`cmd/dex → internal/health → {backendhttp, chat, embed, rerank}`. No cycle:
the backend packages don't import `health`, and nothing imports `cmd/dex`.

## Edge cases (behavior must not change)

- **Lean profile** (`DEX_EMBED_ENGINE=none`): `newEmbedClient` returns nil,
  `collectEndpoints` emits an embed `Probe{Status:"not configured"}` with nil
  `Health`/`Deep`; `classifyEndpoint` reports `Skip`. Guarded by
  `TestCollectEndpointsLean` (stays in cmd).
- **embed critical, chat/rerank degrade**: embed failure → `Fail`+critical;
  chat/rerank → `Warn`. Both plain and deep. Guarded by `TestClassifyDeep`.
- **Cold-load timeout**: `context.DeadlineExceeded` → `Warn` non-critical.
- **Overloaded (429/5xx)**: reachable-but-busy → `Warn`, not `UNREACHABLE`.
- **Model not served (4xx)**: `Fail`+critical with a model-naming hint.

## Test split

- `TestClassifyDeep` (pure: builds a `Probe`, calls the classifier) → moves to
  `internal/health/health_test.go`.
- `TestDoctorDeepEmbedHitsInference` / `…ModelMissing` (httptest servers via
  `collectEndpoints` env wiring) → stay in `cmd/dex`, retargeted to
  `health.ClassifyDeep`.
- `TestCollectEndpointsLean`, `TestCheckProxy*` → stay (retyped fields).

## Validation

- `mooncake task ci` green.
- `dex doctor` and `dex doctor --deep` output unchanged (manual diff vs the
  installed build on this repo).
