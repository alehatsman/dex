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

func taskA() BreadthTask {
	return BreadthTask{Task: "nbhd A", Targets: []string{"a1", "a2", "a3"}, Ranked: []string{"a1", "x", "a2", "y", "a3"}}
}

func taskB() BreadthTask {
	return BreadthTask{Task: "nbhd B", Targets: []string{"b1", "b2"}, Ranked: []string{"b2", "b1"}}
}

func TestComputeBreadth_PerTask_FullHorizon(t *testing.T) {
	rep := ComputeBreadth([]BreadthTask{taskA()}, 10, fixedBreadthCost(), fixedBreadthModel(), "full")
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
	rep := ComputeBreadth([]BreadthTask{taskA()}, 3, fixedBreadthCost(), fixedBreadthModel(), "full")
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
	rep := ComputeBreadth([]BreadthTask{taskB()}, 10, fixedBreadthCost(), fixedBreadthModel(), "full")
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
	rep := ComputeBreadth([]BreadthTask{tk}, 10, fixedBreadthCost(), fixedBreadthModel(), "full")
	r := rep.Results[0]
	if r.MapTokens != 65 { // 15 L0 + 50 (one zoom)
		t.Fatalf("map tokens: got %d want 65", r.MapTokens)
	}
	if r.MapCalls != 2 { // 1 L0 + 1 zoom
		t.Fatalf("map calls: got %d want 2", r.MapCalls)
	}
}

func TestComputeBreadth_Aggregate(t *testing.T) {
	rep := ComputeBreadth([]BreadthTask{taskA(), taskB()}, 10, fixedBreadthCost(), fixedBreadthModel(), "full")
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
