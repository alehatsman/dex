---
id: swarm-context-spine
status: draft
last_verified: b4e4b1c
owners: [aleh]
covers:
  - "internal/store/store_agent.go"
  - "internal/store/store_share.go"
  - "internal/mcp/context.go"
  - "internal/mcp/server_knowledge.go"
tracking: alehatsman/dex#169
---

# Swarm context spine: make good context compound across parallel agents

## Goal

dex is the only process that sees the whole swarm — every concurrent Claude Code
session on one repo spawns its own `dex mcp` but they share the on-disk SQLite.
The 2026 multi-agent failure mode is "bad context compounds across parallel
agents" + a cold-start tax paid once per agent. Three coupled surfaces turn the
already-shipped-but-dead substrate (`agents`, `agent_messages`, `share_cache`)
into a spine where *good* context compounds:

1. **S1 — claim map (#170):** an agent announces which files/symbols it is
   editing; a peer's `ask()` surfaces the overlap as a **trust-envelope caveat**
   ("agent-A is editing `internal/mcp/server.go`"). Directly closes the #159
   divergence (my force-push silently rebased a sibling; a claim would have
   warned it on its next tool call).
2. **S2 — findings bus (#168 spiked → promote):** a peer's `category=finding`
   messages fold into the `ask()` pack's `knowledge_facts` slot, provenance-
   tagged `[peer-agent:<id>]`, recall-matched on the question. GATE A proved the
   read half; this promotes the throwaway to a real surface **with vector recall**
   (see Constraint 1) and **liveness** (Constraint 2).
3. **S3 — shared warm cache (#171):** `share_cache` serves a subagent's first
   read of a file the parent already compressed, killing the per-agent cold-start
   tax on a hot working set.

None pays off alone; all three ride the same coordination substrate and the same
"peer's next tool call is the delivery channel" model. This is the only bet in
the agent-UX vision with real design risk — hence the #168 gate, now passed.

## Non-goals (from #169)

- **No edit/write verb.** Read-only surface (#551) stays. The spine surfaces peer
  state; it never mutates a peer's tree or arbitrates edits.
- **No cross-repo.** Multi-repo retrieval (#176) is a different scaling axis.
- **No live async push.** stdio MCP only speaks on a tool call; delivery is the
  peer's *next* `ask`/`look`/`act` (seconds-to-instant), never a mid-run
  interrupt. A `dex serve` daemon push is a later, separate concern.
- **Swarming is not a reflex.** The spine makes coordination cheap, not mandatory;
  a solo session pays zero cost (empty bus → empty folds, no envelope noise).

## Substrate that already exists (verified `b4e4b1c`)

- **Bus:** `agent_messages` table + `agent_messages_fts` FTS5 virtual table +
  ai/ad/au triggers (`store_migrate.go:247-269`); `agents` table
  (`store_migrate.go:241`). Store methods: `AgentAnnounce`, `AgentList`,
  `AgentPost(agentID,topic,category,body)`, `AgentRead(topic,category,query,
  sinceID,limit)` (`store_agent.go:31-133`). **Fully unwired** — no CLI, no MCP
  verb. `buildAgentFTSQuery` (`store_agent.go:152`) quotes each word → implicit
  **AND**.
- **Warm cache:** `share_cache` table (`store_migrate.go:273`, keyed on
  `path` + `content_hash`); `SharePush(path,hash,content,pushedBy)`,
  `SharePull(path,currentHash)→(content,hits,ok)`, `ShareList`, `ShareClear`
  (`store_share.go:23-96`). `SharePull` already content-hash-gates staleness and
  evicts a stale entry on read. **Unwired.**
- **Fold point:** `loadContextFacts` (`context.go:346`) fills
  `ContextOutput.KnowledgeFacts` from `recallFacts(ctx,st,question,5,…)`
  (`context.go:355`), formatted `"["+Archetype+"] "+capFactBody(Body)`.
- **Embedder (S2 Constraint 1 target):** facts are embedded by `embedFact`
  (`server_knowledge.go:581`) and recalled by `recallFacts` →
  `KnowledgeQueryVec(queryVec,k,minSim)` (`store_knowledge.go:852`). This is the
  exact embedder the bus must share.
- **Cross-process signalling precedent:** `SetIndexing`/`IndexingInProgress`
  (one `meta` row, UnixNano + TTL) surfaced live via the envelope
  (`index_signal.go`, `look_freshness.go`, #152) — the template for S1's caveat.
- **Liveness (S2 Constraint 2 dep):** `internal/mcp/referent.go` `ExtractReferents`
  + `annotateLiveness` (#167 Part 2, `dbd0868`) already flags a fact whose
  `path:line`/symbol referent went dead. A peer finding is exactly an unverified,
  short-lived fact — it reuses this wholesale.

## Design

### S2 — findings bus (promote the #168 spike)

The spike folded `category=finding` messages into `KnowledgeFacts` and passed
GATE A. Two findings from the spike are load-bearing and become requirements:

**Constraint 1 — vector recall, not FTS-AND.** The spike had to add an
`AgentReadAny` (FTS **OR** over salient tokens) stopgap because
`buildAgentFTSQuery`'s implicit-AND never recalls a terse finding from a
natural-language question. The real surface must **embed each posted finding
through the same `embedFact` path** and recall via a bus-side mirror of
`KnowledgeQueryVec` (cosine over the finding vector, same `minSim` floor). Bus
findings and durable facts then rank on one axis. FTS stays only as a keyword
fallback when no embedder is configured (`DEX_EMBED_ENGINE=none` / BM25-only).

- New store method `AgentQueryVec(ctx, queryVec, category, k, minSim)` mirroring
  `KnowledgeQueryVec` — same hybrid rank shape, scoped to `agent_messages` with a
  vector column added to the table (migration; nullable, back-compat).
- `AgentPost` grows an embed side-effect on `category=finding` (best-effort, same
  as `embedFact`: no embedder → row still stored, FTS-only recall).

**Constraint 2 — provenance + liveness.** A peer finding is unverified. It folds
in tagged `[peer-agent:<id>]` (never `[<archetype>]`, so the agent can tell a
peer's claim from a durable fact) and runs through `annotateLiveness`
(`referent.go`) exactly like a recalled fact — a peer finding naming a dead
`path:line` surfaces `needs_verification:true`.

**Finding lifecycle — pragmatic hybrid (not pure-ephemeral, not pure-durable).**
Findings default **ephemeral**: a `posted_at` TTL (default 24h) prunes a stale
bus so it never accretes. But a high-value finding earns durability instead of
evaporating — a **promotion path** reusing existing #167 machinery:

- while live, each recall bumps the finding's `last_retrieved` (the same
  `KnowledgeBump` write #167 Part 1 already makes for facts);
- when a peer finding keeps a **live referent** *and* clears a recall threshold
  (retrieved ≥N times across sessions), dex offers to **supersede it into durable
  knowledge** — the #167 Part 3 referent-overlap supersede prompt, reused whole;
- the acting agent (or an explicit `remember(..., supersedes=<bus-id>)`) confirms;
  unconfirmed, unreferenced findings decay on TTL.

Net: findings that keep proving useful graduate to facts; noise expires. No new
lifecycle machinery — TTL prune + `last_retrieved` bump + supersede all exist.

**Fold** — a second block in `loadContextFacts` after the `recallFacts` fold,
gated on a configured embedder + non-empty bus; empty bus → no-op, zero envelope
change for a solo session.

### S1 — claim map (#170)

A claim is a bus message: `category=claim`, `topic=<repo-relative path or
symbol>`, body = intent ("editing HandleLogin session-token path"). Reuses
`AgentPost`/`AgentAnnounce` — **no new table**.

- **Announce:** an `act` that runs an edit-adjacent command (or an explicit
  claim verb — decided in impl) posts a claim scoped to the touched paths.
  Auto-emit from `act` on a git write is the #159 destructive-op case
  (`category=ops-notice`) — the **same advisory surface**, not an escalation. A
  destructive op is *not* louder than a normal claim caveat (decided): the spine
  informs, it never gates or alarms.
- **Surface:** `ask()`/`look()` on a path with an *active* peer claim (fresh by
  `posted_at` TTL, not self) adds a **trust-envelope caveat** — the
  `indexingNotice` pattern (`index_signal.go`), not a `knowledge_facts` entry:
  "peer agent-A claims `internal/mcp/server.go` (2m ago)". Advisory; never blocks.
- Claims expire on TTL and on an explicit release (a `category=claim` tombstone
  or `AgentPost` with empty body). No lock semantics — this is a *caveat*, the
  agent still decides.

### S3 — shared warm cache (#171)

When a subagent's `look(path)` / `read` first touches a file, check
`SharePull(path, currentHash)`:

- **Hit + hash matches** → serve the parent's already-compressed context, tagged
  `[warm: pushed by <agent>]`; increments `hit_count`. Kills the cold-start
  re-compression.
- **Hit + hash differs** → `SharePull` already evicts the stale row; fall through
  to a fresh read + `SharePush` the new compression.
- **Miss** → normal read, then `SharePush(path, hash, compressed, self)` so the
  next agent warms off it.

Content-hash gating means a stale entry is never served (the #152 freshness
stance). Cache is best-effort: any store error → silent fall-through to a normal
read. `ShareClear` on reindex (the working set churns).

## Build order

S2 first — it is spiked, GATE-A-proven, and owns the epic measurement gate.
S1 next (reuses the same bus + the `indexingNotice` caveat rail; independent of
S2's vector work). S3 last — orthogonal substrate (`share_cache`), lowest risk,
pure latency/token win. Each lands as its own PR with its own before/after gate,
matching the #95/#155 epic+narrow-child pattern.

## Measurement gate (epic-level, from #169)

Fan-out harness: N agents on one task list against a fixed repo, spine-on vs
spine-off. Metrics:

- **redundant reads** — count of file reads a peer had already compressed (S3).
- **conflicting edits** — count of concurrent edits to a claimed path (S1).
- **tokens-to-solution / tool-calls-to-solution** — total across the swarm (all).
- **re-derived findings** — count of findings independently reached by ≥2 agents
  that the bus could have shared (S2; the #168 signal, scaled).

Each child ships with its own narrow gate; the harness is the integrated proof
the spine compounds rather than just adds surface. Target: measurable drop in
redundant reads + re-derived findings at no recall-quality cost to the pack.

## Edge cases

- **Solo session / empty bus** → every fold and lookup is a no-op; envelope
  byte-identical to today. Zero cost is a hard requirement.
- **No embedder** (`DEX_EMBED_ENGINE=none`) → S2 falls back to FTS-OR keyword
  recall; findings still deliver, just coarser. S1/S3 are embedder-independent.
- **Stale peer finding** (referent died, or past TTL) → S2 flags
  `needs_verification` (never silently trusted) or drops on TTL; #167 machinery.
- **Self-messages** → an agent never surfaces its own claims/findings (filter on
  `agent_id == self`).
- **Bus/cache unbounded growth** → TTL prune on read + `ShareClear`/bus GC on
  reindex; findings are ephemeral by design, not durable knowledge.
- **Agent identity** (grounded on `b4e4b1c`, not assumed) → the MCP SDK assigns
  each connection a `Session.ID()`, but the **stdio transport** (Claude Code's)
  returns **empty** — dex already falls back to a fixed `"stdio"` sentinel
  (`seen.go:38-49`), so `Session.ID()` cannot distinguish two concurrent CC
  agents. `clientInfo.Name` is a generic `"claude-code"`; `roots` is the launch
  dir (shared when two agents work one checkout — the #159 case). The real
  identity boundary is the **process** (one `dex mcp` per CC session) meeting at
  the **shared DB**. So identity is a two-tier hybrid:
  - **Default (automatic, zero-config):** dex mints a **per-process random id at
    `dex mcp` startup**, held on the `Server` struct, stamped on every claim /
    finding. Stable for the process = stable for the session, unique across
    concurrent agents. Opaque (`[peer-agent:a1b2c3]`) — sufficient for
    self-filter, provenance, and claim ownership.
  - **Override (opt-in, human-legible):** `DEX_AGENT_ID` (+ optional
    `DEX_AGENT_ROLE`) env lets the spawn harness or a CLAUDE.md instruction make
    an agent assume a named handle (`[peer-agent:reviewer]`); falls back to the
    minted id. Maps 1:1 onto the existing `AgentAnnounce(agentID, role)`.

## Validation

- S2 unit: a posted `category=finding` embeds; `AgentQueryVec` recalls it on a
  paraphrase where FTS-AND returns zero (the spike's exact failure case).
- S2 unit: a peer finding naming a dead `path:line` folds in with
  `needs_verification:true`; a fresh one folds clean; provenance tag is
  `[peer-agent:…]` not `[<archetype>]`.
- S1 unit: an active peer claim on a path surfaces the envelope caveat on that
  path; self-claims and TTL-expired claims do not.
- S3 unit: `SharePull` hit with matching hash serves warm + bumps `hit_count`;
  hash mismatch evicts + falls through; miss pushes.
- Integration: the fan-out harness on this repo's own store shows spine-on <
  spine-off on redundant reads and re-derived findings, pack recall unchanged.
- `mooncake task ci` green (build + test + vet + fmt + race) per child PR.
- Live: two real `dex mcp` sessions on one repo — agent A posts a finding /
  claims a file; agent B's next `ask` surfaces it (the #168 two-agent script,
  promoted off the throwaway binary onto the installed one).

## Out of scope (deferred)

- `dex serve` daemon-mode live async push (mid-run interrupt) — a client/transport
  limit today; revisit only if the next-tool-call channel proves too slow.
- Cross-repo swarm (#176) — different axis.
- Any lock / arbitration semantics on claims — caveat only, by design.
- Rich conflict resolution on divergent findings — surface both, let the agent
  judge (mirrors #167 Part 3's "no opposing-claim semantics" stance).
