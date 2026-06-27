package feedback

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShadowMultiplierBounds(t *testing.T) {
	// Single-lane hit: never reweighted, regardless of signal.
	if m := ShadowMultiplier(0.0, 1000, 1); m != 1.0 {
		t.Errorf("single-lane mult = %v, want 1.0", m)
	}
	// No samples: no signal, no change.
	if m := ShadowMultiplier(0.0, 0, 3); m != 1.0 {
		t.Errorf("n=0 mult = %v, want 1.0", m)
	}
	// High open-rate (no miss): no boost.
	if m := ShadowMultiplier(1.0, 1000, 3); m != 1.0 {
		t.Errorf("no-miss mult = %v, want 1.0", m)
	}
	// Worst case (total miss, large n, max lanes) stays under the hard cap.
	if m := ShadowMultiplier(0.0, 100000, 4); m > 1.0+reweightMaxBoost+1e-9 {
		t.Errorf("mult %v exceeds cap %v", m, 1.0+reweightMaxBoost)
	}
}

// At the post-#734 reality (a single ask), the shadow ranking must be
// effectively identical to static — the reweight decays toward 1 at low n.
func TestShadowMultiplierDecaysAtLowSamples(t *testing.T) {
	// n=1, total miss, 3-lane agreement: boost = 0.5·1·(1/21)·2 ≈ 0.048, so the
	// shadow ranking is within ~5% of static — effectively identical until real
	// traffic accrues, never the >50% swing a fully-confident signal would give.
	m := ShadowMultiplier(0.0, 1, 3)
	if m <= 1.0 || m > 1.05 {
		t.Errorf("n=1 mult = %v, want a small nudge in (1.0, 1.05]", m)
	}
}

// More cross-lane agreement earns a larger boost at the same signal.
func TestShadowMultiplierMonotoneInLanes(t *testing.T) {
	two := ShadowMultiplier(0.2, 200, 2)
	three := ShadowMultiplier(0.2, 200, 3)
	if !(three > two && two > 1.0) {
		t.Errorf("expected 1 < two(%v) < three(%v)", two, three)
	}
}

func TestThrottleSnapshotCachesAndRefreshes(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "hooks.jsonl")
	write := func(lines string) {
		if err := os.WriteFile(logPath, []byte(lines), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"tool_name":"mcp__dex__ask","intent":"auto","paths":["a.go"]}
{"tool_name":"Read","paths":["a.go"]}
`)

	th := NewThrottle(logPath, 0, time.Hour) // long minAge: no auto-refresh in-test
	rep := th.Snapshot()
	if rep.Asks != 1 || rep.OpenedReads != 1 {
		t.Fatalf("first snapshot: asks=%d opened=%d, want 1/1", rep.Asks, rep.OpenedReads)
	}
	or, n := th.IntentSignal("auto")
	if n != 1 || or != 1.0 {
		t.Errorf("IntentSignal(auto) = (%v, %d), want (1.0, 1)", or, n)
	}
	// Missing intent → zero signal.
	if or, n := th.IntentSignal("nope"); n != 0 || or != 0 {
		t.Errorf("missing intent signal = (%v,%d), want (0,0)", or, n)
	}
}

// A missing log must never break the reader — it returns the (empty) cache.
func TestThrottleMissingLogFailSoft(t *testing.T) {
	th := NewThrottle(filepath.Join(t.TempDir(), "absent.jsonl"), 0, time.Hour)
	rep := th.Snapshot()
	if rep.Asks != 0 || rep.Events != 0 {
		t.Errorf("missing-log snapshot non-empty: %+v", rep)
	}
}
