package mcp

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestShadowReorderPrefersCrossLaneAgreementOnMiss(t *testing.T) {
	// A single-lane hit ranked first, a two-lane hit second. Under a total miss
	// with high confidence, the lane-agreement reweight should lift the
	// two-lane hit above the single-lane one.
	hits := []SemHit{
		{Path: "single.go", Score: 1.00, Lanes: []string{"vector"}},
		{Path: "agree.go", Score: 0.95, Lanes: []string{"vector", "bm25"}},
	}
	got := shadowReorder(hits, 0.0 /*open_rate*/, 1000 /*n*/)
	if got[0].Path != "agree.go" {
		t.Errorf("expected cross-lane hit promoted, got order %s,%s", got[0].Path, got[1].Path)
	}
	// Input must not be mutated.
	if hits[0].Path != "single.go" {
		t.Error("shadowReorder mutated its input")
	}
}

func TestShadowReorderNoSignalIsIdentity(t *testing.T) {
	hits := []SemHit{
		{Path: "a.go", Score: 1.0, Lanes: []string{"vector", "bm25"}},
		{Path: "b.go", Score: 0.9, Lanes: []string{"vector"}},
	}
	got := shadowReorder(hits, 0.0, 0) // n=0 → multiplier 1.0 everywhere
	if got[0].Path != "a.go" || got[1].Path != "b.go" {
		t.Errorf("no-signal reorder changed order: %s,%s", got[0].Path, got[1].Path)
	}
}

func TestRecordShadowOffByDefault(t *testing.T) {
	t.Setenv("DEX_FEEDBACK_SHADOW", "")
	t.Setenv("DEX_FEEDBACK_LIVE", "")
	s := &Server{}
	// Must not panic, must not write anything — shadow disabled.
	s.recordShadow("behavior_search", "where is X", []SemHit{
		{Path: "a.go", Score: 1, Lanes: []string{"vector"}},
		{Path: "b.go", Score: 0.5, Lanes: []string{"bm25"}},
	})
	if th, lg := s.feedbackThrottle(); th != nil || lg != nil {
		t.Error("feedbackThrottle should be nil when shadow mode is off")
	}
}

func TestRecordShadowWritesWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("DEX_FEEDBACK_SHADOW", "1")

	// Seed an observe log so the reader has a signal for the intent.
	logDir := filepath.Join(dir, "dex")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	obs := `{"tool_name":"mcp__dex__ask","intent":"behavior_search","paths":["x.go"]}
{"tool_name":"Read","paths":["y.go"]}
`
	if err := os.WriteFile(filepath.Join(logDir, "hooks.jsonl"), []byte(obs), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	s.recordShadow("behavior_search", "how does indexing work", []SemHit{
		{Path: "a.go", Score: 1.0, Lanes: []string{"vector"}},
		{Path: "b.go", Score: 0.9, Lanes: []string{"vector", "bm25"}},
	})

	shadowPath := filepath.Join(logDir, "feedback_shadow.jsonl")
	f, err := os.Open(shadowPath)
	if err != nil {
		t.Fatalf("shadow log not written: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatal("shadow log empty")
	}
	var rec shadowRecord
	if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
		t.Fatalf("bad shadow record: %v", err)
	}
	if rec.Intent != "behavior_search" {
		t.Errorf("intent = %q, want behavior_search", rec.Intent)
	}
	if rec.N != 1 || rec.OpenRate != 0 {
		t.Errorf("signal = (open_rate %v, n %d), want (0, 1)", rec.OpenRate, rec.N)
	}
	if len(rec.ServedTopK) != 2 {
		t.Errorf("served_topk = %v, want 2 entries", rec.ServedTopK)
	}
	if rec.Query != "how does indexing work" {
		t.Errorf("query = %q, want the question (the #743 join key)", rec.Query)
	}
}

func TestApplyLiveReweightIsIdentityWhenOff(t *testing.T) {
	t.Setenv("DEX_FEEDBACK_LIVE", "")
	s := &Server{}
	hits := []SemHit{
		{Path: "a.go", Score: 1.0, Lanes: []string{"vector", "bm25"}},
		{Path: "b.go", Score: 0.9, Lanes: []string{"vector"}},
	}
	got := s.applyLiveReweight("behavior_search", hits)
	if got[0].Path != "a.go" || got[1].Path != "b.go" {
		t.Errorf("live-off should preserve order, got %s,%s", got[0].Path, got[1].Path)
	}
}

func TestApplyLiveReweightReordersWithSignal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("DEX_FEEDBACK_LIVE", "1")
	t.Setenv("DEX_FEEDBACK_SHADOW", "") // live-only: throttle built, no shadow log

	logDir := filepath.Join(dir, "dex")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 100 ask events with no opens = total miss at high n → strong reweight signal.
	f, err := os.Create(filepath.Join(logDir, "hooks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(`{"tool_name":"mcp__dex__ask","intent":"behavior_search","paths":["x.go"]}` + "\n")
	for i := 0; i < 100; i++ {
		if _, err := f.Write(line); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	f.Close()

	s := &Server{}
	hits := []SemHit{
		{Path: "single.go", Score: 1.00, Lanes: []string{"vector"}},
		{Path: "agree.go", Score: 0.95, Lanes: []string{"vector", "bm25"}},
	}
	got := s.applyLiveReweight("behavior_search", hits)
	// Total miss at high n → cross-lane hit promoted above single-lane.
	if got[0].Path != "agree.go" {
		t.Errorf("live reweight should promote cross-lane hit, got %s first", got[0].Path)
	}
}

func TestTopPathsDedupsDistinctFiles(t *testing.T) {
	// Several chunk hits of the same file must collapse to one top-k slot (#743).
	hits := []SemHit{
		{Path: "a.go"}, {Path: "a.go"}, {Path: "b.go"}, {Path: "a.go"}, {Path: "c.go"},
	}
	got := topPaths(hits, 5)
	want := []string{"a.go", "b.go", "c.go"}
	if len(got) != len(want) {
		t.Fatalf("topPaths = %v, want distinct %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("topPaths = %v, want %v (first occurrence order)", got, want)
		}
	}
}

func TestJaccardStaysInUnitRange(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want float64
	}{
		{"identical", []string{"a", "b"}, []string{"b", "a"}, 1.0},
		{"disjoint", []string{"a"}, []string{"b"}, 0.0},
		{"half", []string{"a", "b"}, []string{"a", "c"}, 1.0 / 3.0},
		{"both empty", nil, nil, 1.0},
		// Duplicate-laden lists must never exceed 1.0 (the old bug returned 5.0).
		{"dups", []string{"a", "a", "a"}, []string{"a", "a", "a"}, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jaccard(tc.a, tc.b)
			if got < 0 || got > 1 {
				t.Fatalf("jaccard out of [0,1]: %v", got)
			}
			if got != tc.want {
				t.Fatalf("jaccard(%v,%v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
