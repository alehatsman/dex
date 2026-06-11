package proxy

import "sync/atomic"

// Stats holds cumulative per-session counters for the proxy.
// All fields are safe for concurrent access via atomic operations.
type Stats struct {
	RequestsTotal      atomic.Int64
	RequestsCompressed atomic.Int64 // at least one pass saved tokens
	TokensBefore       atomic.Int64
	TokensAfter        atomic.Int64
}

// Snapshot is a JSON-serializable point-in-time view of Stats.
type Snapshot struct {
	RequestsTotal      int64   `json:"requests_total"`
	RequestsCompressed int64   `json:"requests_compressed"`
	TokensBefore       int64   `json:"tokens_before"`
	TokensAfter        int64   `json:"tokens_after"`
	TokensSaved        int64   `json:"tokens_saved"`
	CompressionRatio   float64 `json:"compression_ratio"` // tokens_saved/tokens_before; 0 if no traffic
}

// record adds one request's before/after token counts to the cumulative totals.
func (s *Stats) record(before, after int64) {
	s.RequestsTotal.Add(1)
	if after < before {
		s.RequestsCompressed.Add(1)
	}
	s.TokensBefore.Add(before)
	s.TokensAfter.Add(after)
}

// Snapshot returns a consistent point-in-time view of the stats.
func (s *Stats) Snapshot() Snapshot {
	before := s.TokensBefore.Load()
	after := s.TokensAfter.Load()
	saved := before - after
	if saved < 0 {
		saved = 0
	}
	var ratio float64
	if before > 0 {
		ratio = float64(saved) / float64(before)
	}
	return Snapshot{
		RequestsTotal:      s.RequestsTotal.Load(),
		RequestsCompressed: s.RequestsCompressed.Load(),
		TokensBefore:       before,
		TokensAfter:        after,
		TokensSaved:        saved,
		CompressionRatio:   ratio,
	}
}
