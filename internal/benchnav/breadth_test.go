package benchnav

import "testing"

// fixedBreadthCost prices the no-map find envelope as one token per ranked path.
// Read is unused by the breadth (discovery) metric and returns 0.
func fixedBreadthCost() CostModel {
	return CostModel{
		Read:         func(string) int { return 0 },
		FindEnvelope: func(ranked []string) int { return len(ranked) },
	}
}

// fixedBreadthModel: cluster 1 (l1=50) names a1,a2; cluster 2 (l1=70) names
// a3,b1. b2 is in no L0-shown cluster — a map miss.
func fixedBreadthModel() BreadthModel {
	type cl struct{ id, l1 int }
	m := map[string]cl{
		"a1": {1, 50}, "a2": {1, 50}, "a3": {2, 70}, "b1": {2, 70},
	}
	return BreadthModel{
		L0Tokens: 15,
		Cluster: func(p string) (int, int, bool) {
			c, ok := m[p]
			return c.id, c.l1, ok
		},
	}
}

// fixedAround: the exact around-render text per task. taskA's region names all
// three targets; taskB's names BOTH b1 and b2 — including the b2 the community
// lane misses, the case the exact lane is meant to win.
func fixedAround() AroundModel {
	text := map[string]struct {
		text   string
		tokens int
	}{
		"nbhd A": {"a1\na2\na3\n", 80},
		"nbhd B": {"b1\nb2\n", 60},
	}
	return AroundModel{
		Region: func(task string) (string, int, bool) {
			r, ok := text[task]
			return r.text, r.tokens, ok
		},
	}
}

func taskA() BreadthTask {
	return BreadthTask{Task: "nbhd A", Targets: []string{"a1", "a2", "a3"}, Ranked: []string{"a1", "x", "a2", "y", "a3"}}
}

func taskB() BreadthTask {
	return BreadthTask{Task: "nbhd B", Targets: []string{"b1", "b2"}, Ranked: []string{"b2", "b1"}}
}

func TestComputeBreadth_PerTask_FullHorizon(t *testing.T) {
	rep := ComputeBreadth([]BreadthTask{taskA()}, 10, fixedBreadthCost(), fixedBreadthModel(), AroundModel{}, "full")
	r := rep.Results[0]
	// no-map: all 3 visible in top-5; discovery cost = find envelope (5 paths)
	if r.NoMapFound != 3 || r.NoMapCoverage != 1.0 {
		t.Fatalf("no-map found/cov: got %d/%v want 3/1.0", r.NoMapFound, r.NoMapCoverage)
	}
	if r.NoMapTokens != 5 || r.NoMapCalls != 1 {
		t.Fatalf("no-map tokens/calls: got %d/%d want 5/1", r.NoMapTokens, r.NoMapCalls)
	}
	// map: 3 enumerated, clusters {1,2} zoomed once each; tokens 15+50+70=135
	if r.MapFound != 3 || r.MapCoverage != 1.0 {
		t.Fatalf("map found/cov: got %d/%v want 3/1.0", r.MapFound, r.MapCoverage)
	}
	if r.MapTokens != 135 || r.MapCalls != 3 { // L0 + 2 zooms
		t.Fatalf("map tokens/calls: got %d/%d want 135/3", r.MapTokens, r.MapCalls)
	}
}

func TestComputeBreadth_HorizonTruncatesNoMap(t *testing.T) {
	rep := ComputeBreadth([]BreadthTask{taskA()}, 3, fixedBreadthCost(), fixedBreadthModel(), AroundModel{}, "full")
	r := rep.Results[0]
	// depth=3 → scan [a1,x,a2]; sees a1,a2; a3 beyond horizon
	if r.NoMapFound != 2 {
		t.Fatalf("no-map found: got %d want 2", r.NoMapFound)
	}
	if r.NoMapCoverage <= 0.66 || r.NoMapCoverage >= 0.67 {
		t.Fatalf("no-map coverage: got %v want ~0.667", r.NoMapCoverage)
	}
	// map is unaffected by the read horizon — still enumerates all 3
	if r.MapFound != 3 {
		t.Fatalf("map found: got %d want 3", r.MapFound)
	}
}

