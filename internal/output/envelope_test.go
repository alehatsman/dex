package output

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAgeStale(t *testing.T) {
	t.Run("zero time is unknown", func(t *testing.T) {
		s := AgeStale(time.Time{})
		if s.Coverage != CoverageUnknown || s.IsStale {
			t.Fatalf("zero time: got %+v, want unknown/not-stale", s)
		}
		if s.LastIndexedAt != "" {
			t.Fatalf("zero time should carry no timestamp, got %q", s.LastIndexedAt)
		}
	})
	t.Run("fresh index is age_only and not stale", func(t *testing.T) {
		s := AgeStale(time.Now().Add(-time.Hour))
		if s.Coverage != CoverageAgeOnly || s.IsStale {
			t.Fatalf("fresh: got %+v, want age_only/not-stale", s)
		}
		if s.LastIndexedAt == "" {
			t.Fatal("fresh index should carry a last_indexed_at timestamp")
		}
	})
	t.Run("old index trips is_stale", func(t *testing.T) {
		s := AgeStale(time.Now().Add(-StaleAgeThreshold - time.Hour))
		if s.Coverage != CoverageAgeOnly || !s.IsStale {
			t.Fatalf("old: got %+v, want age_only/stale", s)
		}
	})
}

func TestNormalize(t *testing.T) {
	e := Envelope{Confidence: Confidence{Level: LevelHigh}}
	e.NextCalls = []NextCall{
		{Tool: "a", Reason: "r"}, {Tool: "b", Reason: "r"},
		{Tool: "c", Reason: "r"}, {Tool: "d", Reason: "r"},
	}
	e.Normalize()
	if e.Evidence == nil {
		t.Fatal("Normalize must replace nil Evidence with [] so JSON emits an array")
	}
	if e.Stale.Coverage != CoverageUnknown {
		t.Fatalf("Normalize must default Coverage to unknown, got %q", e.Stale.Coverage)
	}
	if len(e.NextCalls) != MaxNextCalls {
		t.Fatalf("Normalize must cap next_calls at %d, got %d", MaxNextCalls, len(e.NextCalls))
	}
	// nil Evidence must marshal as [] not null.
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"evidence":[]`) {
		t.Fatalf("empty evidence must render as []: %s", b)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		env  Envelope
		ok   bool
	}{
		{"valid minimal", Envelope{Confidence: Confidence{Level: LevelHigh}, Stale: StaleStatus{Coverage: CoverageAgeOnly}}, true},
		{"empty evidence allowed", Envelope{Confidence: Confidence{Level: LevelLow, Gaps: []string{"no span"}}, Stale: StaleStatus{Coverage: CoverageUnknown}, Evidence: []EvidenceSpan{}}, true},
		{"bad level", Envelope{Confidence: Confidence{Level: "great"}, Stale: StaleStatus{Coverage: CoverageAgeOnly}}, false},
		{"bad coverage", Envelope{Confidence: Confidence{Level: LevelHigh}, Stale: StaleStatus{Coverage: "sorta"}}, false},
		{"span missing path", Envelope{Confidence: Confidence{Level: LevelHigh}, Stale: StaleStatus{Coverage: CoverageAgeOnly}, Evidence: []EvidenceSpan{{Start: 1, End: 2}}}, false},
		{"span inverted range", Envelope{Confidence: Confidence{Level: LevelHigh}, Stale: StaleStatus{Coverage: CoverageAgeOnly}, Evidence: []EvidenceSpan{{Path: "x.go", Start: 9, End: 2}}}, false},
		{"too many next_calls", Envelope{Confidence: Confidence{Level: LevelHigh}, Stale: StaleStatus{Coverage: CoverageAgeOnly}, NextCalls: []NextCall{{Tool: "a", Reason: "r"}, {Tool: "b", Reason: "r"}, {Tool: "c", Reason: "r"}, {Tool: "d", Reason: "r"}}}, false},
		{"next_call missing reason", Envelope{Confidence: Confidence{Level: LevelHigh}, Stale: StaleStatus{Coverage: CoverageAgeOnly}, NextCalls: []NextCall{{Tool: "a"}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.env.Validate()
			if tc.ok && err != nil {
				t.Fatalf("want valid, got error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}
