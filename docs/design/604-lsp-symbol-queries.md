# Design — #604 Precise symbol queries (references / implementations / type hierarchy)

Status: **design / proposal** · Author: agent#647 · Supersedes the framing in
issue #604 (mcsearch#35).

## TL;DR

The issue proposes bolting an LSP client (gopls et al.) onto dex to fix the
tree-sitter graph's recall gaps. Two facts change the shape of the right answer:

1. **For Go, dex already has type-precise analysis on demand** — `internal/refactor`
   (#638) and `internal/cohesion` (#643) load `go/packages` + `go/types` and
   answer rename / implementor questions exactly, with no index and no external
   server. References / implementations / type-hierarchy for Go are the same
   `go/types` queries. **gopls adds nothing here except a process to manage.**
2. **The "reuse the IDE's already-running language server" premise is mostly
   false.** Editors spawn gopls / rust-analyzer over **stdio pipes** they own,
   not on a discoverable Unix socket. There is no standard way to find and attach
   to that process. To use LSP, dex must **spawn and manage its own server** —
   the long-lived process the issue's "one socket connection, not a new process"
   pitch was trying to avoid.

So the precise-query capability splits cleanly by language:

| Language | Engine | Cost | Status |
|---|---|---|---|
| **Go** | `go/types` on-demand (the refactor/cohesion path) | cheap, dep-free, already in-tree | **build now (Tier 1)** |
| **Rust / TS / Python / Java** | a dex-spawned, pooled LSP client | a managed subprocess per project+lang | **opt-in, evidence-gated (Tier 2)** |

This design recommends **shipping Tier 1 now** and **gating Tier 2 behind a
measured recall win**, mirroring the #499 SCIP verdict (which parked an
equivalent precision lane because graph precision was already 1.0 and the gap was
recall, with a cheaper fix available).

---

## Background — what's actually broken

The current graph (`internal/graph`, schema `store_migrate.go:115/154`) is:

- **Go**: `EdgeCalls` are **type-resolved** via `go/types` (`go.go`); `EdgeImplements`
  links concrete **type → interface** (`go.go:855`, `:929`); plus `EdgeHasMethod` /
  `EdgeEmbeds`. Precision is high.
- **Other langs**: tree-sitter, **name-based** call edges (`sitter.go`), stamped
  `metadata.provenance="sitter"`. Incomplete recall by construction — no type
  info, so dynamic dispatch / cross-module aliases leave no edge.

`trace` / `impact` (`graphquery/traverse.go`) walk `EdgeCalls` only. The concrete
gaps the issue names, re-checked against the code:

1. **Interface-dispatch callers are invisible — even for Go.** A call through an
   interface method resolves (in `EdgeCalls`) to the *interface* method, not the
   concrete implementations. `EdgeImplements` is **type-level**, so there's no
   single edge walk from "interface method M" to "callers of every concrete M".
   This is a **recall** gap, and it is closeable with `go/types` method-sets
   (exactly what `cohesion.ImplementorsOf` already computes) — **no LSP needed.**
