// Package benchnav implements the navigation benchmark lane (epic #316,
// story 7): the RULER that gates the explore stories. It measures how much
// work an agent spends to first touch a gold file, under a deterministic,
// zero-inference navigation policy.
//
// Phase A (this file) measures the NO-MAP baseline: the agent issues one
// find() call, then reads the ranked files top-down until it touches a
// relevant (gold) file. The metric is the count of tool calls and the tokens
// consumed up to that first touch. Phase B (gated on `dex map`, story 1) will
// re-measure with a map() call seeding navigation and report the delta.
//
// The package is intentionally decoupled from the retrieval stack: callers
// adapt their ranked results into Query values and supply a CostModel, so the
// policy is unit-testable with no index, embedder, or GPU.
package benchnav

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Query is one labeled navigation example: the ranked file list a find()
// returned (rank order, deduped) and the set of relevant (gold) files.
type Query struct {
	Query    string   `json:"query"`
	Ranked   []string `json:"ranked"`   // repo-relative paths, best-first
	Relevant []string `json:"relevant"` // gold files; touching any one counts as reached
}

// CostModel prices the two actions the policy takes. Read returns the tokens
// an agent spends reading one file; FindEnvelope returns the tokens of the
// find() result envelope the agent reads before deciding what to open. Both
// are injected so the policy stays free of any tokenizer or disk dependency.
type CostModel struct {
	Read         func(path string) int
	FindEnvelope func(ranked []string) int
}

// NavResult is the per-query outcome under the no-map top-down read policy.
type NavResult struct {
	Query         string `json:"query"`
	Reached       bool   `json:"reached"`         // a gold file fell within the ranked top-k
	FirstGoldRank int    `json:"first_gold_rank"` // 1-indexed; 0 when not reached
	Calls         int    `json:"calls"`           // find + reads to touch (or to exhaust top-k on a miss)
	Tokens        int    `json:"tokens"`          // find-envelope + read tokens consumed
}

// Report is the aggregate over a golden set. Mean/median calls and tokens are
// taken over REACHED queries only — averaging in a miss (whose cost is the
// full top-k traversal) would conflate "expensive to reach" with "unreachable".
// ReachRate carries the unreachable signal separately.
type Report struct {
	Lane         string      `json:"lane"`
	K            int         `json:"k"`
	NumQueries   int         `json:"num_queries"`
	NumReached   int         `json:"num_reached"`
	ReachRate    float64     `json:"reach_rate"`
	MeanCalls    float64     `json:"mean_calls"`
	MedianCalls  float64     `json:"median_calls"`
	MeanTokens   float64     `json:"mean_tokens"`
	MedianTokens float64     `json:"median_tokens"`
	Results      []NavResult `json:"results"`
}

// Compute runs the deterministic no-map navigation policy over the queries and
// aggregates nav-efficiency metrics. k bounds how deep the agent reads before
// giving up. cost prices reads and the find envelope.
func Compute(queries []Query, k int, cost CostModel, lane string) Report {
	rep := Report{Lane: lane, K: k, NumQueries: len(queries)}
	var callsReached, tokensReached []int

	for _, q := range queries {
		gold := make(map[string]bool, len(q.Relevant))
		for _, r := range q.Relevant {
			gold[r] = true
		}

		depth := k
		if len(q.Ranked) < depth {
			depth = len(q.Ranked)
		}

		// Rank (1-indexed) of the first gold file within the read horizon.
		rank := 0
		for i := 0; i < depth; i++ {
			if gold[q.Ranked[i]] {
				rank = i + 1
				break
			}
		}
		reached := rank > 0

		// The agent always pays for the find envelope. On a hit it reads
		// `rank` files (top-down, inclusive of the gold one); on a miss it
		// reads the whole horizon before giving up.
		reads := rank
		if !reached {
			reads = depth
		}
		tokens := cost.FindEnvelope(q.Ranked)
		for i := 0; i < reads; i++ {
			tokens += cost.Read(q.Ranked[i])
		}
		calls := 1 + reads // the find, plus one read per file opened

		rep.Results = append(rep.Results, NavResult{
			Query:         q.Query,
			Reached:       reached,
			FirstGoldRank: rank,
			Calls:         calls,
			Tokens:        tokens,
		})
		if reached {
			callsReached = append(callsReached, calls)
			tokensReached = append(tokensReached, tokens)
		}
	}

	rep.NumReached = len(callsReached)
	if rep.NumQueries > 0 {
		rep.ReachRate = float64(rep.NumReached) / float64(rep.NumQueries)
	}
	rep.MeanCalls, rep.MedianCalls = meanMedian(callsReached)
	rep.MeanTokens, rep.MedianTokens = meanMedian(tokensReached)
	return rep
}

// meanMedian returns the arithmetic mean and median of xs (0,0 when empty).
func meanMedian(xs []int) (mean, median float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	sum := 0
	for _, x := range xs {
		sum += x
	}
	mean = float64(sum) / float64(len(xs))

	s := append([]int(nil), xs...)
	sort.Ints(s)
	n := len(s)
	if n%2 == 1 {
		median = float64(s[n/2])
	} else {
		median = float64(s[n/2-1]+s[n/2]) / 2
	}
	return mean, median
}

// JSON renders the report as indented JSON.
func (r Report) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

// Markdown renders a human-readable summary.
func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# dex bench nav — %s lane (k=%d)\n\n", r.Lane, r.K)
	fmt.Fprintf(&b, "Navigation cost to first gold-file touch (no-map baseline).\n\n")
	fmt.Fprintf(&b, "| metric | value |\n|---|---|\n")
	fmt.Fprintf(&b, "| queries | %d |\n", r.NumQueries)
	fmt.Fprintf(&b, "| reached (gold in top-%d) | %d (%.1f%%) |\n", r.K, r.NumReached, r.ReachRate*100)
	fmt.Fprintf(&b, "| mean calls to touch | %.2f |\n", r.MeanCalls)
	fmt.Fprintf(&b, "| median calls to touch | %.1f |\n", r.MedianCalls)
	fmt.Fprintf(&b, "| mean tokens to touch | %.0f |\n", r.MeanTokens)
	fmt.Fprintf(&b, "| median tokens to touch | %.0f |\n", r.MedianTokens)
	return b.String()
}

// Regression names one metric that worsened beyond tolerance.
type Regression struct {
	Metric string
	Was    float64
	Now    float64
}

func (r Regression) String() string {
	return fmt.Sprintf("%s: was %.3f, now %.3f", r.Metric, r.Was, r.Now)
}

// Regressions compares this report against a committed reference and returns
// the metrics that worsened. ReachRate must not fall by more than absTol; mean
// calls and mean tokens (the cost the explore work is trying to drive down)
// must not rise by more than relTol (a fraction, e.g. 0.05 = 5%).
func (r Report) Regressions(ref Report, absTol, relTol float64) []Regression {
	var regs []Regression
	if d := ref.ReachRate - r.ReachRate; d > absTol {
		regs = append(regs, Regression{"reach_rate", ref.ReachRate, r.ReachRate})
	}
	if ref.MeanCalls > 0 && r.MeanCalls > ref.MeanCalls*(1+relTol) {
		regs = append(regs, Regression{"mean_calls", ref.MeanCalls, r.MeanCalls})
	}
	if ref.MeanTokens > 0 && r.MeanTokens > ref.MeanTokens*(1+relTol) {
		regs = append(regs, Regression{"mean_tokens", ref.MeanTokens, r.MeanTokens})
	}
	return regs
}
