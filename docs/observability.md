# dex observability

dex uses Go's `log/slog` throughout. All structured log output goes to
`os.Stderr` in text format by default.

## Canonical attribute schema

The helpers in `internal/logx/logx.go` produce these canonical keys. Use
them instead of bare `slog.String`/`slog.Int` to keep log output consistent
and machine-parseable across subsystems.

| Key            | Helper              | Type    | Meaning |
|----------------|---------------------|---------|---------|
| `duration_ms`  | `logx.DurMS(d)`     | int64   | elapsed time in milliseconds |
| `path`         | `logx.Path(s)`      | string  | filesystem or logical path (do not use `"root"` or `"dir"`) |
| `phase`        | `logx.Phase(s)`     | string  | pipeline phase (`start`, `walk`, `embed`, `summarize`, `done`, …) |
| `kind`         | `logx.Kind(s)`      | string  | chunk or entity kind |
| `count`        | `logx.Count(n)`     | int     | generic item count |
| `model`        | `logx.Model(s)`     | string  | model identifier |

Keys outside this set are fine for context-specific fields (`err`, `op`,
`start_line`, `debounce`, `batch`, `chunks`, …) but must not shadow canonical
keys under aliases (`"root"` or `"dir"` instead of `"path"`).

Durations are always `int64` milliseconds (`duration_ms`) for machine
parseability. Never log raw `time.Duration` values (they serialize as `"123ms"`
strings and break numeric queries).

## Subsystem roster

Subsystems attach a fixed `subsystem` key at logger construction:

```go
logger = logger.With("subsystem", "watch")
```

| `subsystem` | Package | Origin |
|-------------|---------|--------|
| `indexer`   | `internal/index` | `index.New()` |
| `drain`     | `internal/index` | derived alongside `indexer` from the same base logger |
| `watch`     | `internal/watch` | `watch.New()` |
| `graph`     | `internal/graph` | `graph.New()` |
| `mcp`       | `internal/mcp`   | `mcp.(*Server).RunHTTP()` |

## Log levels

| Level   | When |
|---------|------|
| `INFO`  | normal operational milestones (phase transitions, reindex complete, listen ready) |
| `WARN`  | recoverable errors that don't stop the run (embed failed for one chunk, stale GC error) |
| `ERROR` | fatal errors immediately before `os.Exit` or error return |
| `DEBUG` | per-file/per-chunk detail; off by default |

## grep / ripgrep recipes

```sh
# All drain activity
dex serve . 2>&1 | grep 'subsystem=drain'

# Slow embed batches (>500 ms)
dex serve . 2>&1 | grep 'duration_ms=' | awk -F'duration_ms=' '{print $2, $0}' | awk '$1>500'

# Watch re-index events
dex serve . 2>&1 | grep 'subsystem=watch.*re-indexed'

# Index phase transitions
dex index . 2>&1 | grep 'phase='
```

## jq recipes (JSON handler)

Switch to JSON output by setting `DEX_LOG_FORMAT=json` (or wiring a JSON
handler in `cliLogger()`), then:

```sh
dex serve . 2>&1 | tee dex.log

# Filter by phase
jq 'select(.phase == "walk")' dex.log

# Slowest index runs
jq 'select(.phase == "done") | {time, path, duration_ms, chunks_seen}' dex.log

# Embed errors only
jq 'select(.level == "WARN" and (.msg | contains("embed")))' dex.log
```

## Loki (LogQL)

```logql
# All drain lines
{job="dex"} | logfmt | subsystem="drain"

# MCP requests slower than 200 ms
{job="dex"} | logfmt | subsystem="mcp" | duration_ms > 200

# Errors by subsystem
{job="dex"} | logfmt | err != "" | keep subsystem, err
```

## Adding a new subsystem

1. At the package's `New()` / construction point, derive the logger:
   ```go
   logger = logger.With("subsystem", "mysubsystem")
   ```
2. For packages with distinct internal components (like `indexer`/`drain`),
   derive separate loggers from the same base before tagging either.
3. Use canonical `logx.*` helpers for timing, paths, phases, counts, and model IDs.
