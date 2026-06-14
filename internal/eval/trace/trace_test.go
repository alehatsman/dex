package trace

import (
	"math"
	"testing"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// buildView constructs a tiny in-memory graph with these `calls` edges:
//
//	A → B, A → C, D → B
//
// so callees(A) = {B, C}, callers(B) = {A, D}, callers(C) = {A}.
// Nodes are indexed by bare Name (the shape ResolveCallTargets matches first).
func buildView() *graphquery.View {
	mk := func(id, name, qn string) graphquery.Node {
		return graphquery.Node{ID: id, Kind: graph.NodeFunction, Name: name, QualifiedName: qn}
	}
	nodes := []graphquery.Node{
		mk("a", "A", "pkg.A"),
		mk("b", "B", "pkg.B"),
		mk("c", "C", "pkg.C"),
		mk("d", "D", "pkg.D"),
	}
	v := &graphquery.View{
		NodesByID:        map[string]graphquery.Node{},
		NodesByName:      map[string][]graphquery.Node{},
		NodesByQualified: map[string][]graphquery.Node{},
		EdgesBySrc:       map[string][]graphquery.Edge{},
		EdgesByDst:       map[string][]graphquery.Edge{},
	}
	for _, n := range nodes {
		v.NodesByID[n.ID] = n
		v.NodesByName[n.Name] = append(v.NodesByName[n.Name], n)
		v.NodesByQualified[n.QualifiedName] = append(v.NodesByQualified[n.QualifiedName], n)
	}
	edge := func(src, dst string) {
		e := graphquery.Edge{Kind: graph.EdgeCalls, SrcID: src, DstID: dst}
		v.EdgesBySrc[src] = append(v.EdgesBySrc[src], e)
		v.EdgesByDst[dst] = append(v.EdgesByDst[dst], e)
	}
	edge("a", "b")
	edge("a", "c")
	edge("d", "b")
	return v
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestScoreProbeCases(t *testing.T) {
	view := buildView()

	cases := []struct {
		name         string
		probe        Probe
		wantP, wantR float64
		wantResolved bool
	}{
		{
			name:         "perfect callees",
			probe:        Probe{Symbol: "A", Direction: DirectionCallees, Expected: []string{"pkg.B", "pkg.C"}},
			wantP:        1.0,
			wantR:        1.0,
			wantResolved: true,
		},
		{
			name:         "over-returns callers (low precision)",
			probe:        Probe{Symbol: "B", Direction: DirectionCallers, Expected: []string{"pkg.A"}},
			wantP:        0.5, // got {A, D}, only A expected
			wantR:        1.0,
			wantResolved: true,
		},
		{
			name:         "under-covers callers (low recall)",
			probe:        Probe{Symbol: "C", Direction: DirectionCallers, Expected: []string{"pkg.A", "pkg.X"}},
			wantP:        1.0, // got {A}, both correct of what's returned
			wantR:        0.5, // X never found
			wantResolved: true,
		},
		{
			name:         "unresolved symbol scores zero",
			probe:        Probe{Symbol: "ZZZ", Direction: DirectionCallers, Expected: []string{"pkg.A"}},
			wantP:        0.0,
			wantR:        0.0,
			wantResolved: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := scoreProbe(view, tc.probe)
			if res.Resolved != tc.wantResolved {
				t.Errorf("resolved = %v, want %v", res.Resolved, tc.wantResolved)
			}
			if !approx(res.Precision, tc.wantP) {
				t.Errorf("precision = %v, want %v", res.Precision, tc.wantP)
			}
			if !approx(res.Recall, tc.wantR) {
				t.Errorf("recall = %v, want %v", res.Recall, tc.wantR)
			}
		})
	}
}

func TestScoreAggregates(t *testing.T) {
	view := buildView()
	gold := Gold{
		Repo: "synthetic",
		Lang: "go",
		Probes: []Probe{
			{Symbol: "A", Direction: DirectionCallees, Expected: []string{"pkg.B", "pkg.C"}}, // P=1 R=1
			{Symbol: "B", Direction: DirectionCallers, Expected: []string{"pkg.A"}},          // P=.5 R=1
			{Symbol: "ZZZ", Direction: DirectionCallers, Expected: []string{"pkg.A"}},        // P=0 R=0, unresolved
		},
	}
	rep := Score(view, gold)

	if rep.Probes != 3 {
		t.Fatalf("probes = %d, want 3", rep.Probes)
	}
	if rep.Unresolved != 1 {
		t.Errorf("unresolved = %d, want 1", rep.Unresolved)
	}
	// macro precision = (1 + .5 + 0) / 3
	if want := (1.0 + 0.5 + 0.0) / 3; !approx(rep.MacroPrecision, want) {
		t.Errorf("macro precision = %v, want %v", rep.MacroPrecision, want)
	}
	// macro recall = (1 + 1 + 0) / 3
	if want := (1.0 + 1.0 + 0.0) / 3; !approx(rep.MacroRecall, want) {
		t.Errorf("macro recall = %v, want %v", rep.MacroRecall, want)
	}
}

func TestScoreEmptyGold(t *testing.T) {
	rep := Score(buildView(), Gold{Repo: "empty"})
	if rep.Probes != 0 || rep.MacroF1 != 0 {
		t.Errorf("empty gold: got probes=%d f1=%v, want 0/0", rep.Probes, rep.MacroF1)
	}
}
