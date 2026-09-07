package retrieve

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/alehatsman/dex/internal/rerank"
)

// TestRerankStatsNilSafe pins the "don't measure" path: live query surfaces
// leave RerankStats nil and must not pay for, or panic on, the observation.
func TestRerankStatsNilSafe(t *testing.T) {
	var s *RerankStats
	s.Observe(nil)
	s.Observe(errors.New("boom"))
	if a, sv, to := s.Snapshot(); a != 0 || sv != 0 || to != 0 {
		t.Errorf("nil Snapshot = (%d, %d, %d); want (0, 0, 0)", a, sv, to)
	}
	if _, ok := s.Rate(); ok {
		t.Error("nil Rate reported ok=true; a nil sink has observed nothing")
	}
}

// TestRerankStatsRate covers the distinction the whole type exists for:
// "never eligible" is not "degraded", and must not be reported as a 0.0 rate.
func TestRerankStatsRate(t *testing.T) {
	t.Run("no eligible calls -> not ok", func(t *testing.T) {
		s := &RerankStats{}
		if rate, ok := s.Rate(); ok {
			t.Errorf("Rate = (%v, true) with nothing attempted; want ok=false so callers do not report an outage", rate)
		}
	})

	t.Run("fully served -> 1.0", func(t *testing.T) {
		s := &RerankStats{}
		for range 5 {
			s.Observe(nil)
		}
		rate, ok := s.Rate()
		if !ok || rate != 1.0 {
			t.Errorf("Rate = (%v, %v); want (1, true)", rate, ok)
		}
	})

	t.Run("partial outage -> fractional", func(t *testing.T) {
		s := &RerankStats{}
		for range 3 {
			s.Observe(nil)
		}
		for range 1 {
			s.Observe(errors.New("down"))
		}
		rate, ok := s.Rate()
		if !ok || rate != 0.75 {
			t.Errorf("Rate = (%v, %v); want (0.75, true) — a breaker trip mid-run must be visible", rate, ok)
		}
	})

	t.Run("total outage -> 0.0 but ok", func(t *testing.T) {
		s := &RerankStats{}
		s.Observe(errors.New("down"))
		rate, ok := s.Rate()
		if !ok || rate != 0 {
			t.Errorf("Rate = (%v, %v); want (0, true) — attempted-and-never-served is a real observation", rate, ok)
		}
	})
}

// TestRerankStatsConcurrent guards the shared-counter contract: Service is
// copied by value, so every copy writes through the same pointer, and the
// eval harness may run queries in parallel.
func TestRerankStatsConcurrent(t *testing.T) {
	s := &RerankStats{}
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				s.Observe(nil)
			} else {
				s.Observe(errors.New("down"))
			}
		}()
	}
	wg.Wait()
	attempted, served, _ := s.Snapshot()
	if attempted != 100 || served != 50 {
		t.Errorf("Snapshot = (%d, %d); want (100, 50)", attempted, served)
	}
}

// TestRerankStatsSeparatesTimeoutFromOutage pins #868: a call lost to our own
// deadline and a call lost to a dead endpoint degrade identically, but point at
// opposite fixes. Reporting a timeout as "unreachable" sends the operator after
// a healthy service — which is exactly what happened before this split.
func TestRerankStatsSeparatesTimeoutFromOutage(t *testing.T) {
	s := &RerankStats{}
	s.Observe(nil)
	s.Observe(fmt.Errorf("%w: %w after 1.5s", rerank.ErrUnreachable, rerank.ErrTimeout))
	s.Observe(fmt.Errorf("%w: dial tcp: connection refused", rerank.ErrUnreachable))

	attempted, served, timedOut := s.Snapshot()
	if attempted != 3 || served != 1 || timedOut != 1 {
		t.Errorf("Snapshot = (%d, %d, %d); want (3, 1, 1) — one served, one timeout, one true outage",
			attempted, served, timedOut)
	}

	// The fallback must still fire for a timeout, or a slow reranker starts
	// failing searches instead of degrading them.
	timeoutErr := fmt.Errorf("%w: %w after 1.5s", rerank.ErrUnreachable, rerank.ErrTimeout)
	if !errors.Is(timeoutErr, rerank.ErrUnreachable) {
		t.Error("a timeout no longer satisfies errors.Is(err, ErrUnreachable) — the degradation path at retrieve/rerank.go would stop firing")
	}
	if errors.Is(fmt.Errorf("%w: refused", rerank.ErrUnreachable), rerank.ErrTimeout) {
		t.Error("a plain outage matched ErrTimeout — the two causes are no longer distinguishable")
	}
}
