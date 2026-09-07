package retrieve

import "sync"

// RerankStats accumulates what actually happened at the cross-encoder across a
// run, so a measurement can tell "reranked" apart from "silently not reranked".
//
// It exists because the degradation it counts is deliberately invisible:
// RerankFused falls through to the local quality rerank on a reranker outage
// without surfacing an error (#864), which is the right call for a live agent
// query — an outage must not fail a search — but leaves a benchmark unable to
// say whether its numbers came from the cross-encoder. A preflight health probe
// catches a reranker that is down for the whole run; it cannot see a breaker
// trip mid-run, a partial outage, or a server that answers /health while
// failing every rerank call (#865).
//
// Nil is the "don't measure" case and every method tolerates it, so live query
// paths pay nothing. Service is copied by value, so this must be held by
// pointer for the copies to share one counter.
type RerankStats struct {
	mu sync.Mutex
	// attempted counts eligible calls — a reranker wired AND a pool larger
	// than k. Calls that were never eligible are not degradations and must
	// not dilute the rate.
	attempted int
	// served counts calls whose ordering came from the cross-encoder,
	// including cache hits (the cached ordering IS the cross-encoder's).
	served int
}

// Observe records one eligible rerank call. served reports whether the
// cross-encoder's ordering was used, as opposed to falling through to the
// local rerank. Safe on a nil receiver.
func (s *RerankStats) Observe(served bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempted++
	if served {
		s.served++
	}
}

// Snapshot returns the counts so far. Safe on a nil receiver (0, 0).
func (s *RerankStats) Snapshot() (attempted, served int) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempted, s.served
}

// Rate returns served/attempted and whether any call was eligible. ok=false
// means the cross-encoder was never reached for a reason that is not a
// degradation — no reranker wired, or every pool at or below k — and a caller
// must not report 0.0 as if it were an outage.
func (s *RerankStats) Rate() (rate float64, ok bool) {
	attempted, served := s.Snapshot()
	if attempted == 0 {
		return 0, false
	}
	return float64(served) / float64(attempted), true
}
