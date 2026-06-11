// Package ctx provides context window budget tracking for dex sessions.
package ctx

const DefaultWindowSize = 128_000

// PressureLevel indicates how full the context window is.
type PressureLevel string

const (
	PressureNormal   PressureLevel = "normal"   // <60%
	PressureCompress PressureLevel = "compress" // 60–80%: switch to terse views
	PressureEvict    PressureLevel = "evict"    // 80–90%: drop low-value entries
	PressureCritical PressureLevel = "critical" // >90%: immediate action required
)

// Ledger is a lightweight snapshot of context window utilization for a session.
type Ledger struct {
	WindowSize  int // total context window in tokens
	UsedTokens  int // estimated tokens consumed this session
	SavedTokens int // tokens saved by compression (informational)
}

// Utilization returns the fraction of the window consumed (0.0–1.0).
func (l Ledger) Utilization() float64 {
	if l.WindowSize <= 0 {
		return 0
	}
	u := float64(l.UsedTokens) / float64(l.WindowSize)
	if u > 1 {
		u = 1
	}
	return u
}

// Remaining returns estimated tokens still available.
func (l Ledger) Remaining() int {
	r := l.WindowSize - l.UsedTokens
	if r < 0 {
		return 0
	}
	return r
}

// Pressure returns the recommended action based on current utilization.
func (l Ledger) Pressure() PressureLevel {
	u := l.Utilization()
	switch {
	case u >= 0.90:
		return PressureCritical
	case u >= 0.80:
		return PressureEvict
	case u >= 0.60:
		return PressureCompress
	default:
		return PressureNormal
	}
}

// BytesToTokens converts a byte count to an approximate token count.
// Uses the ~4 bytes/token rule of thumb common for mixed code+prose.
func BytesToTokens(bytes int64) int {
	return int(bytes / 4)
}
