package nav

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RoutingModel answers, for a gold file and an L0 token budget, whether a single
// map(budget) call routes an agent to that file's region — its package named
// within the L0-shown clusters. This is the L0-ONLY orientation signal: one call,
// no L1 zoom, no find(). It is the primary map-quality metric for the explore epic
// (#316 story 8, issue #351): after the #349 verdict (the map does not cut
// first-touch cost), orientation — not first-touch — is the win the map must earn.
type RoutingModel struct {
	// Routable reports whether path is reachable from the L0 overview rendered at
	// the given token budget. Implementations scan only what L0 shows (cluster
	// membership), never an L1 zoom, so the answer reflects one map() call.
	Routable func(path string, budget int) bool
}

// RoutingPoint is routing accuracy at one L0 budget.
type RoutingPoint struct {
	Budget   int     `json:"budget"`
	Queries  int     `json:"queries"`  // queries with at least one gold file
	Routed   int     `json:"routed"`   // queries with at least one gold routable at this budget
	Accuracy float64 `json:"accuracy"` // Routed / Queries
}

// RoutingCurve is routing accuracy swept over a set of L0 budgets — the lever
// stories #347 (task-conditioned map) and #348 (L0 injection) must move. Accuracy
// is monotonic non-decreasing in budget: a larger L0 shows a superset of clusters,
// so it can only add routable golds.
type RoutingCurve struct {
	Lane   string         `json:"lane"`
	Points []RoutingPoint `json:"points"`
}

// ComputeRouting measures routing accuracy at each budget. A query is routed if
// ANY of its gold files is routable at that budget (touching one gold counts, the
// same reach rule as Compute). Queries with no gold files are skipped.
func ComputeRouting(queries []Query, m RoutingModel, budgets []int, lane string) RoutingCurve {
	curve := RoutingCurve{Lane: lane}
	for _, budget := range budgets {
		pt := RoutingPoint{Budget: budget}
		for _, q := range queries {
			if len(q.Relevant) == 0 {
				continue
			}
			pt.Queries++
			for _, g := range q.Relevant {
				if m.Routable(g, budget) {
					pt.Routed++
					break
				}
			}
		}
		if pt.Queries > 0 {
			pt.Accuracy = float64(pt.Routed) / float64(pt.Queries)
		}
		curve.Points = append(curve.Points, pt)
	}
	return curve
}

// At returns routing accuracy at the given budget, or (0, false) if not swept.
func (rc RoutingCurve) At(budget int) (float64, bool) {
	for _, p := range rc.Points {
		if p.Budget == budget {
			return p.Accuracy, true
		}
	}
	return 0, false
}

// JSON renders the curve.
func (rc RoutingCurve) JSON() ([]byte, error) {
	return json.MarshalIndent(rc, "", "  ")
}

// Markdown renders the routing-accuracy table.
func (rc RoutingCurve) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## routing accuracy@budget (%s lane) — orientation, one map() call\n\n", rc.Lane)
	fmt.Fprintf(&b, "Fraction of queries whose gold file is reachable from the L0 overview at\n")
	fmt.Fprintf(&b, "budget B (no L1 zoom, no find). The map-quality number stories #347/#348 must raise.\n\n")
	fmt.Fprintf(&b, "| L0 budget | routing accuracy | routed |\n|---|---|---|\n")
	for _, p := range rc.Points {
		fmt.Fprintf(&b, "| %d | %.1f%% | %d/%d |\n", p.Budget, p.Accuracy*100, p.Routed, p.Queries)
	}
	return b.String()
}

// Regressions gates routing accuracy as a FLOOR at every shared budget: accuracy
// may not fall by more than absTol (absolute, e.g. 0.02 = 2 points) below the
// reference. Stories must raise this curve; the guardrail is that it never drops.
func (rc RoutingCurve) Regressions(ref RoutingCurve, absTol float64) []Regression {
	var regs []Regression
	for _, rp := range ref.Points {
		now, ok := rc.At(rp.Budget)
		if !ok {
			continue
		}
		if rp.Accuracy-now > absTol {
			regs = append(regs, Regression{
				Metric: fmt.Sprintf("routing_accuracy@%d", rp.Budget),
				Was:    rp.Accuracy,
				Now:    now,
			})
		}
	}
	return regs
}