func TestComputeBreadth_MapMissCountedHonestly(t *testing.T) {
	rep := ComputeBreadth([]BreadthTask{taskB()}, 10, fixedBreadthCost(), fixedBreadthModel(), AroundModel{}, "full")
	r := rep.Results[0]
	// b2 is in no shown cluster: map enumerates only b1
	if r.MapFound != 1 || r.MapCoverage != 0.5 {
		t.Fatalf("map found/cov: got %d/%v want 1/0.5", r.MapFound, r.MapCoverage)
	}
	if r.MapTokens != 85 || r.MapCalls != 2 { // L0 15 + one zoom 70
		t.Fatalf("map tokens/calls: got %d/%d want 85/2", r.MapTokens, r.MapCalls)
	}
	// no-map sees both in the ranking → full coverage
	if r.NoMapFound != 2 || r.NoMapCoverage != 1.0 {
		t.Fatalf("no-map found/cov: got %d/%v want 2/1.0", r.NoMapFound, r.NoMapCoverage)
	}
}

func TestComputeBreadth_SharedClusterZoomedOnce(t *testing.T) {
	// both targets live in cluster 1 → one zoom, not two
	tk := BreadthTask{Task: "shared", Targets: []string{"a1", "a2"}, Ranked: []string{"a1", "a2"}}
	rep := ComputeBreadth([]BreadthTask{tk}, 10, fixedBreadthCost(), fixedBreadthModel(), AroundModel{}, "full")
	r := rep.Results[0]
	if r.MapTokens != 65 { // 15 L0 + 50 (one zoom)
		t.Fatalf("map tokens: got %d want 65", r.MapTokens)
	}
	if r.MapCalls != 2 { // 1 L0 + 1 zoom
		t.Fatalf("map calls: got %d want 2", r.MapCalls)
	}
}

func TestComputeBreadth_Aggregate(t *testing.T) {
	rep := ComputeBreadth([]BreadthTask{taskA(), taskB()}, 10, fixedBreadthCost(), fixedBreadthModel(), AroundModel{}, "full")
	if rep.NumTasks != 2 {
		t.Fatalf("num tasks: got %d want 2", rep.NumTasks)
	}
	// map coverage mean = (1.0 + 0.5)/2 = 0.75; no-map = (1.0 + 1.0)/2 = 1.0
	if rep.MeanMapCoverage != 0.75 || rep.MeanNoMapCoverage != 1.0 {
		t.Fatalf("mean cov: map %v nomap %v want 0.75/1.0", rep.MeanMapCoverage, rep.MeanNoMapCoverage)
	}
	if rep.DeltaCoverage != rep.MeanMapCoverage-rep.MeanNoMapCoverage {
		t.Fatalf("delta coverage not consistent: %v", rep.DeltaCoverage)
	}
	// no around model → exact lane absent
	if rep.HasExact {
		t.Fatalf("exact lane must stay off without an AroundModel")
	}
}

func TestComputeBreadth_ExactLane_BeatsCommunityOnMissedNeighbor(t *testing.T) {
	rep := ComputeBreadth([]BreadthTask{taskA(), taskB()}, 10, fixedBreadthCost(), fixedBreadthModel(), fixedAround(), "full")
	if !rep.HasExact || rep.NumExact != 2 {
		t.Fatalf("exact lane should score both tasks: hasExact=%v num=%d", rep.HasExact, rep.NumExact)
	}
	// The around region IS the neighborhood: every target is surfaced → 100%.
	if rep.MeanExactCoverage != 1.0 {
		t.Fatalf("mean exact coverage: got %v want 1.0", rep.MeanExactCoverage)
	}
	// Per-task: taskB's exact lane enumerates both b1 AND b2 (community missed b2).
	var rb BreadthResult
	for _, r := range rep.Results {
		if r.Task == "nbhd B" {
			rb = r
		}
	}
	if !rb.HasExact || rb.ExactFound != 2 || rb.ExactCoverage != 1.0 {
		t.Fatalf("taskB exact: found=%d cov=%v want 2/1.0", rb.ExactFound, rb.ExactCoverage)
	}
	if rb.ExactCalls != 1 || rb.ExactTokens != 60 {
		t.Fatalf("taskB exact cost: calls=%d tokens=%d want 1/60", rb.ExactCalls, rb.ExactTokens)
	}
	// Headline: exact − community coverage = 1.0 − (1.0+0.5)/2 = +0.25.
	if d := rep.DeltaExactVsMapCoverage; d <= 0.24 || d >= 0.26 {
		t.Fatalf("exact-vs-map coverage advantage: got %v want ~0.25", d)
	}
	// exact tokens (80,60)=70 mean vs community (135,85)=110 mean → −40.
	if d := rep.DeltaExactVsMapTokens; d != 70-110 {
		t.Fatalf("exact-vs-map token delta: got %v want -40", d)
	}
}

