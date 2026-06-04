// Package logx provides canonical slog attribute helpers for dex.
//
// Convention:
//   - All durations log as duration_ms (int64 milliseconds) for machine parseability.
//   - Subsystems attach a "subsystem" key via logger.With at construction time.
package logx

import (
	"log/slog"
	"time"
)

// DurMS returns a slog.Attr for a duration as integer milliseconds.
// Use instead of bare "elapsed"/"took" string-formatted durations.
func DurMS(d time.Duration) slog.Attr {
	return slog.Int64("duration_ms", d.Milliseconds())
}
