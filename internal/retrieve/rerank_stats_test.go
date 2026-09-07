package retrieve

import (
	"sync"
	"testing"
)

// TestRerankStatsNilSafe pins the "don't measure" path: live query surfaces
// leave RerankStats nil and must not pay for, or panic on, the observation.
func TestRerankStatsNilSafe(t *testing.T) {
	var s *RerankStats
	s.Observe(true)
	s.Observe(false)
	if a, sv := s.Snapshot(); a != 0 || sv != 0 {
		t.Errorf("nil Snapshot = (%d, %d); want (0, 0)", a, sv)
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
			s.Observe(true)
		}
		rate, ok := s.Rate()
		if !ok || rate != 1.0 {
			t.Errorf("Rate = (%v, %v); want (1, true)", rate, ok)
		}
	})

	t.Run("partial outage -> fractional", func(t *testing.T) {
		s := &RerankStats{}
		for range 3 {
			s.Observe(true)
		}
		for range 1 {
			s.Observe(false)
		}
		rate, ok := s.Rate()
		if !ok || rate != 0.75 {
			t.Errorf("Rate = (%v, %v); want (0.75, true) — a breaker trip mid-run must be visible", rate, ok)
		}
	})

	t.Run("total outage -> 0.0 but ok", func(t *testing.T) {
		s := &RerankStats{}
		s.Observe(false)
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
			s.Observe(i%2 == 0)
		}()
	}
	wg.Wait()
	attempted, served := s.Snapshot()
	if attempted != 100 || served != 50 {
		t.Errorf("Snapshot = (%d, %d); want (100, 50)", attempted, served)
	}
}