func TestComputeBreadth_ExactTruncationHonest(t *testing.T) {
	// The around render omits a3 (budget truncation): coverage must drop to 2/3,
	// never silently report the full neighborhood.
	around := AroundModel{Region: func(task string) (string, int, bool) {
		return "a1\na2\n", 50, true // a3 missing
	}}
	rep := ComputeBreadth([]BreadthTask{taskA()}, 10, fixedBreadthCost(), fixedBreadthModel(), around, "full")
	r := rep.Results[0]
	if r.ExactFound != 2 {
		t.Fatalf("truncated exact found: got %d want 2", r.ExactFound)
	}
	if r.ExactCoverage <= 0.66 || r.ExactCoverage >= 0.67 {
		t.Fatalf("truncated exact coverage: got %v want ~0.667", r.ExactCoverage)
	}
}

func TestComputeBreadth_ExactSkippedWhenSeedHasNoEdges(t *testing.T) {
	// ok=false (no edges to render) → the task carries no exact result, and the
	// aggregate stays off when no task scored.
	around := AroundModel{Region: func(task string) (string, int, bool) { return "", 0, false }}
	rep := ComputeBreadth([]BreadthTask{taskA()}, 10, fixedBreadthCost(), fixedBreadthModel(), around, "full")
	if rep.Results[0].HasExact {
		t.Fatalf("edgeless seed must not produce an exact result")
	}
	if rep.HasExact {
		t.Fatalf("aggregate exact lane must stay off when no task scored")
	}
}

func TestBreadthReport_Regressions_FloorTrips(t *testing.T) {
	ref := BreadthReport{MeanMapCoverage: 0.90, DeltaCoverage: 0.30}
	now := BreadthReport{MeanMapCoverage: 0.85, DeltaCoverage: 0.30} // -0.05 > 0.02 tol
	regs := now.Regressions(ref, 0.02)
	if len(regs) != 1 || regs[0].Metric != "breadth_map_coverage" {
		t.Fatalf("expected coverage floor trip, got %v", regs)
	}
}

func TestBreadthReport_Regressions_AdvantageErosionTrips(t *testing.T) {
	ref := BreadthReport{MeanMapCoverage: 0.90, DeltaCoverage: 0.30}
	now := BreadthReport{MeanMapCoverage: 0.90, DeltaCoverage: 0.05} // advantage -0.25 > tol
	regs := now.Regressions(ref, 0.02)
	if len(regs) != 1 || regs[0].Metric != "breadth_coverage_advantage" {
		t.Fatalf("expected advantage erosion trip, got %v", regs)
	}
}

func TestBreadthReport_Regressions_GainSilent(t *testing.T) {
	ref := BreadthReport{MeanMapCoverage: 0.80, DeltaCoverage: 0.20}
	now := BreadthReport{MeanMapCoverage: 0.90, DeltaCoverage: 0.35} // both improved
	if regs := now.Regressions(ref, 0.02); len(regs) != 0 {
		t.Fatalf("gains must not trip: %v", regs)
	}
}

func TestBreadthReport_Regressions_ExactFloorAndAdvantageTrip(t *testing.T) {
	ref := BreadthReport{
		HasExact: true, MeanMapCoverage: 0.90, DeltaCoverage: 0.30,
		MeanExactCoverage: 0.95, DeltaExactVsMapCoverage: 0.20,
	}
	// exact coverage -0.10 and exact-vs-map advantage -0.15, both > tol.
	now := BreadthReport{
		HasExact: true, MeanMapCoverage: 0.90, DeltaCoverage: 0.30,
		MeanExactCoverage: 0.85, DeltaExactVsMapCoverage: 0.05,
	}
	regs := now.Regressions(ref, 0.02)
	var sawFloor, sawAdv bool
	for _, r := range regs {
		switch r.Metric {
		case "breadth_exact_coverage":
			sawFloor = true
		case "breadth_exact_vs_map_advantage":
			sawAdv = true
		}
	}
	if !sawFloor || !sawAdv {
		t.Fatalf("expected exact floor + advantage trips, got %v", regs)
	}
}

func TestBreadthReport_Regressions_ExactGatesSilentWithoutRef(t *testing.T) {
	// A reference without an exact lane must not gate the exact metrics even when
	// the current report has them — nothing to compare against.
	ref := BreadthReport{MeanMapCoverage: 0.90, DeltaCoverage: 0.30}
	now := BreadthReport{HasExact: true, MeanMapCoverage: 0.90, DeltaCoverage: 0.30, MeanExactCoverage: 0.10}
	if regs := now.Regressions(ref, 0.02); len(regs) != 0 {
		t.Fatalf("exact gates must stay silent without an exact reference: %v", regs)
	}
}
