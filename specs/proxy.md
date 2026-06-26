---
id: proxy
status: living
last_verified: c4b4bdc
owners: [aleh]
covers:
  - "internal/proxy/**"
  - "cmd/dex/proxy.go"
---
# Proxy

## Intent

`dex proxy` is a loopback Anthropic API pass-through that sits between Claude
Code and the Anthropic API. It intercepts each `/v1/messages` request, runs it
through a deterministic pipeline (model routing, history pruning, effort
injection, tool-description compression, cache-breakpoint alignment), then
forwards it upstream — with SSE streaming passing through unbuffered. It does
not do semantic search, index code, or inspect content for any purpose other
than the pipeline passes listed here; it is a cost and context-window reduction
layer, not a feature surface.

## Behavior

- WHEN `dex proxy` starts, it binds `127.0.0.1:8788` by default; a different
  address is accepted via `--addr` or `DEX_PROXY_ADDR`; a non-loopback bind
  (`--addr 0.0.0.0:…`, a public IP, or the bare `:port` form) is rejected at
  startup unless `DEX_PROXY_TOKEN` is also set. The bind error is synchronous
  — it surfaces to the caller, not from a goroutine.

- WHERE `DEX_PROXY_TOKEN` is set, every incoming request (including `GET
  /stats`) must carry the token in the `X-Dex-Proxy-Token` header; a missing
  or mismatched token returns 401. Without it the proxy binds loopback-only and
  accepts any request from the loopback interface without a token check.

- WHILE forwarding, the upstream API key (Authorization / x-api-key) is
  forwarded untouched and never persisted; request/response bodies are never
  logged; `X-Forwarded-*` headers are stripped from outbound requests; and the
  `X-Dex-Proxy-Token` header is stripped so the loopback token never reaches
  the upstream. `Accept-Encoding` is also stripped so Go's transport owns
  gzip negotiation and the usage-tee writer sees plain SSE bytes.

