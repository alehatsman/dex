package slo

import (
	"strings"
	"testing"
	"time"
)

func newTestTracker(slos []SLOEntry) *Tracker {
	return newTracker(Config{SLOs: slos})
}

func TestNoViolations(t *testing.T) {
	tr := newTestTracker([]SLOEntry{
		{Name: "tokens", Metric: MetricContextTokens, Threshold: 1000, Action: ActionWarn},
	})
	tr.RecordTokens(500)
	tr.RecordToolCall()
	if v := tr.Check(); len(v) != 0 {
		t.Fatalf("expected no violations, got %v", v)
	}
}

func TestWarnOnHardThreshold(t *testing.T) {
	tr := newTestTracker([]SLOEntry{
		{Name: "tokens", Metric: MetricContextTokens, Threshold: 100, Action: ActionWarn},
	})
	tr.RecordTokens(100)
	v := tr.Check()
	if len(v) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(v))
	}
	if v[0].SLO.Name != "tokens" {
		t.Errorf("unexpected SLO name %q", v[0].SLO.Name)
	}
	if v[0].Warning {
		t.Error("expected hard violation, got warning")
	}
	ann := v[0].Annotation()
	if !strings.Contains(ann, "[SLO:") {
		t.Errorf("annotation missing [SLO: prefix: %q", ann)
	}
}

func TestToolCallThreshold(t *testing.T) {
	tr := newTestTracker([]SLOEntry{
		{Name: "tools", Metric: MetricToolCalls, Threshold: 3, Action: ActionBlock},
	})
	tr.RecordToolCall()
	tr.RecordToolCall()
	if v := tr.Check(); len(v) != 0 {
		t.Fatal("should not fire at 2 calls")
	}
	tr.RecordToolCall()
	v := tr.Check()
	if len(v) != 1 {
		t.Fatalf("expected block violation, got %d", len(v))
	}
	if v[0].SLO.Action != ActionBlock {
		t.Errorf("expected block action, got %q", v[0].SLO.Action)
	}
	msg := v[0].BlockMessage()
	if !strings.Contains(msg, "SLO block:") {
		t.Errorf("unexpected block message: %q", msg)
	}
}

func TestThrottleActionSetsFlag(t *testing.T) {
	tr := newTestTracker([]SLOEntry{
		{Name: "tokens", Metric: MetricContextTokens, Threshold: 10, Action: ActionThrottle},
	})
	tr.RecordTokens(10)
	v := tr.Check()
	if len(v) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(v))
	}
	if !tr.ConsumeThrottle() {
		t.Error("expected throttle flag to be set after throttle action")
	}
	// Consumed — second call returns false.
	if tr.ConsumeThrottle() {
		t.Error("throttle flag should be cleared after consume")
	}
}

func TestDebounce(t *testing.T) {
	tr := newTestTracker([]SLOEntry{
		{Name: "tokens", Metric: MetricContextTokens, Threshold: 10, Action: ActionWarn},
	})
	tr.RecordTokens(10)
	v1 := tr.Check()
	if len(v1) != 1 {
		t.Fatalf("first check: expected 1 violation, got %d", len(v1))
	}
	// Immediate second check — should be debounced.
	v2 := tr.Check()
	if len(v2) != 0 {
		t.Fatalf("second check (debounced): expected 0 violations, got %d", len(v2))
	}
}

func TestDebounceExpiry(t *testing.T) {
	tr := newTestTracker([]SLOEntry{
		{Name: "tokens", Metric: MetricContextTokens, Threshold: 10, Action: ActionWarn},
	})
	tr.RecordTokens(10)
	tr.Check() // prime debounce

	// Manually back-date the last-fired entry to simulate expiry.
	tr.mu.Lock()
	tr.lastFired["tokens"] = time.Now().Add(-debounceWindow - time.Second)
	tr.mu.Unlock()

	v := tr.Check()
	if len(v) != 1 {
		t.Fatalf("after debounce expiry: expected 1 violation, got %d", len(v))
	}
}

func TestPercentWarning(t *testing.T) {
	tr := newTestTracker([]SLOEntry{
		{
			Name:      "tokens",
			Metric:    MetricContextTokens,
			Threshold: 1000,
			Percent:   80,
			Action:    ActionWarn,
		},
	})
	// 800 = 80% of 1000 — should fire percent warning.
	tr.RecordTokens(800)
	v := tr.Check()
	if len(v) != 1 {
		t.Fatalf("expected percent warning, got %d violations", len(v))
	}
	if !v[0].Warning {
		t.Error("expected Warning=true for percent-based trigger")
	}
	ann := v[0].Annotation()
	if !strings.Contains(ann, "80%") {
		t.Errorf("annotation should mention percent: %q", ann)
	}

	// Hard limit not yet crossed.
	tr.RecordTokens(100) // total 900, still < 1000
	// Debounce will suppress the percent warning here, but hard limit not crossed.
	v2 := tr.Check()
	if len(v2) != 0 {
		t.Logf("unexpected violation: %+v", v2)
	}

	// Cross hard limit.
	tr.mu.Lock()
	tr.lastFired = make(map[string]time.Time) // clear debounce
	tr.mu.Unlock()
	tr.RecordTokens(200) // total 1100 >= 1000
	v3 := tr.Check()
	if len(v3) == 0 {
		t.Fatal("expected hard violation at 1100 tokens")
	}
	if v3[0].Warning {
		t.Error("expected hard violation (Warning=false)")
	}
}

func TestShellCallMetric(t *testing.T) {
	tr := newTestTracker([]SLOEntry{
		{Name: "shells", Metric: MetricShellCalls, Threshold: 2, Action: ActionWarn},
	})
	tr.RecordShellCall()
	if v := tr.Check(); len(v) != 0 {
		t.Fatal("should not fire at 1 shell call")
	}
	tr.RecordShellCall()
	v := tr.Check()
	if len(v) != 1 {
		t.Fatalf("expected 1 violation at 2 shell calls, got %d", len(v))
	}
}

func TestEmptyConfig(t *testing.T) {
	tr := newTestTracker(nil)
	tr.RecordTokens(999999)
	tr.RecordToolCall()
	tr.RecordToolCall()
	if v := tr.Check(); len(v) != 0 {
		t.Fatalf("empty config should never fire, got %v", v)
	}
}

func TestLoadConfig_Missing(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(cfg.SLOs) != 0 {
		t.Errorf("expected empty SLOs, got %v", cfg.SLOs)
	}
}
