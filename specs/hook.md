---
id: hook
status: living
last_verified: c4b4bdc
owners: [aleh]
covers:
  - "cmd/dex/hook.go"
  - "cmd/dex/hook_inject.go"
  - "cmd/dex/hook_observe.go"
  - "cmd/dex/hook_redirect.go"
  - "cmd/dex/hook_rewrite.go"
---
# Hook

## Intent

`dex hook` is the Claude Code hook integration: four subcommands map onto four
Claude Code hook events and together ensure each turn starts with relevant
context, search commands are transparently upgraded to semantic equivalents, and
large file reads are compressed before they consume context. The hooks are
surfaced as routing rules written by `dex setup`, run as short-lived subprocess
invocations, and must never block the Claude session — every handler silently
passes through on any error.

## Behavior

### Registration (`dex setup`)

- WHEN `dex setup` runs it writes a routing-rules block (between
  `# dex — semantic search & context routing` and `<!-- /dex -->` markers,
  tagged `<!-- dex-rules-v2 -->`) to `$CLAUDE_CONFIG_DIR/rules/dex.md`
  (defaulting to `~/.claude/rules/dex.md`). The write is idempotent: if the
  deployed block equals the canonical `rulesContent` constant, the file is left
  unchanged.
- WHEN the deployed block contains the start marker but a different version
  string or drifted content, `dex setup` replaces only the dex block,
  preserving surrounding content.

### `dex hook inject` — UserPromptSubmit

- WHEN Claude Code fires `UserPromptSubmit`, `dex hook inject` reads the JSON
  payload from stdin (3 s timeout), extracts the `prompt` field, and writes
  `{"additionalContext": "..."}` to stdout so Claude sees injected context
  before processing the turn. No output is written when the combined context
  is empty.
- IF the prompt is fewer than 4 words (confirmations, "yes", "ok", etc.),
  the ask query is skipped and only nudge/session blocks are emitted.
- WHERE a project index exists and the prompt is substantive, `inject` calls
  `ContextRouter` with `K=6` and `NoInline=true` (paths + reasons only, no
  raw file bodies) and a 10 s timeout, then formats the result as a
  `## Dex: relevant context` block listing suggested reads (with optional
  `path:startLine-endLine` ranges and reasons) and any matched symbols.
- IF the context router fails or returns a non-ok status, `inject` falls back
  to emitting only the nudge and session blocks (no retrieval context), never
  an error.

### One-time rules nudge

- WHEN the deployed routing rules are missing, contain no dex markers, carry an
  outdated version string, or have drifted from canonical content, `inject`
  prepends a `[DEX]` warning instructing the user to run `dex setup`.
- The nudge is debounced by a sentinel file at
  `$XDG_DATA_HOME/dex/rules-nudge-sentinel` with an 8 h TTL: the nudge fires
  at most once per 8 h window regardless of how many turns are processed. When
  the sentinel file cannot be written (e.g. permissions), the nudge fires on
  every prompt.
- WHEN the rules are in sync, no nudge is emitted.

### Per-prompt schemas nudge

- WHEN dex MCP tool schemas have not yet been loaded via `ToolSearch` this
  session, `inject` appends a `[DEX] Schemas not loaded` reminder instructing
  Claude to call `ToolSearch` as its first action.
- The schemas-loaded state is tracked by a sentinel file at
  `$XDG_DATA_HOME/dex/schemas-loaded-sentinel` with a 30 min TTL. The sentinel
  is created by `hookObserve` when it records a `ToolSearch` tool call.
- IF the sentinel is absent or older than 30 min, the nudge fires on every
  prompt; once the sentinel exists and is fresh, the nudge is suppressed for
  the remainder of the session window.

### Active-session block

- WHEN the project's session has a non-empty task AND contains at least one
  note OR at least three file touches, `inject` prepends a `[DEX] Active
  session` block to the injected context, summarising the task, notes (capped
  at 600 runes with an ellipsis), and the working-set file count.
- IF context utilization is estimated above the `compress` threshold (>60% of
  the configured context window), a `[DEX] Context pressure` line is appended
  indicating the pressure level, utilization percentage, and a suggestion to
  call `session(action=recap, budget=4000)`. The window size defaults to
  128 000 tokens and can be overridden via `DEX_CONTEXT_WINDOW`.
- IF the session is empty (no task, or index not yet created), no session block
  is emitted.

### `dex hook rewrite` — PreToolUse (Bash)

- WHEN Claude Code fires `PreToolUse` for a Bash tool call, `hookRewrite`
  reads the `command` field and attempts to rewrite known search commands:
  - `rg PATTERN` (no flags) → `dex find . "PATTERN"`
  - `rg PATTERN PATH` (no flags) → `dex find PATH "PATTERN"`
  - `grep [-rniIElh]* PATTERN [PATH]` (with `-r`, no pipes/redirects) →
    `ORIG 2>&1 | dex compress-stdin --command grep`
- IF the command contains any pipe (`|`), semicolon, subshell, redirect, or
  backtick, it passes through unchanged.
- IF a rewrite applies, the hook returns a `hookSpecificOutput` object with
  `permissionDecision: "allow"` and `updatedInput` containing the rewritten
  `command`. All other `tool_input` fields are preserved.
- IF no rewrite rule matches, or the payload cannot be parsed, the hook emits
  the constant `hookAllow` pass-through and returns.

