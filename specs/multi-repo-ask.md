---
status: proposed
issue: 552
supersedes: none
---

# Multi-repo / cross-project ask (design)

> **Status: proposed.** This is a scoping document (#552), not a living spec.
> No code ships from it yet. It records the transport decision, the fan-out and
> fusion design, and the open questions an implementation must resolve.

## Intent

Every dex tool is scoped to **one** project, resolved from the caller's working
directory (see [mcp-server.md](mcp-server.md)). A question that spans repos —
"where is this shared proto consumed?", "which services call this library's
retry path?", "is this helper duplicated across our repos?" — cannot be answered
in one call. The agent has to ask each project in turn and stitch the answers by
hand, which is exactly the cross-cutting work Sourcegraph's CodeScaleBench shows
MCP nav tools should shoulder, and where single-scoping is dex's sharpest limit.

The goal: a cross-project `ask`/`find` that fans out over several indexed
projects, fuses the results, and keeps **per-project provenance** so the agent
always knows which repo each hit came from.

## Transport decision

**Multi-repo lives on `dex serve` (HTTP), not stdio.**

- The stdio surface (`dex mcp`) is bound to one project resolved from cwd. That
  binding is load-bearing for the local-index, remote-shim, and HTTP-MCP paths
  alike — relaxing it would fork the scoping contract. stdio stays
  single-project.
- `dex serve` already hosts many projects behind one bearer-authed endpoint:
  `GET /v1/projects` lists them, each addressed as `/v1/projects/{id}/...`. It
  is the natural home for a query that reads across several of them at once.

Proposed endpoint:

```
POST /v1/ask                 # cross-project; body selects the project set
POST /v1/find                # same, raw ranking
```

with a body like:

```json
{
  "question": "where is RetryPolicy consumed?",
  "projects": ["billing", "gateway", "shared-go"],   // omitted = all served
  "k": 20
}
```

`/v1/projects/{id}/ask` (single-project) is unchanged. The cross-project routes
are additive and behind the same bearer auth.

## Fan-out and fusion

1. **Fan-out.** Run the existing per-project retrieval (the same lanes `ask`
   composes today) against each selected project concurrently, bounded like the
   eval runner's errgroup. A project that errors or has no index degrades to a
   per-project `status` entry, never a whole-call failure (mirrors the existing
   structured-status contract).
2. **Fuse.** Merge the per-project ranked lists into one. **Score
   normalization across indexes is the crux** — BM25 and vector scores are not
   comparable across corpora of different sizes, so raw-score merging would let
   the largest repo dominate. Options to evaluate (decision deferred to
   implementation, measured on a multi-repo corpus cell):
   - **Rank-based fusion (RRF)** across projects — corpus-size-agnostic, the
     safe default.
   - Per-project score standardization (z-score / min-max) before a linear
     merge — finer-grained but needs per-project calibration.
3. **Provenance.** Every fused hit carries its `project` id (and root). The
   response groups or tags hits by project so the agent can route follow-up
   single-project calls. `suggested_reads` and any synthesized answer cite
   `project:path:line`.

## Synthesis

When a chat model is wired, the cross-project `ask` synthesizes one answer over
the fused evidence, with each cited span prefixed by its project. The
[faithfulness gate](semantic-search.md) (#550) extends naturally: evidence is
the union of per-project evidence, provenance-tagged.

## Security / scoping

- Same bearer auth as the rest of `/v1`. No new auth surface.
- The project set is an explicit allow-list in the request (or "all served");
  the server never widens beyond the projects it already serves.
- `shell` and other mutating-adjacent tools are **not** part of cross-project
  fan-out — this is read-only retrieval only (consistent with #551).

## Phasing

1. `POST /v1/find` cross-project: fan-out + RRF fusion + provenance. No
   synthesis. Smallest useful slice; validates fusion on a real multi-repo set.
2. `POST /v1/ask` cross-project: add synthesis + `next_action` over the fused
   evidence, faithfulness-gated.
3. Calibrate fusion (RRF vs normalized linear) on a `dex bench corpus`
   multi-repo cell; pick the default from measured cross-repo recall.

## Non-goals

- Cross-project **graph** edges (a call/import graph spanning repos). The graph
  is per-index; a federated graph is a separate, much larger effort. Out of
  scope for #552.
- Multi-repo on the stdio transport. stdio stays single-project (see above).
- Any write/mutation across repos (see #551 — dex is read-only).

## Open questions

- Fusion default: RRF vs per-project normalized linear — decide on measured
  cross-repo recall, not a priori.
- Result budget: how to divide `k` across N projects (even split vs
  score-proportional vs over-fetch-then-fuse).
- Does the agent want results **grouped by project** or **globally ranked with
  a project tag**? Likely both, behind a response flag.
- Project selection ergonomics: ids vs tags/globs over the served set.
