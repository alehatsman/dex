// Package logx provides canonical slog attribute helpers for dex.
//
// Canonical key set — use these helpers instead of bare slog.String/Int:
//
//	duration_ms  DurMS(d)   — elapsed time as integer milliseconds
//	path         Path(s)    — filesystem or logical path
//	phase        Phase(s)   — pipeline phase name (walk/embed/summarize/…)
//	kind         Kind(s)    — chunk/entity kind string
//	count        Count(n)   — generic item count
//	model        Model(s)   — model identifier string
//
// Subsystems attach a "subsystem" key via logger.With at construction time.
// Keys outside this set are fine for context-specific fields (e.g. "err",
// "start_line", "op") but must not duplicate canonical keys under aliases.
package logx

import (
	"log/slog"
	"time"
)

// DurMS returns a slog.Attr for elapsed time as integer milliseconds.
func DurMS(d time.Duration) slog.Attr { return slog.Int64("duration_ms", d.Milliseconds()) }

// Path returns a slog.Attr for a filesystem or logical path.
func Path(s string) slog.Attr { return slog.String("path", s) }

// Phase returns a slog.Attr for a pipeline phase name.
func Phase(s string) slog.Attr { return slog.String("phase", s) }

// Kind returns a slog.Attr for a chunk or entity kind string.
func Kind(s string) slog.Attr { return slog.String("kind", s) }

// Count returns a slog.Attr for a generic item count.
func Count(n int) slog.Attr { return slog.Int("count", n) }

// Model returns a slog.Attr for a model identifier.
func Model(s string) slog.Attr { return slog.String("model", s) }