2. **"What implements this interface?"** — already answered for Go by `cohort`
   (#643). Missing only for non-Go.
3. **Type hierarchy (super/sub types)** — derivable from `go/types` for Go;
   missing for non-Go.
4. **Non-Go precision generally** — only LSP (or per-language type inference)
   closes this.

Key reframing: **the precision problem is mostly already solved for Go and mostly
unsolved for everything else.** The design must not spend Go complexity on gopls.

---

## Goals / non-goals

**Goals**

- A precise `references` / `implementations` / `type_hierarchy` capability,
  language-routed, that degrades cleanly when precision isn't available.
- Close the interface-dispatch **recall** gap for Go using `go/types` (Tier 1).
- A clean seam for an LSP engine (Tier 2) that does **not** leak LSP types into
  the rest of dex and is opt-in + lifecycle-safe.

**Non-goals**

- Write operations (rename/move/inline) — `refactor` owns planning; dex stays
  read-only (#551).
- Spawning a server dex can't cleanly own/kill. No orphaned gopls processes.
- Replacing the tree-sitter graph. LSP/`go/types` *enrich* it on demand; the
  persisted graph stays the fast default.

---

## Surface

One capability, exposed two ways (consistent with how `cohort`/`refactor` landed):

### A. Enrich existing verbs (transparent, Tier 1)

- **`trace --dir callers`**: when the resolved symbol is an interface method (Go),
  expand to callers of every concrete implementation via `go/types` method-sets.
  Adds a `via: "interface"` tag on those call sites. Off when the symbol isn't an
  interface method; unchanged for non-Go.
- This is the highest-value, lowest-surface change — it fixes the headline
  "trace misses callers through interfaces" without a new tool.

### B. A new power-lane verb `refs` (DEX_EXPERT)

Mirrors the `smells`/`routes`/`cohort` analysis family. One verb, an `action`:

```
refs(action, symbol|ref, project_root)
  action: references | implementations | supertypes | subtypes
```

Output: a compact, path-relativised list of `{path, line, kind, role}`. Routed by
language:
- Go → `go/types` engine (Tier 1).
- other → LSP engine if `DEX_LSP=1` and a server is configured (Tier 2), else
  `status: "lsp-unavailable"` with the tree-sitter best-effort list + a hint.

> Naming: `refs` over the issue's `lsp` — the verb is about a *capability*
> (references/impls/hierarchy), not the transport. Most calls won't touch LSP at
> all (Go uses go/types). Leaking "lsp" into the verb name would misname the Go
> path and bind the contract to a backend.

`implementations` is a superset of `cohort`'s data; `cohort` stays the
interface-cohesion *planning* verb, `refs implementations` is the raw query.
(If that overlap proves redundant, fold one into the other in v2 — flag, don't
pre-optimise.)

---

## Tier 1 — Go via `go/types` (build now)

Pure reuse of the refactor/cohesion precedent. New package `internal/symbols`
(or extend `internal/cohesion`) with the loader config already proven in
`go.go:159` / `refactor` / `cohesion`:

- **references(obj)**: walk every loaded package's `TypesInfo.Uses`/`Defs` for
  idents whose object == target (this is exactly `refactor.collectEdits`, minus
  the rewrite). Return def + use sites.
- **implementations(iface)**: `cohesion.ImplementorsOf` (already shipped).
- **super/subtypes(named)**: walk embedded interfaces (supertypes) and
  `types.Implements` across named types (subtypes).
- **interface-dispatch callers** (the `trace` enrichment): resolve the interface
  method → its `*types.Func`; find concrete implementors via method-set; union
  their callers from the persisted `EdgeCalls` graph (or a fresh references walk).

Properties: no index, no external process, no new deps (x/tools already vendored),
Go-only, ~<1s on a typical module (same as refactor/cohort). Degrades to
`unsupported-language` off-Go.

**Cost**: one package + the `trace` enrichment hook + the `refs` verb plumbing
(toolSurface + 4 backends + REST + registry + parity + docs — the now-standard
8-site dance; `cohort toolSurface` will list the sites).

---

## Tier 2 — LSP for non-Go (opt-in, evidence-gated)

### The hard truth about discovery

LSP servers are launched by the editor with **stdio** pipes; they do not listen
on a well-known socket and do not advertise themselves. "Attach to the IDE's
gopls" is not generally possible. Therefore Tier 2 means dex **spawns** the
server itself:

```
internal/lsp/
  client.go     minimal JSON-RPC 2.0 over stdio: initialize, initialized,
                didOpen, textDocument/{references,definition,implementation},
                typeHierarchy/{prepare,supertypes,subtypes}, shutdown/exit
  server.go     spawn + lifecycle: one server per (projectRoot, language),
                ref-counted, idle-timeout killed, killed on dex shutdown
  registry.go   language → server command + args, from config (no bundling):
                rust → rust-analyzer, python → pyright-langserver/pylsp,
                typescript → typescript-language-server, java → jdtls
```

### Lifecycle (the part that bites)

- **Spawn on first use** per (root, lang); cache the connection; **ref-count**
  and **idle-timeout** (e.g. 5 min) → kill. Kill all on dex process exit
  (defer + signal handler) so no orphaned servers.
- **initialize handshake** is async and slow (rust-analyzer indexes the crate on
  startup — seconds to tens of seconds). First `refs` call after spawn pays this;
  cache amortises. Surface `status: "lsp-warming"` rather than blocking long.
- **didOpen** the target file before querying (servers answer positionally).
- Treat every LSP call as best-effort with a hard timeout; on any error →
  degrade to the tree-sitter list + `lsp_unavailable: true`.

### Config (extends `.dex/config.yml`, `config_file.go:43`)

```yaml
lsp:
  enabled: false                 # DEX_LSP (default off — opt-in)
  idle_timeout: 5m               # DEX_LSP_IDLE
  servers:                       # operator-provided commands; nothing bundled
    rust:       rust-analyzer
    python:     pyright-langserver --stdio
    typescript: typescript-language-server --stdio
```

Nothing auto-downloaded; the command must be on PATH (same posture as the ONNX
runtime in CLAUDE.md). Off by default.

### Why Tier 2 is gated, not assumed

This is the same shape as **#499 (SCIP/LSIF lane), which was parked** after an
evidence gate: graph **precision was already 1.0** (zero false edges); the real
gap was **recall**, and the cheaper fix was intra-procedural type inference in
tree-sitter, not a heavy external precision pipeline. #604's LSP lane is the same
bet for non-Go. Before building it we should **measure** the recall it would add:

**Gate (must pass before Tier 2 code lands):**
- Stand up a tree-sitter-vs-LSP recall probe on the existing polyglot corpus
  (`dex bench` family — `internal/eval/corpus`, `internal/eval/trace`, the
  `skew` instrument from #553). For a sample of non-Go symbols, compare
  tree-sitter `callers`/`implementations` against LSP ground truth.
- **Ship Tier 2 only if** LSP recall materially beats tree-sitter on a real cell
  (e.g. ≥X% more true callers on the TS/Rust corpus) **and** that gap isn't
  cheaper to close with per-language type inference (the #499 alternative).
- Otherwise **park Tier 2 behind the tracked number**, exactly like #499/#555.

---

## Degradation contract

Every precise query returns a structured status, never a hard failure:

- `ok` — precise result (go/types or LSP).
- `unsupported-language` — no engine for this language (and `enrich` is a no-op).
- `lsp-unavailable` / `lsp-warming` — Tier 2 not reachable/ready → tree-sitter
  best-effort list + hint.
- `not-found`, `error` — as elsewhere.

`trace`/`impact` never regress: enrichment only **adds** edges; with no engine
they return today's results unchanged.

---

## Phasing

1. **Phase 1 (Tier 1, ship):** `internal/symbols` go/types queries +
   `trace` interface-dispatch enrichment + `refs` verb (Go lanes) + parity +
   docs. Closes the headline Go gaps with zero new deps/processes. Self-contained,
   testable against temp modules (refactor/cohort pattern).
2. **Phase 2 (measure):** the recall gate above. One `dex bench` instrument; no
   product code. Produces the go/no-go number for Tier 2.
3. **Phase 3 (Tier 2, conditional):** `internal/lsp` client + spawn/lifecycle +
   config + `refs` non-Go routing — **only if Phase 2 clears the gate.**

## Risks

- **Orphaned LSP processes** (Tier 2) — mitigated by ref-count + idle-kill +
  shutdown handler; covered by a test that asserts no child survives `Server`
  close.
- **rust-analyzer warm-up latency** — surfaced as `lsp-warming`, never blocks.
- **Surface creep** — `refs` is one expert-lane verb; the interface-dispatch win
  ships as a `trace` enrichment, not a new tool. Respects #573.
- **`cohort` / `refs implementations` overlap** — accepted for v1; reconcile once
  both have usage.
- **Scope discipline** — Tier 1 is the commitment; Tier 2 is explicitly
  conditional on measured value, not built on spec.

## Recommendation

Build **Tier 1 (Go go/types: `refs` + interface-dispatch `trace` enrichment)**
now — it's cheap, dep-free, and erases the accuracy asterisk on Go `trace`/`impact`
that motivated the issue. **Defer Tier 2 (LSP) behind the Phase-2 recall gate**;
the "reuse the running server" premise doesn't hold, so LSP costs a managed
subprocess and must earn it with a measured non-Go recall win — and may lose to
the cheaper #499 alternative (tree-sitter type inference).