- WHEN the request is `POST /v1/messages`, the proxy runs the body through the
  following pipeline in order before forwarding:
  1. **RouteModel** — rewrites the `model` field based on input token count
     (opt-in; off by default).
  2. **PruneRequestBody** — rewrites old tool_result blocks outside the
     keep-recent window to compact stubs.
  3. **CCR re-injection** — collapses in-window re-reads of already-teed files
     to markers, then expands any markers in the body to their original bytes
     (no-op when CCR is off).
  4. **ApplyEffort** — injects a reasoning-budget field (opt-in; no-op when
     `DEX_PROXY_EFFORT` is unset).
  5. **ColdPrefixRepack** — if no `/v1/messages` request has been forwarded for
     ≥ 600 seconds (2 × Anthropic's 5-minute cache TTL), latches a
     `repacking` flag and escalates tool-description compression from `full`
     to `terse` for this and all subsequent turns. The first request in a
     session never triggers repack (requires a prior touch). Always-on; no
     flag required.
  6. **CompressToolDescriptions** — rewrites tool `description` fields in the
     `tools` array (no-op in `full` mode, the default; escalated to `terse`
     when the cold-prefix repack flag is latched).
  7. **AlignCacheBreakpoints** — strips any client-set `cache_control` markers
     and re-places up to 4 `cache_control:{type:"ephemeral"}` breakpoints on
     the stable prefix.

- WHEN PruneRequestBody runs, it leaves the trailing `DefaultKeepRecent` (10)
  messages verbatim. The prune boundary is rounded down to the nearest
  `PruneStride` (16) multiple so the stable byte prefix stays identical
  turn-over-turn and keeps the provider cache warm. Content shorter than 200
  characters (`minPruneChars`) is never rewritten. Results that carry a
  `<lc_safe>` marker, have a positive `saved_pct` (already compressed by dex),
  or contain test/build output are kept verbatim even when old. File-read
  results are replaced with a re-read stub (`[earlier file read (N lines)
  pruned to save tokens; re-read if needed]`); shell and command results are
  replaced with a head/tail summary.

- WHERE CCR is enabled (`--ccr` / `DEX_PROXY_CCR`, off by default), file-read
  stubs also embed a content-addressed recovery marker
  (`dex:lc_expand:<16-hex-hash>`). The original bytes are stored under
  `~/.cache/dex/proxy/tee/` with a 24-hour TTL. The `read(ccr_hash=…)` MCP
  tool can retrieve any live entry by hash. Content below 512 bytes is not
  teed. GC is self-throttled (at most once per 600 seconds per process) and
  fail-open — a disk error never aborts the request.

- WHEN RouteModel is enabled (`--route-model on` / `DEX_PROXY_ROUTE_MODEL=on`,
  off by default), the `model` field is rewritten based on the input token
  count of the request: below `--route-low-threshold` (default 2 000) the
  model becomes `--route-low-model` (default `claude-haiku-4-5-20251001`);
  below `--route-mid-threshold` (default 20 000) it becomes `--route-mid-model`
  (default `claude-sonnet-4-6`); above both thresholds the model field is
  unchanged. Thresholds and model names are also configurable via
  `DEX_PROXY_ROUTE_LOW_THRESHOLD`, `DEX_PROXY_ROUTE_LOW_MODEL`,
  `DEX_PROXY_ROUTE_MID_THRESHOLD`, `DEX_PROXY_ROUTE_MID_MODEL`.

- WHEN ApplyEffort is enabled (`--effort low|medium|high` /
  `DEX_PROXY_EFFORT`), it injects a provider-specific reasoning-budget field
  into requests that do not already set one, and only for known reasoning
  models. For Anthropic models it writes
  `thinking.budget_tokens` (low=1 024, medium=4 096, high=10 000); for OpenAI
  o-series it writes `reasoning_effort`; for Gemini it writes
  `generationConfig.thinkingConfig.thinkingBudget`. If the client already set
  any of those fields, or the model is not a known reasoning model, the pass is
  a no-op. Fail-open: a JSON parse or marshal error also leaves the body
  unchanged.

- WHEN CompressToolDescriptions runs in `terse` mode, each tool `description`
  is abbreviated via the GENERAL dictionary, Example/Note/See-also lines are
  dropped, and at most the first 3 surviving lines are kept. In `lazy` mode
  only the first line (truncated to 77 runes) is kept with a pointer suffix.
  `name` and `input_schema` are never touched. The mode is static per session
  so the tools block compresses to identical bytes every turn (keeping the
  provider cache warm). IF `ENABLE_TOOL_SEARCH` is set, any aggressive mode is
  clamped to `full` to preserve full tool docs for tool-selection.

- WHEN AlignCacheBreakpoints runs, it strips all existing `cache_control`
  markers from the request, then places up to 4 `cache_control:{type:"ephemeral"}`
  markers on the stable prefix (tool definitions, system prompt, early turns).
  The volatile tail is left uncached. It runs after pruning so the marked
  prefix is the deterministic post-pruned region. The minimum cacheable prefix
  is model-dependent: 1 024 tokens for Sonnet 4.5/4.1/4.0/3.7; 2 048 for
  Sonnet 4.6, Fable, Haiku 3.x; 4 096 (safe-high) for Opus 4.x, Haiku 4.5,
  and unknown models. A breakpoint whose prefix falls below the floor is not
  placed (it would be silently ignored by Anthropic, wasting a slot).

- WHEN the request is `GET /stats`, the proxy returns a JSON `Snapshot` of
  cumulative per-session counters: total requests, compressed requests,
  tokens before/after pruning, cache breakpoints placed, tool descriptions
  compressed, re-reads after stub, dup reads in window, provider-reported
  input/output/cache tokens, session cost in USD, and routed requests. The
  `dex proxy --stats` subcommand fetches this endpoint from a running proxy
  and prints a human-readable summary, then exits.

- WHEN the request is `POST /compact`, the proxy records a context-compaction
  event in the per-session budget log (no-op if the log was not opened). This
  endpoint is called by the PreCompact hook.

- WHILE running, the proxy writes a per-session budget event log (one JSONL
  file per session) under `~/.cache/dex/sessions/` and updates a
  `current_session` pointer file so the PreCompact hook can locate it. Each
  response's provider-reported token usage is appended to the log. Cost is
  computed from provider usage and forwarded to the project's SLO tracker
  (`cost_usd` entries in `.dex/config.yml`).

- WHILE the proxy is active, SSE streaming responses pass through with
  `FlushInterval=-1` (each chunk forwarded as it arrives) so the agent sees
  tokens stream rather than waiting on a buffer.

## Non-goals

- **Semantic search or indexing.** The proxy does not consult or modify the dex
  index; it is a pure request-pipeline layer.
- **Content inspection for logging or analytics.** Request and response bodies
  are never persisted; the API key and tool results are never logged.
- **General HTTP proxying.** Only `POST /v1/messages` runs through the
  pipeline; other paths are forwarded verbatim (or handled as `GET /stats` /
  `POST /compact`). The proxy is not a general API gateway.
- **Multi-project or multi-user support.** The proxy is a single-process,
  single-session tool; it keeps one Stats accumulator and one budget log per
  process lifetime.
- **Upstream model behavior.** The proxy rewrites request fields deterministically
  before forwarding; it does not interpret, summarize, or alter response content.

## Checklist

- [x] Binds loopback (`127.0.0.1:8788`) by default; non-loopback rejected at startup unless `DEX_PROXY_TOKEN` set
- [x] `DEX_PROXY_TOKEN` gates all routes via `X-Dex-Proxy-Token`; 401 on missing/wrong token
- [x] API key forwarded untouched; bodies never logged; `X-Forwarded-*` and `X-Dex-Proxy-Token` stripped outbound; `Accept-Encoding` stripped for clean SSE tee
- [x] Pipeline order: RouteModel → PruneRequestBody → CCR → ApplyEffort → ColdPrefixRepack → CompressToolDescriptions → AlignCacheBreakpoints
- [x] PruneRequestBody: `DefaultKeepRecent=10` messages kept verbatim; boundary rounded to `PruneStride=16`; `minPruneChars=200`; `<lc_safe>`, already-compressed, and test/build results preserved; file reads → re-read stub; commands → head/tail
- [x] CCR (off by default): file-read stubs carry `dex:lc_expand:<16-hex>` marker; bytes stored under `~/.cache/dex/proxy/tee/`; 24-hour TTL; `ccrMinBytes=512`; fail-open
- [x] RouteModel (off by default): rewrites `model` by token-count thresholds (low<2 000→haiku, mid<20 000→sonnet); configurable via flags/env
- [x] ApplyEffort (off by default): injects provider-specific reasoning-budget field; skips if client already set it or model not a reasoning model; fail-open
- [x] ColdPrefixRepack (always-on): tracks last-touch time in `~/.cache/dex/proxy/cold_prefix_touch.json` (atomic write-rename, 30 s throttle); latches `repacking` flag when elapsed > 600 s; never acts on first sighting; escalates `full` → `terse` tool-description mode once latched; `cold_repack_latched` counter in `/stats`
- [x] CompressToolDescriptions: full (no-op)/terse/lazy; `name`+`input_schema` untouched; deterministic output; clamped to `full` when `ENABLE_TOOL_SEARCH` set; effective mode may be escalated by ColdPrefixRepack
- [x] AlignCacheBreakpoints: strips client markers, places up to 4; model-dependent minimum prefix; runs after pruning
- [x] `GET /stats` returns JSON `Snapshot`; `dex proxy --stats` fetches and prints it
- [x] `POST /compact` records compaction event in budget log
- [x] Budget log at `~/.cache/dex/sessions/`; per-turn usage appended; cost forwarded to SLO tracker
- [x] SSE streaming: `FlushInterval=-1`, unbuffered passthrough
- [x] Verified against the code by the verify workflow (flip to `living`)
