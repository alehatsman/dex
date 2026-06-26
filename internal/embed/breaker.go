package embed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// BreakerState is a snapshot of a Breaker's current state for status reporting.
type BreakerState struct {
	Open             bool
	OpenUntil        time.Time
	ConsecutiveFails int
}

// Breaker wraps an Embedder with a consecutive-failure circuit breaker.
// After Threshold back-to-back ErrUnreachable failures it opens for OpenFor,
// short-circuiting Embed/Health with ErrUnreachable so the caller's existing
// fallback path triggers immediately without waiting through retry backoff.
//
// Only reachability-shaped errors (ErrUnreachable) advance the counter;
// configuration bugs (4xx, decode errors) are surfaced every time.
type Breaker struct {
	Inner     Embedder
	Threshold int
	OpenFor   time.Duration

	mu               sync.Mutex
	openUntil        time.Time
	consecutiveFails int
	now              func() time.Time
}

// NewBreaker wraps inner with a circuit breaker. threshold ≤ 0 → 3; openFor ≤ 0 → 30s.
func NewBreaker(inner Embedder, threshold int, openFor time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = 3
	}
	if openFor <= 0 {
		openFor = 30 * time.Second
	}
	return &Breaker{Inner: inner, Threshold: threshold, OpenFor: openFor, now: time.Now}
}

func (b *Breaker) Endpoint() string  { return b.Inner.Endpoint() }
func (b *Breaker) ModelName() string { return b.Inner.ModelName() }
func (b *Breaker) BatchSize() int    { return b.Inner.BatchSize() }

// Embed short-circuits with ErrUnreachable when the breaker is open;
// otherwise delegates and records the outcome.
func (b *Breaker) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if b.isOpen() {
		return nil, fmt.Errorf("%w: breaker open", ErrUnreachable)
	}
	vecs, err := b.Inner.Embed(ctx, inputs)
	b.record(err)
	return vecs, err
}

// Health short-circuits with ErrUnreachable when the breaker is open;
// otherwise delegates and records the outcome.
func (b *Breaker) Health(ctx context.Context) error {
	if b.isOpen() {
		return fmt.Errorf("%w: breaker open", ErrUnreachable)
	}
	err := b.Inner.Health(ctx)
	b.record(err)
	return err
}

// State snapshots the breaker for status reporting.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := BreakerState{ConsecutiveFails: b.consecutiveFails}
	if now := b.now(); !b.openUntil.IsZero() && now.Before(b.openUntil) {
		st.Open = true
		st.OpenUntil = b.openUntil
	}
	return st
}

func (b *Breaker) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return false
	}
	if b.now().Before(b.openUntil) {
		return true
	}
	b.openUntil = time.Time{}
	b.consecutiveFails = 0
	return false
}

func (b *Breaker) record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		b.consecutiveFails = 0
		b.openUntil = time.Time{}
		return
	}
	if !errors.Is(err, ErrUnreachable) {
		return
	}
	b.consecutiveFails++
	if b.consecutiveFails >= b.Threshold {
		b.openUntil = b.now().Add(b.OpenFor)
	}
}
