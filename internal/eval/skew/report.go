package skew

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Cell is one indexed repo's skew Report under a repo label. A skew run over
// the corpus produces one Cell per repo; --project mode produces a single Cell.
type Cell struct {
	Repo   string `json:"repo"`
	Report Report `json:"report"`
}

// Suite is the committed/printed collection of skew cells. Cells are sorted by
// repo name for stable output and diffable baselines.
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
		return Suite{}, fmt.Errorf("parse skew baseline: %w", err)
	}
	return s, nil
}

// Markdown renders a per-repo table: language rows with node share, PageRank
// share, and the skew ratio. skew_ratio > 1 marks a language holding more
// centrality mass than its node count warrants (the gate-2 distortion).
func (s Suite) Markdown() string {
	var b strings.Builder
	for _, c := range s.Cells {
		r := c.Report
		fmt.Fprintf(&b, "### %s — %d call-graph nodes, %d communities\n\n", c.Repo, r.TotalNodes, r.TotalCommunities)
		b.WriteString("| language | nodes | node% | pagerank% | skew | communities |\n")
		b.WriteString("|----------|------:|------:|----------:|-----:|------------:|\n")
		for _, l := range r.Languages {
			fmt.Fprintf(&b, "| %s | %d | %.1f%% | %.1f%% | %.2f | %d |\n",
				l.Language, l.NodeCount, l.NodeShare*100, l.PageRankShare*100, l.SkewRatio, l.Communities)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Drift is a per-(repo, language) skew_ratio change beyond tolerance — the
// regression signal a future resolver change (e.g. receiver inference, SCIP)
// would move. Languages added or dropped between baseline and current also
// register as drift.
type Drift struct {
	Repo     string  `json:"repo"`
	Language string  `json:"language"`
	Old      float64 `json:"old"`
	New      float64 `json:"new"`
}

func (d Drift) String() string {
	return fmt.Sprintf("%s/%s skew %.2f → %.2f", d.Repo, d.Language, d.Old, d.New)
}

// Drift reports per-language skew_ratio movements vs ref beyond tol (absolute).
// A language present in one suite but not the other is reported with the
// missing side as 0. Cells present only in the current suite are ignored
// (new coverage is not a regression); cells dropped from current ARE reported.
func (s Suite) Drift(ref Suite, tol float64) []Drift {
	cur := skewIndex(s)
	base := skewIndex(ref)
	var out []Drift
	for key, oldVal := range base {
		newVal, ok := cur[key]
		if !ok {
			out = append(out, Drift{Repo: key.repo, Language: key.lang, Old: oldVal, New: 0})
			continue
		}
		if abs(newVal-oldVal) > tol {
			out = append(out, Drift{Repo: key.repo, Language: key.lang, Old: oldVal, New: newVal})
		}
	}
	// New languages that appeared in a baseline-known repo register as drift
	// from 0 — they shift the cell's distribution.
	baseRepos := map[string]bool{}
	for key := range base {
		baseRepos[key.repo] = true
	}
	for key, newVal := range cur {
		if _, ok := base[key]; ok {
			continue
		}
		if baseRepos[key.repo] && abs(newVal) > tol {
			out = append(out, Drift{Repo: key.repo, Language: key.lang, Old: 0, New: newVal})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Language < out[j].Language
	})
	return out
}

type skewKey struct{ repo, lang string }

func skewIndex(s Suite) map[skewKey]float64 {
	m := map[skewKey]float64{}
	for _, c := range s.Cells {
		for _, l := range c.Report.Languages {
			m[skewKey{c.Repo, l.Language}] = l.SkewRatio
		}
	}
	return m
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
