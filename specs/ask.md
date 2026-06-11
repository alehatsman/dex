---
id: ask
status: living
last_verified: c331a3c
owners: [aleh]
covers:
  - "internal/chat/**"
  - "internal/mcp/context.go"
  - "internal/mcp/answer.go"
---
# Ask

## Intent

`ask` (`dex ask` / the MCP `ask` tool) is the single retrieval-augmented entry
point an agent reaches for instead of fanning out into grep / Read /
search_semantic loops. Given a project and a free-text question, it routes the
question to a strategy, retrieves evidence from the index (semantic, symbol, and
graph lanes), and asks an LLM to compose a short prose answer grounded in that
evidence and citing `path:line`. The defining contract is that answer synthesis
is *best-effort and never blocking*: when the chat backend is absent or
unreachable, `ask` still returns the full evidence bundle so the caller can work
from `suggested_reads` + `next_action`. This spec covers the answer surface —
routing the question and composing the grounded answer; the underlying retrieval
mechanics belong to the lane specs.

## Behavior

- WHEN `ask` receives a non-empty question, it resolves the project, classifies
  an intent (explicit override, else keyword/identifier heuristics over the
  question), runs the retrieval lanes, and returns an evidence bundle
  (`suggested_reads`, `semantic_hits`, `symbols`, optional `graph`,
  `next_action`, `avoid`) alongside a `status`.
- WHEN a chat client is configured and reachable and the bundle carries usable
  evidence text, `ask` composes a grounded prose `answer` (naming the producing
  model in `answer_model`) from that evidence and returns it as the lead.
- WHERE the answer is composed, the model is instructed to use ONLY the supplied
  evidence, to answer in a few concise sentences, to cite supporting locations
  inline as `path:line`, and to never invent paths, identifiers, or APIs absent
  from the evidence.
- WHILE assembling the evidence block for synthesis, `ask` orders it the way an
  agent would read — curated reads first, then remaining semantic hits, then
  symbol signatures/docs, then graph edges — and bounds it to a byte budget sized
  for the smallest common local context window. Session task and injected
  knowledge facts are appended last so the code prefix is stable for LLM provider
  KV-cache; only the dynamic tail changes across calls.
- WHEN a session task or knowledge facts are present for the project, `ask`
  appends them as a SESSION CONTEXT block at the tail of the evidence passed to
  the LLM, so the model understands what the agent is working on and avoids
  re-discovering known facts.
- IF the chat client is absent, returns `ErrUnreachable`, or yields an empty
  answer, `ask` leaves `answer` empty and returns the evidence bundle unchanged
  with `status` still `ok` — synthesis failure never becomes a caller error.
- IF the chat backend fails for a reason other than unreachability, `ask`
  appends a short "answer synthesis skipped" note to the `hint` so the caller
  knows why the answer is missing.
- WHEN the same question, intent, model, and evidence recur, `ask` serves the
  previously composed answer from a bounded in-memory cache; a re-index that
  changes the retrieved evidence (or a different model) naturally misses, so no
  explicit invalidation is needed.
- WHEN no index exists for the project, retrieval finds nothing, or the embedding
  service is unreachable with no symbol hits, `ask` returns the corresponding
  `status` (`no-index` / `ok` with a no-matches hint / `embedding-service-unreachable`)
  and a `next_action` steering the caller to grep, never a fabricated answer.
- WHERE the chat leg talks to the backend, it POSTs to an OpenAI-compatible
  `/v1/chat/completions` endpoint, returns the first choice, and distinguishes a
  transport failure (`ErrUnreachable`) from a non-2xx HTTP status or a server
  error payload so the answer surface can degrade rather than surface a broken
  result.
- WHERE reachability is probed, the chat client does a cheap `GET /v1/models`
  rather than a real generation, so a cold model load doesn't masquerade as an
  unreachable backend.
- WHEN invoked as `dex ask`, the CLI maps its flags one-to-one onto the same
  router input (`--intent`, `--k`, `--no-inline`, `--format text|json`), so a CLI
  call and an MCP tool call produce the same bundle and answer.

## Non-goals

- The retrieval lanes themselves — embedding the query and scoring chunks
  (**semantic-search**), exact identifier lookup (**symbol-search**), and
  caller/callee/topology edges (**graph**) — are owned by those specs; `ask`
  only orchestrates and synthesizes over their output.
- Producing and storing the index those lanes read (**indexing**) and its
  on-disk format (**storage**).
- The embedding client contract — batching, vectors, dimensions, and its
  distinct unreachable handling (**embedding**).
- MCP tool registration, transport, and the HTTP surface that exposes `ask`
  (**mcp-server**, **http-api**).

## Checklist

- [x] `ask` routes a free-text question and returns an evidence bundle with `status`, `suggested_reads`, and `next_action`.
- [x] A grounded prose `answer` with `path:line` citations is composed when a chat client is configured and reachable.
- [x] Answer synthesis is best-effort: absent/unreachable/empty chat leaves `answer` empty with `status` still `ok`.
- [x] Non-unreachable chat failures are surfaced as a `hint` note, not an error.
- [x] Evidence is ordered (reads → semantic → symbols → graph → SESSION CONTEXT) and byte-bounded for the local context window; session/knowledge appended last for KV-cache stability.
- [x] Repeated (question, intent, model, evidence) answers are served from a bounded in-memory cache.
- [x] The chat client distinguishes transport `ErrUnreachable` from non-2xx / server-error responses and probes reachability via `GET /v1/models`.
- [x] `dex ask` mirrors the MCP `ask` input one-to-one (intent/k/no-inline/format).
- [x] Verified against the code by the verify workflow (flip to `living`)
