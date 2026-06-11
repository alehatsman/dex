package rerank

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// stubInner is a HealthChecker that returns canned outcomes for Rerank
// and Health, recording call counts so tests can assert pass-through
// vs short-circuit behavior.
type stubInner struct {
	rerankErr   error
	healthErr   error
	rerankCalls int
	healthCalls int
}

func (s *stubInner) Rerank(_ context.Context, _ string, _ []string) ([]Score, error) {
	s.rerankCalls++
	return nil, s.rerankErr
}
func (s *stubInner) Health(_ context.Context) error {
	s.healthCalls++
	return s.healthErr
}
func (*stubInner) Endpoint() string  { return "stub" }
func (*stubInner) ModelName() string { return "stub-model" }

// fakeClock is a controllable monotonic time source. Driving the breaker's
// cooldown through it makes the open→short-circuit→recover transitions
// deterministic — no real sleeps, no dependency on wall-clock advancement
// (which is also non-monotonic under WSL2/NTP — cf. the prune clock-flake
// class, dex #32).
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestBreakerTripsAfterThreshold(t *testing.T) {
	inner := &stubInner{rerankErr: fmt.Errorf("%w: down", ErrUnreachable)}
	b := NewBreaker(inner, 3, 30*time.Second)

	for i := 0; i < 3; i++ {
		_, err := b.Rerank(context.Background(), "q", []string{"d"})
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("call %d err = %v, want ErrUnreachable", i, err)
		}
	}
	if inner.rerankCalls != 3 {
		t.Errorf("inner.rerankCalls = %d, want 3", inner.rerankCalls)
	}

	// Fourth call should short-circuit without reaching the inner client.
	_, err := b.Rerank(context.Background(), "q", []string{"d"})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("after-trip err = %v, want ErrUnreachable", err)
	}
	if inner.rerankCalls != 3 {
		t.Errorf("after-trip inner.rerankCalls = %d, want 3 (short-circuited)", inner.rerankCalls)
	}

	st := b.State()
	if !st.Open {
		t.Errorf("State.Open = false, want true")
	}
	if st.ConsecutiveFails != 3 {
		t.Errorf("State.ConsecutiveFails = %d, want 3", st.ConsecutiveFails)
	}
}

func TestBreakerSuccessResetsCounter(t *testing.T) {
	inner := &stubInner{rerankErr: fmt.Errorf("%w: down", ErrUnreachable)}
	b := NewBreaker(inner, 3, 30*time.Second)

	// Two failures.
	_, _ = b.Rerank(context.Background(), "q", []string{"d"})
	_, _ = b.Rerank(context.Background(), "q", []string{"d"})

	// One success.
	inner.rerankErr = nil
	if _, err := b.Rerank(context.Background(), "q", []string{"d"}); err != nil {
		t.Fatalf("success call err = %v", err)
	}
	if got := b.State().ConsecutiveFails; got != 0 {
		t.Errorf("ConsecutiveFails = %d, want 0", got)
	}

	// Three more reachability failures still needed to trip.
	inner.rerankErr = fmt.Errorf("%w: down", ErrUnreachable)
	for i := 0; i < 3; i++ {
		_, _ = b.Rerank(context.Background(), "q", []string{"d"})
	}
	if !b.State().Open {
		t.Errorf("expected breaker open after counter reset + 3 fails")
	}
}

func TestBreakerSkipsNonReachabilityErrors(t *testing.T) {
	// A 4xx-shaped error should not advance the counter — those are
	// configuration bugs we want surfaced every call, not masked.
	inner := &stubInner{rerankErr: errors.New("rerank: http 400: bad request")}
	b := NewBreaker(inner, 3, 30*time.Second)

	for i := 0; i < 10; i++ {
		_, _ = b.Rerank(context.Background(), "q", []string{"d"})
	}
	if got := b.State().ConsecutiveFails; got != 0 {
		t.Errorf("ConsecutiveFails = %d, want 0 (4xx should not count)", got)
	}
	if b.State().Open {
		t.Errorf("breaker opened on non-reachability errors")
	}
}

// TestBreakerReopensAfterWindow drives the full open→short-circuit→recover
// cycle through an injected clock, so the 30s cooldown is exercised
// deterministically with no real sleep (the previous version slept 15ms and
// raced the open window — a wall-clock flake, cf. dex #32).
func TestBreakerReopensAfterWindow(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	inner := &stubInner{rerankErr: fmt.Errorf("%w: down", ErrUnreachable)}
	b := NewBreaker(inner, 2, 30*time.Second)
	b.now = clk.now
	ctx := context.Background()

	// Trip the breaker (threshold 2).
	_, _ = b.Rerank(ctx, "q", []string{"d"})
	_, _ = b.Rerank(ctx, "q", []string{"d"})
	if !b.State().Open {
		t.Fatal("breaker not open after threshold")
	}

	// While open: every call short-circuits without touching the inner,
	// even as time advances up to (but not past) the cooldown boundary.
	callsAtOpen := inner.rerankCalls
	for _, step := range []time.Duration{0, 10 * time.Second, 19*time.Second + 999*time.Millisecond} {
		clk.advance(step)
		if _, err := b.Rerank(ctx, "q", []string{"d"}); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("open short-circuit err = %v, want ErrUnreachable", err)
		}
		if !b.State().Open {
			t.Fatalf("breaker closed early at +%s, total %s into a 30s window", step, clk.t.Sub(time.Unix(1_700_000_000, 0)))
		}
	}
	if inner.rerankCalls != callsAtOpen {
		t.Errorf("inner called %d times while open; want 0 (short-circuit)", inner.rerankCalls-callsAtOpen)
	}

	// Cross the cooldown boundary and let the inner recover. The next call
	// half-opens: it probes the now-healthy inner, succeeds, and resets.
	clk.advance(time.Second) // now 30.999s in, past the 30s window
	inner.rerankErr = nil
	callsBefore := inner.rerankCalls
	if _, err := b.Rerank(ctx, "q", []string{"d"}); err != nil {
		t.Fatalf("probe call after cooldown errored: %v", err)
	}
	if inner.rerankCalls == callsBefore {
		t.Error("probe call did not reach inner client after window elapsed")
	}
	if st := b.State(); st.Open || st.ConsecutiveFails != 0 {
		t.Errorf("breaker not recovered after cooldown + success (state=%+v)", st)
	}
}
