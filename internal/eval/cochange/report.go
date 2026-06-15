package cochange

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Cell is one repo's cochange Report under a repo label. A corpus run produces
// one Cell per blast-radius-enabled repo; --project mode produces a single Cell.
type Cell struct {
	Repo   string `json:"repo"`
	Report Report `json:"report"`
}

// Suite is the committed/printed collection of cochange cells, sorted by repo
// for stable, diffable baselines.
type Suite struct {
	Cells []Cell `json:"cells"`
}

// NewSuite sorts cells by repo for determinism.
func NewSuite(cells []Cell) Suite {
	sort.Slice(cells, func(i, j int) bool { return cells[i].Repo < cells[j].Repo })
	return Suite{Cells: cells}
}

// JSON renders the suite as indented JSON (the committed baseline shape).
func (s Suite) JSON() ([]byte, error) { return json.MarshalIndent(s, "", "  ") }

// LoadSuite parses a baseline JSON produced by JSON().
func LoadSuite(data []byte) (Suite, error) {
	var s Suite
	if err := json.Unmarshal(data, &s); err != nil {
		return Suite{}, fmt.Errorf("parse cochange baseline: %w", err)
	}
	return s, nil
}

// Markdown renders one row per repo: the gold-type split and the structural
// reachability of the src-only subset. A low two_hop% means the co-change
// coupling is non-structural — the graph lane cannot re-rank it (#555 ceiling).
func (s Suite) Markdown() string {
	var b strings.Builder
	b.WriteString("| repo | lang | queries | test-gold% | src-only | anchor-in-graph% | 1-hop% | 2-hop% |\n")
	b.WriteString("|------|------|--------:|-----------:|---------:|-----------------:|-------:|-------:|\n")
	for _, c := range s.Cells {
		r := c.Report
		fmt.Fprintf(&b, "| %s | %s | %d | %.0f%% | %d | %.0f%% | %.0f%% | %.0f%% |\n",
			c.Repo, r.Lang, r.Queries, r.TestGoldShare()*100, r.SrcOnly,
			r.AnchorResolveShare()*100, r.OneHopShare()*100, r.TwoHopShare()*100)
	}
	return b.String()
}

// Drift is a per-repo two-hop reachability movement vs the baseline beyond
// tolerance — the regression signal a future resolver change (receiver
// inference, SCIP) would move. Repos dropped from the current suite register
// as drift to 0; new repos are ignored (added coverage is not a regression).
type Drift struct {
	Repo string  `json:"repo"`
	Old  float64 `json:"old"`
	New  float64 `json:"new"`
}

func (d Drift) String() string {
	return fmt.Sprintf("%s two-hop %.2f → %.2f", d.Repo, d.Old, d.New)
}

// Drift reports per-repo two_hop-share movements vs ref beyond tol (absolute).
// Two-hop share is the headline ceiling number; gating it catches both
// regressions (extractor drops edges) and unexpected improvements (a resolver
// change that newly connects co-change pairs — which would reopen #555).
func (s Suite) Drift(ref Suite, tol float64) []Drift {
	cur := twoHopIndex(s)
	base := twoHopIndex(ref)
	var out []Drift
	for repo, oldVal := range base {
		newVal, ok := cur[repo]
		if !ok {
			out = append(out, Drift{Repo: repo, Old: oldVal, New: 0})
			continue
		}
		if abs(newVal-oldVal) > tol {
			out = append(out, Drift{Repo: repo, Old: oldVal, New: newVal})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}

func twoHopIndex(s Suite) map[string]float64 {
	m := map[string]float64{}
	for _, c := range s.Cells {
		m[c.Repo] = c.Report.TwoHopShare()
	}
	return m
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