### `dex hook redirect` — PreToolUse (Read)

- WHEN Claude Code fires `PreToolUse` for a `Read` / `ReadFile` / `read` /
  `read_file` tool call, `hookRedirect` checks whether the target file exceeds
  400 lines.
- IF the file is fewer than 400 lines, is unindexed, or yields no graph symbols
  of a top-level kind (`function`, `method`, `struct`, `interface`, `type`,
  `class`), the hook emits `hookAllow` and passes through unchanged.
- WHERE the file exceeds 400 lines and has indexed top-level symbols, `redirect`
  builds a signatures view: a comment header followed by one declaration line
  per symbol (the exact source line at `start_line`, bodies dropped), each
  annotated with its line number and kind. The view is written to a temp file
  (`dex-redirect-*.<ext>`), and the hook returns an `updatedInput` with `path`
  pointing to the temp file. The original file is never modified.
- IF the index is not yet created, the project cannot be resolved, or the temp
  file write fails, the hook emits `hookAllow` and falls back to a normal Read.
- The signatures-view lookup is bounded by a 3 s timeout.

### `dex hook observe` — PostToolUse / Stop / PreCompact

- WHEN any `PostToolUse`, `Stop`, or `PreCompact` hook fires, `hookObserve`
  appends a compact JSON event record `{"ts": <unix>, "tool_name": "...",
  "tokens": <rough-estimate>}` to `$XDG_DATA_HOME/dex/hooks.jsonl`
  (creating the file and directory as needed). No stdout output is produced.
  The token estimate is `len(tool_input_bytes) / 4`.
- WHERE the tool name is `ToolSearch`, `observe` also touches the
  schemas-loaded sentinel so `schemasNudge` in `hookInject` stops firing for
  the next 30 minutes.
- WHEN the hook event is `PreCompact`, `observe` additionally sends
  `POST /compact` to the proxy at `ANTHROPIC_BASE_URL` (with
  `X-Dex-Proxy-Token` if `DEX_PROXY_TOKEN` is set) to record a budget compact
  event and advance the session window counter. The POST has a 2 s client
  timeout and is fire-and-forget: all errors are silently swallowed.
- IF the log directory cannot be created, or the file cannot be opened,
  `observe` returns nil (no error propagated).

### Output format and error contract

- All handlers read exactly one JSON object from stdin within 3 s; if stdin is
  empty or the read times out, the handler returns nil immediately.
- `PreToolUse` handlers (`rewrite`, `redirect`) always produce valid JSON to
  stdout: either the `hookAllow` constant or a `hookSpecificOutput` object.
  They never write to stderr in normal operation.
- `UserPromptSubmit` (`inject`) writes `{"additionalContext": "..."}` to stdout
  when context exists; writes nothing when the combined output is empty.
- `PostToolUse`/`Stop`/`PreCompact` (`observe`) writes nothing to stdout.
- Every handler fails open: any parse error, I/O failure, index miss, or
  timeout causes the handler to emit pass-through output (or silence for
  observe) and return nil — Claude is never blocked or shown an error from a
  hook.

## Non-goals

- **Hook registration into `~/.claude/settings.json`**: hooks are surfaced as
  routing rules only; `dex setup` does not write settings.json hook entries
  — the user wires them manually per project.
- **Network calls to external services**: all retrieval is against the local
  dex index. The only outbound call is the fire-and-forget `POST /compact` to
  the locally-running proxy.
- **Repo mutation**: no hook writes to the working tree or the index.
- **Persistent process**: hooks are ephemeral subprocesses invoked per event,
  not a daemon.
- **Hook chaining or ordering guarantees**: hooks fire in the order Claude Code
  invokes them; dex makes no assumptions about other installed hooks.

## Checklist

- [x] `dex hook inject` is invoked as a `UserPromptSubmit` hook and emits `{"additionalContext": "..."}`.
- [x] Retrieval is skipped for prompts shorter than 4 words; pass-through with nudge/session context only.
- [x] Context router is called with `K=6`, `NoInline=true`, and a 10 s timeout; failures degrade gracefully to nudge+session only.
- [x] Rules nudge fires at most once per 8 h (sentinel at `$XDG_DATA_HOME/dex/rules-nudge-sentinel`); suppressed when rules are in sync.
- [x] Schemas nudge fires per-prompt until a `ToolSearch` PostToolUse is observed; suppressed for 30 min after (sentinel at `$XDG_DATA_HOME/dex/schemas-loaded-sentinel`).
- [x] Active-session block emitted when task is set AND (notes exist OR ≥3 file touches); includes budget-pressure warning above compress threshold.
- [x] `DEX_CONTEXT_WINDOW` overrides the 128 000-token default for budget estimation.
- [x] `dex hook rewrite` rewrites plain `rg` and recursive `grep` calls; passes through compound commands unchanged.
- [x] `dex hook redirect` redirects Read calls on files >400 lines with indexed top-level symbols to a temp signatures view; passes through otherwise.
- [x] `dex hook observe` appends to `hooks.jsonl`, touches schemas sentinel on `ToolSearch`, sends fire-and-forget `POST /compact` on `PreCompact`.
- [x] All handlers fail open: errors produce pass-through output (or silence for observe) and return nil.
- [x] Verified against the code by the verify workflow (flip to `living`)
