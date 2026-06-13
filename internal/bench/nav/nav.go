// Package nav implements the navigation benchmark lane (epic #316,
// story 7): the RULER that gates the explore stories. It measures how much
// work an agent spends to first touch a gold file, under a deterministic,
// zero-inference navigation policy.
//
// Phase A (this file) measures the NO-MAP baseline: the agent issues one
// find() call, then reads the ranked files top-down until it touches a
// relevant (gold) file. The metric is the count of tool calls and the tokens
// consumed up to that first touch. Phase B (gated on `dex map`, story 1) will
// re-measures with a map() call seeding navigation and reports the delta
// (ComputeMap + Compare, below).
//
// The package is intentionally decoupled from the retrieval stack: callers
// adapt their ranked results into Query values and supply a CostModel, so the
// policy is unit-testable with no index, embedder, or GPU.
package nav

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

// MapModel injects the orientation map the seeded policy navigates with. Like
// CostModel it keeps benchnav free of the graph/codemap stack — the caller
// builds it from the live communities. L0Tokens is the cost of the single
// map() L0 orientation call. Locate answers, for a gold file, whether the map
// surfaces it (named within an L0-shown cluster) and the token cost of the L1
// zoom into that cluster — i.e. can the map route an agent to this file, and
// for how many tokens.
type MapModel struct {
	L0Tokens int
	// Locate reports (l1Tokens, true) when path is named in some L0-shown
	// cluster — the agent zooms that cluster (one map() call costing l1Tokens)
	// and sees the file's location. It reports (0, false) on a map miss: the
	// file is not surfaced anywhere the agent could have navigated to from L0.
	Locate func(path string) (l1Tokens int, named bool)
}

// ComputeMap runs the deterministic MAP-SEEDED navigation policy: the agent
// issues one map() L0 call to orient, zooms the cluster that names the cheapest
// reachable gold file (one more map() call), then opens that file — three calls
// total. This is the map's ceiling: it credits L0 with routing the agent to the
// right cluster, so the lane measures the cost when the map's structure is used
// perfectly, bounded by ReachRate (how often the map names the gold file at
// all). readCost is the SAME CostModel.Read the no-map lane uses, so the
// Phase-B delta is honest. Results are emitted one-per-query in input order, so
// Compare can zip them against a no-map report over the same queries.
func ComputeMap(queries []Query, cost CostModel, m MapModel, lane string) Report {
	rep := Report{Lane: lane + " (map-seeded)", NumQueries: len(queries)}
	var callsReached, tokensReached []int
	for _, q := range queries {
		bestTokens, located := 0, false
		for _, g := range q.Relevant {
			l1, named := m.Locate(g)
			if !named {
				continue
			}
			// Cost to reach this gold via the map: orient + zoom + read it.
			t := m.L0Tokens + l1 + cost.Read(g)
			if !located || t < bestTokens {
				located = true
				bestTokens = t
			}
		}
		res := NavResult{Query: q.Query, Reached: located}
		if located {
			res.Calls = 3 // map L0 (orient) + cluster L1 (zoom) + gold read
			res.Tokens = bestTokens
			callsReached = append(callsReached, res.Calls)
			tokensReached = append(tokensReached, res.Tokens)
		} else {
			// Map miss: the agent paid for the L0 orientation and learned nothing.
			res.Calls = 1
			res.Tokens = m.L0Tokens
		}
		rep.Results = append(rep.Results, res)
	}
	rep.NumReached = len(callsReached)
	if rep.NumQueries > 0 {
		rep.ReachRate = float64(rep.NumReached) / float64(rep.NumQueries)
	}
	rep.MeanCalls, rep.MedianCalls = meanMedian(callsReached)
	rep.MeanTokens, rep.MedianTokens = meanMedian(tokensReached)
	return rep
}

// Comparison is the Phase-B headline: the no-map baseline and the map-seeded
// lane over the same golden set, plus deltas taken over the queries reached by
// BOTH lanes. Averaging a delta over a query only one lane reaches would be
// dishonest (no comparable cost on the other side), so the intersection is the
// only fair set; each lane's own ReachRate carries the coverage signal.
type Comparison struct {
	NoMap     Report `json:"no_map"`
	MapSeeded Report `json:"map_seeded"`

	BothReached         int     `json:"both_reached"`
	NoMapMeanCallsBoth  float64 `json:"no_map_mean_calls_both"`
	MapMeanCallsBoth    float64 `json:"map_mean_calls_both"`
	NoMapMeanTokensBoth float64 `json:"no_map_mean_tokens_both"`
	MapMeanTokensBoth   float64 `json:"map_mean_tokens_both"`
	DeltaMeanCalls      float64 `json:"delta_mean_calls"`  // map - no-map; negative = map cheaper
	DeltaMeanTokens     float64 `json:"delta_mean_tokens"` // map - no-map; negative = map cheaper

	// Routing is the L0-only orientation lane (issue #351): routing accuracy
	// swept over L0 budgets. The primary map-quality number post-#349 verdict.
	Routing RoutingCurve `json:"routing"`

	// Breadth is the multi-target lane (#351 phase 2): coverage of a whole
	// structural set (e.g. a symbol's call-graph neighborhood) via one map
	// enumeration vs repeated find — the regime the map should win, the inverse
	// of the first-touch lane the #349 verdict found it loses.
	Breadth BreadthReport `json:"breadth"`

	// Reorient is the post-compaction re-orientation lane (#351 phase 3,
	// gating #346 recap()): tokens to restore a session's working set via one
	// recap() digest vs replaying the exploration (re-find + full re-read).
	Reorient ReorientReport `json:"reorient"`
}

