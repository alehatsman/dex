package benchnav

import "testing"

// fixedRouting is a deterministic RoutingModel: larger budgets route a superset of
// paths, so routing accuracy is monotonic non-decreasing across the sweep.
func fixedRouting() RoutingModel {
	sets := map[int]map[string]bool{
		75:  {"a.go": true},
		150: {"a.go": true, "b.go": true},
		300: {"a.go": true, "b.go": true, "c.go": true},
	}
	return RoutingModel{Routable: func(path string, budget int) bool {
		return sets[budget][path]
	}}
}

func routingQueries() []Query {
	return []Query{
		{Query: "q1", Relevant: []string{"a.go"}},         // routed at every budget
		{Query: "q2", Relevant: []string{"c.go"}},         // routed only at 300
		{Query: "q3", Relevant: []string{"z.go"}},         // never routed
		{Query: "q4", Relevant: nil},                      // skipped (no gold)
		{Query: "q5", Relevant: []string{"z.go", "b.go"}}, // routed once any gold routes (b.go at >=150)
	}
}

func TestComputeRouting_AccuracyPerBudget(t *testing.T) {
	curve := ComputeRouting(routingQueries(), fixedRouting(), []int{75, 150, 300}, "full")
	if curve.Lane != "full" {
		t.Fatalf("lane = %q, want full", curve.Lane)
	}
	if len(curve.Points) != 3 {
		t.Fatalf("points = %d, want 3", len(curve.Points))
	}
	// 4 queries have gold (q4 skipped).
	for _, p := range curve.Points {
		if p.Queries != 4 {
			t.Errorf("budget %d: queries = %d, want 4", p.Budget, p.Queries)
		}
	}
	// budget 75: only q1 (a.go) routes.
	if got := curve.Points[0]; got.Routed != 1 || got.Accuracy != 0.25 {
		t.Errorf("budget 75: routed=%d acc=%.3f, want 1 / 0.250", got.Routed, got.Accuracy)
	}
	// budget 150: q1 (a.go) + q5 (b.go) route.
	if got := curve.Points[1]; got.Routed != 2 || got.Accuracy != 0.5 {
		t.Errorf("budget 150: routed=%d acc=%.3f, want 2 / 0.500", got.Routed, got.Accuracy)
	}
	// budget 300: q1, q2 (c.go), q5 route.
	if got := curve.Points[2]; got.Routed != 3 || got.Accuracy != 0.75 {
		t.Errorf("budget 300: routed=%d acc=%.3f, want 3 / 0.750", got.Routed, got.Accuracy)
	}
}

func TestComputeRouting_Monotonic(t *testing.T) {
	curve := ComputeRouting(routingQueries(), fixedRouting(), []int{75, 150, 300}, "full")
	for i := 1; i < len(curve.Points); i++ {
		if curve.Points[i].Accuracy < curve.Points[i-1].Accuracy {
			t.Errorf("accuracy dropped from budget %d (%.3f) to %d (%.3f) — must be monotonic",
				curve.Points[i-1].Budget, curve.Points[i-1].Accuracy,
				curve.Points[i].Budget, curve.Points[i].Accuracy)
		}
	}
}

func TestRoutingCurve_At(t *testing.T) {
	curve := ComputeRouting(routingQueries(), fixedRouting(), []int{75, 150}, "full")
	if acc, ok := curve.At(150); !ok || acc != 0.5 {
		t.Errorf("At(150) = %.3f, %v; want 0.500, true", acc, ok)
	}
	if _, ok := curve.At(999); ok {
		t.Errorf("At(999) should be absent")
	}
}

func TestRoutingCurve_Regressions_FloorTrips(t *testing.T) {
	ref := RoutingCurve{Lane: "full", Points: []RoutingPoint{
		{Budget: 75, Accuracy: 0.30},
		{Budget: 150, Accuracy: 0.50},
	}}
	// 150 drops 5 points (> 0.02 tol) → trips; 75 holds → silent.
	now := RoutingCurve{Lane: "full", Points: []RoutingPoint{
		{Budget: 75, Accuracy: 0.30},
		{Budget: 150, Accuracy: 0.45},
	}}
	regs := now.Regressions(ref, 0.02)
	if len(regs) != 1 {
		t.Fatalf("regressions = %d, want 1 (%+v)", len(regs), regs)
	}
	if regs[0].Metric != "routing_accuracy@150" {
		t.Errorf("metric = %q, want routing_accuracy@150", regs[0].Metric)
	}
}

func TestRoutingCurve_Regressions_WithinTolSilent(t *testing.T) {
	ref := RoutingCurve{Points: []RoutingPoint{{Budget: 150, Accuracy: 0.50}}}
	now := RoutingCurve{Points: []RoutingPoint{{Budget: 150, Accuracy: 0.49}}} // -1pt, within 0.02
	if regs := now.Regressions(ref, 0.02); len(regs) != 0 {
		t.Errorf("a 1-point drop within tol should be silent, got %+v", regs)
	}
}

func TestRoutingCurve_Regressions_GainSilent(t *testing.T) {
	ref := RoutingCurve{Points: []RoutingPoint{{Budget: 150, Accuracy: 0.50}}}
	now := RoutingCurve{Points: []RoutingPoint{{Budget: 150, Accuracy: 0.62}}} // a win
	if regs := now.Regressions(ref, 0.02); len(regs) != 0 {
		t.Errorf("a gain must never trip the floor, got %+v", regs)
	}
}
