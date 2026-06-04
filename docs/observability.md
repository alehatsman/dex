# dex observability

## Log field conventions

All structured log output from dex follows these rules:

| Field | Type | Description |
|-------|------|-------------|
| `subsystem` | string | Source subsystem (see roster below). Attached via `.With()` at logger construction — every line from a subsystem carries it automatically. |
| `duration_ms` | int64 | Elapsed time in integer milliseconds. Always `int64`, never a string duration. Use `logx.DurMS(d)` at call sites. |
| `err` | error | Error value. Use the bare key `"err"`, not `"error"`. |
| `path` | string | File path (relative to project root). |
| `root` | string | Project root (absolute path). |
| `dir` | string | Package or directory path (relative to project root). |
| `kind` | string | Chunk kind (`file_summary`, `chunk_summary`, `package_summary`, `repo_summary`). |
| `count` | int | Row or item count in context-specific log messages. |
| `batch` | int | Current batch number (1-based) in a multi-batch loop. |
| `batch_total` | int | Total number of batches in a multi-batch loop. |
| `chunks` | int | Number of chunks in the current operation. |
| `start_line` | int | Start line of a chunk within its source file. |
| `op` | string | Optional operation name when a subsystem runs multiple distinct operations (e.g. `"embed"`, `"walk"`, `"prune"`). |

Durations are always `int64` milliseconds (`duration_ms`) for machine parseability. Never log raw `time.Duration` values (they serialize as `"123ms"` strings, which break numeric queries).

## Subsystem roster

| `subsystem` | Package | Logger origin |
|-------------|---------|---------------|
| `indexer` | `internal/index` | `index.New()` — tags the indexer logger at construction |
| `drain` | `internal/index` | `index.New()` — derived alongside `indexer` from the same base logger |
| `watch` | `internal/watch` | `watch.New()` — tags at construction |
| `graph` | `internal/graph` | `graph.New()` — tags at construction |
| `mcp` | `internal/mcp` | `mcp.(*Server).RunHTTP()` — tags before the handler loop |

`indexer` and `drain` share a common parent logger (set by the caller) but carry distinct `subsystem` tags so their lines are independently filterable.

## Adding a new subsystem

1. At the package's `New()` / construction point, derive the logger:
   ```go
   logger = logger.With("subsystem", "mysubsystem")
   ```
2. If the package has distinct internal components (like indexer/drain), derive separate loggers from the same base _before_ tagging either:
   ```go
   subA := base.With("subsystem", "a")
   subB := base.With("subsystem", "b")
   ```
3. Use `logx.DurMS(elapsed)` for any timing field.

## Example queries

### grep / ripgrep

```sh
# All drain activity
dex serve 2>&1 | grep 'subsystem=drain'

# Slow embed batches (>500 ms)
dex serve 2>&1 | grep 'duration_ms=' | awk -F'duration_ms=' '{print $2, $0}' | awk '$1>500'

# Watch re-index events
dex serve 2>&1 | grep 'subsystem=watch.*re-indexed'
```

### Loki (LogQL)

```logql
# All drain lines
{job="dex"} | logfmt | subsystem="drain"

# MCP requests slower than 200 ms
{job="dex"} | logfmt | subsystem="mcp" | duration_ms > 200

# Errors by subsystem
{job="dex"} | logfmt | err != "" | keep subsystem, err
```

### jq (JSON handler output)

```sh
# Switch to JSON output by setting the handler in cmd/dex/main.go's cliLogger()
dex serve 2>&1 | jq -c 'select(.subsystem=="drain") | {msg,duration_ms}'
```

## logx package

`internal/logx` is the single source of truth for shared attr helpers:

```go
// DurMS returns a slog.Attr for duration as int64 milliseconds.
logx.DurMS(time.Since(start))   // → duration_ms=<n>
```

Add new helpers here (not inline at call sites) if a field shape is reused across subsystems.