// Compare zips two reports produced over the SAME queries (Compute and
// ComputeMap emit one result per query in input order) and averages the
// cost delta over queries reached by both lanes.
func Compare(noMap, mapSeeded Report) Comparison {
	c := Comparison{NoMap: noMap, MapSeeded: mapSeeded}
	var nmCalls, mCalls, nmTok, mTok []int
	n := len(noMap.Results)
	if len(mapSeeded.Results) < n {
		n = len(mapSeeded.Results)
	}
	for i := 0; i < n; i++ {
		a, b := noMap.Results[i], mapSeeded.Results[i]
		if a.Reached && b.Reached {
			nmCalls = append(nmCalls, a.Calls)
			mCalls = append(mCalls, b.Calls)
			nmTok = append(nmTok, a.Tokens)
			mTok = append(mTok, b.Tokens)
		}
	}
	c.BothReached = len(nmCalls)
	c.NoMapMeanCallsBoth, _ = meanMedian(nmCalls)
	c.MapMeanCallsBoth, _ = meanMedian(mCalls)
	c.NoMapMeanTokensBoth, _ = meanMedian(nmTok)
	c.MapMeanTokensBoth, _ = meanMedian(mTok)
	c.DeltaMeanCalls = c.MapMeanCallsBoth - c.NoMapMeanCallsBoth
	c.DeltaMeanTokens = c.MapMeanTokensBoth - c.NoMapMeanTokensBoth
	return c
}

// JSON renders the comparison as indented JSON.
func (c Comparison) JSON() ([]byte, error) { return json.MarshalIndent(c, "", "  ") }

// Markdown renders the map-vs-no-map summary and the both-reached delta.
func (c Comparison) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# dex bench nav — map vs no-map (%s lane)\n\n", c.NoMap.Lane)
	fmt.Fprintf(&b, "Cost to first gold-file touch. Map quality = gold file named within the\n")
	fmt.Fprintf(&b, "L0-shown clusters (reach); interface quality = fewer calls + tokens to that touch.\n\n")
	fmt.Fprintf(&b, "| metric | no-map | map-seeded |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| queries | %d | %d |\n", c.NoMap.NumQueries, c.MapSeeded.NumQueries)
	fmt.Fprintf(&b, "| reached | %d (%.1f%%) | %d (%.1f%%) |\n",
		c.NoMap.NumReached, c.NoMap.ReachRate*100, c.MapSeeded.NumReached, c.MapSeeded.ReachRate*100)
	fmt.Fprintf(&b, "| mean calls to touch | %.2f | %.2f |\n", c.NoMap.MeanCalls, c.MapSeeded.MeanCalls)
	fmt.Fprintf(&b, "| mean tokens to touch | %.0f | %.0f |\n", c.NoMap.MeanTokens, c.MapSeeded.MeanTokens)
	fmt.Fprintf(&b, "\n**Delta over %d queries reached by both lanes** (negative = map cheaper):\n\n", c.BothReached)
	fmt.Fprintf(&b, "| metric | no-map | map-seeded | delta |\n|---|---|---|---|\n")
	fmt.Fprintf(&b, "| mean calls | %.2f | %.2f | %+.2f |\n", c.NoMapMeanCallsBoth, c.MapMeanCallsBoth, c.DeltaMeanCalls)
	fmt.Fprintf(&b, "| mean tokens | %.0f | %.0f | %+.0f |\n", c.NoMapMeanTokensBoth, c.MapMeanTokensBoth, c.DeltaMeanTokens)
	if len(c.Routing.Points) > 0 {
		b.WriteString("\n")
		b.WriteString(c.Routing.Markdown())
	}
	if len(c.Breadth.Results) > 0 {
		b.WriteString("\n")
		b.WriteString(c.Breadth.Markdown())
	}
	if len(c.Reorient.Results) > 0 {
		b.WriteString("\n")
		b.WriteString(c.Reorient.Markdown())
	}
	return b.String()
}

// Regressions gates both lanes against a committed reference (each lane's own
// reach/calls/tokens via Report.Regressions, map lane metrics prefixed "map_")
// and flags erosion of the map's token advantage on the both-reached set — the
// explore epic's whole thesis is that the map keeps navigation cheaper.
func (c Comparison) Regressions(ref Comparison, absTol, relTol float64) []Regression {
	regs := c.NoMap.Regressions(ref.NoMap, absTol, relTol)
	for _, r := range c.MapSeeded.Regressions(ref.MapSeeded, absTol, relTol) {
		r.Metric = "map_" + r.Metric
		regs = append(regs, r)
	}
	// Map advantage is a negative delta; erosion = it rising toward 0. Only gate
	// it once the advantage is MATERIAL (beyond 5% of the no-map cost) — a tiny
	// advantage is within run-to-run noise and gating it would just flake. When
	// material, bound it to within relTol of the reference's (negative) edge.
	material := ref.DeltaMeanTokens < -0.05*ref.NoMapMeanTokensBoth
	if material {
		if ceil := ref.DeltaMeanTokens * (1 - relTol); c.DeltaMeanTokens > ceil {
			regs = append(regs, Regression{"map_advantage_tokens", ref.DeltaMeanTokens, c.DeltaMeanTokens})
		}
	}
	// Routing accuracy is a floor at every budget — stories must raise it.
	regs = append(regs, c.Routing.Regressions(ref.Routing, absTol)...)
	// Breadth coverage is a floor and its advantage may not erode (#351 ph2).
	regs = append(regs, c.Breadth.Regressions(ref.Breadth, absTol)...)
	// Re-orientation: recap coverage is a floor and its token advantage over
	// re-exploration may not erode — recap's home regime (#351 ph3).
	regs = append(regs, c.Reorient.Regressions(ref.Reorient, absTol, relTol)...)
	return regs
}
