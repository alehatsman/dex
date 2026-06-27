package feedback

import (
	"os"
	"path/filepath"
	"testing"
)

// askAt is an ask event stamped with a timestamp, so a ShadowRecord can join to
// it. Paths here are the served suggested_reads (not used by the shadow join,
// which reads served/shadow top-k off the record).
func askAt(ts int64, intent string) Event {
	return Event{ToolName: "mcp__dex__ask", Intent: intent, TS: ts}
}

// shadowSession is an ask followed by the files the agent opened, the shape the
// join walks: match the record to the ask, then look ahead for opens.
func shadowSession(ts int64, intent string, opened ...string) []Event {
	evs := []Event{askAt(ts, intent)}
	for _, p := range opened {
		evs = append(evs, read(p))
	}
	return evs
}

func TestAnalyzeShadowWin(t *testing.T) {
	// Agent opened "c", which only the shadow top-k surfaced.
	events := shadowSession(100, "behavior_search", "c")
	rec := ShadowRecord{
		TS: 101, Intent: "behavior_search",
		ServedTopK: []string{"a", "b"},
		ShadowTopK: []string{"a", "c"},
	}
	r := AnalyzeShadow(events, []ShadowRecord{rec}, 0)

	if r.Matched != 1 {
		t.Fatalf("Matched = %d, want 1", r.Matched)
	}
	if r.Reordered != 1 {
		t.Fatalf("Reordered = %d, want 1 (sets differ)", r.Reordered)
	}
	if r.ShadowWins != 1 || r.ShadowLosses != 0 {
		t.Fatalf("wins/losses = %d/%d, want 1/0", r.ShadowWins, r.ShadowLosses)
	}
	if r.ShadowOpenRate <= r.ServedOpenRate {
		t.Fatalf("shadow open-rate %.3f should beat served %.3f", r.ShadowOpenRate, r.ServedOpenRate)
	}
	if r.Verdict != "win-candidate" {
		t.Fatalf("Verdict = %q, want win-candidate (%s)", r.Verdict, r.Note)
	}
}

func TestAnalyzeShadowLoss(t *testing.T) {
	// Agent opened "b", which only the served top-k surfaced — reweight hurt.
	events := shadowSession(100, "behavior_search", "b")
	rec := ShadowRecord{
		TS: 100, Intent: "behavior_search",
		ServedTopK: []string{"a", "b"},
		ShadowTopK: []string{"a", "c"},
	}
	r := AnalyzeShadow(events, []ShadowRecord{rec}, 0)

	if r.ShadowWins != 0 || r.ShadowLosses != 1 {
		t.Fatalf("wins/losses = %d/%d, want 0/1", r.ShadowWins, r.ShadowLosses)
	}
	if r.Verdict != "no-target" {
		t.Fatalf("Verdict = %q, want no-target (%s)", r.Verdict, r.Note)
	}
}

func TestAnalyzeShadowNoDivergence(t *testing.T) {
	// Identical top-k sets: open-rate can't differ; verdict is insufficient.
	events := shadowSession(100, "symbol_lookup", "a")
	rec := ShadowRecord{
		TS: 100, Intent: "symbol_lookup",
		ServedTopK: []string{"a", "b"},
		ShadowTopK: []string{"b", "a"}, // same set, reordered
	}
	r := AnalyzeShadow(events, []ShadowRecord{rec}, 0)

	if r.Matched != 1 {
		t.Fatalf("Matched = %d, want 1", r.Matched)
	}
	if r.Reordered != 0 {
		t.Fatalf("Reordered = %d, want 0 (same set)", r.Reordered)
	}
	if r.Verdict != "insufficient" {
		t.Fatalf("Verdict = %q, want insufficient (%s)", r.Verdict, r.Note)
	}
}

func TestAnalyzeShadowUnmatched(t *testing.T) {
	events := shadowSession(100, "behavior_search", "c")
	tests := []struct {
		name string
		rec  ShadowRecord
	}{
		{"ts too far", ShadowRecord{TS: 1000, Intent: "behavior_search", ServedTopK: []string{"a"}, ShadowTopK: []string{"c"}}},
		{"intent mismatch", ShadowRecord{TS: 100, Intent: "architecture", ServedTopK: []string{"a"}, ShadowTopK: []string{"c"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := AnalyzeShadow(events, []ShadowRecord{tt.rec}, 0)
			if r.Matched != 0 {
				t.Fatalf("Matched = %d, want 0", r.Matched)
			}
			if r.Verdict != "insufficient" {
				t.Fatalf("Verdict = %q, want insufficient", r.Verdict)
			}
		})
	}
}

// TestAnalyzeShadowGreedyMatch checks that two asks of the same intent in one
// session each claim their own nearest record rather than double-counting one.
func TestAnalyzeShadowGreedyMatch(t *testing.T) {
	events := []Event{
		askAt(100, "behavior_search"), read("c"),
		askAt(200, "behavior_search"), read("d"),
	}
	recs := []ShadowRecord{
		{TS: 100, Intent: "behavior_search", ServedTopK: []string{"a"}, ShadowTopK: []string{"c"}},
		{TS: 201, Intent: "behavior_search", ServedTopK: []string{"a"}, ShadowTopK: []string{"d"}},
	}
	r := AnalyzeShadow(events, recs, 0)
	if r.Matched != 2 {
		t.Fatalf("Matched = %d, want 2 (each record claims its own ask)", r.Matched)
	}
}

func TestReadShadowLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feedback_shadow.jsonl")
	content := `{"ts":100,"intent":"behavior_search","served_topk":["a"],"shadow_topk":["b"]}
not json — partial final line tolerated
{"ts":200,"intent":"symbol_lookup","served_topk":["c"],"shadow_topk":["c"]}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadShadowLog(path)
	if err != nil {
		t.Fatalf("ReadShadowLog: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (malformed line skipped)", len(recs))
	}
	if recs[0].Intent != "behavior_search" || recs[1].ShadowTopK[0] != "c" {
		t.Fatalf("parsed records wrong: %+v", recs)
	}
}

func TestReadShadowLogMissing(t *testing.T) {
	if _, err := ReadShadowLog(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Fatal("expected error for missing log")
	}
}
