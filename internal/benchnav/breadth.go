package benchnav

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BreadthTask is a multi-target navigation goal: enumerate a whole STRUCTURAL
// set, not just touch the first hit. The canonical task (issue #351 phase 2) is
// "the call-graph neighborhood of a hub symbol" — Targets is the set of files
// holding the seed's callers ∪ callees (ground truth from the graph edges,
// uncapped), and Ranked is what one find() over the seed returns, for the no-map
// lane. This is the regime where the map's community structure should beat flat
// semantic retrieval: communities are built from call edges, so a hub's
// neighbors cluster together and one L1 zoom enumerates them — whereas find()
// ranks by similarity, not adjacency. The inverse of the single-target
// first-touch lane the #349 verdict found the map loses.
type BreadthTask struct {
	Task    string   `json:"task"`    // human label, e.g. "neighborhood of store.Open"
	Targets []string `json:"targets"` // ground-truth set (repo-relative neighbor files)
	Ranked  []string `json:"ranked"`  // one find()'s ranked result, for the no-map lane
}

// BreadthModel injects the map's enumeration power, built by the caller from the
// live communities. L0Tokens prices the single orientation call. Cluster reports,
// for a target path, the cluster that names it within the L0-shown overview: its
// id (so each distinct zoom is charged once), the L1 zoom token cost, and whether
// the map surfaces it at all. A miss (found=false) means the file is in no
// L0-shown cluster's rendered L1 — the map cannot route an agent to it, honoring
// L1 budget truncation exactly as the agent would see it.
type BreadthModel struct {
	L0Tokens int
	Cluster  func(path string) (id, l1Tokens int, found bool)
}

// AroundModel prices the EXACT-neighborhood variant: `map --around <seed>`
// renders the seed's callers ∪ callees region directly (issue #347), so — unlike
// the community lane, which routes each target through its global Louvain cluster
// and misses neighbors that landed in a different community — coverage is the
// neighborhood by construction, bounded only by the L1 token budget. Region
// returns, for a task, the single around-render's text (so coverage is read off
// the same truncation the agent sees, via the same substring test the community
// lane uses) and its token cost; ok is false when the seed has no graph edges to
// render. The cost is always one call. A zero-value AroundModel (nil Region)
// disables the exact lane, leaving the no-map/community report untouched.
type AroundModel struct {
	Region func(task string) (text string, tokens int, ok bool)
}

// BreadthResult is one task's outcome under the lanes. Coverage is the fraction
// of the target set each lane ENUMERATES; tokens/calls is the discovery cost —
// the listing the agent reads to surface the set (find's ranked paths vs the
// map's L0+L1 vs the around region). File reads are deliberately excluded:
// opening the located files is a downstream step every lane shares, and charging
// it buries the enumeration signal under file size (the package-breadth pilot,
// #351 ph2, showed this). Exact* fields are populated only when an AroundModel is
// supplied and the seed has edges (HasExact).
type BreadthResult struct {
	Task    string `json:"task"`
	Targets int    `json:"targets"`

	NoMapFound    int     `json:"no_map_found"`
	NoMapCoverage float64 `json:"no_map_coverage"`
	NoMapCalls    int     `json:"no_map_calls"`
	NoMapTokens   int     `json:"no_map_tokens"`

	MapFound    int     `json:"map_found"`
	MapCoverage float64 `json:"map_coverage"`
	MapCalls    int     `json:"map_calls"`
	MapTokens   int     `json:"map_tokens"`

	HasExact      bool    `json:"has_exact,omitempty"`
	ExactFound    int     `json:"exact_found,omitempty"`
	ExactCoverage float64 `json:"exact_coverage,omitempty"`
	ExactCalls    int     `json:"exact_calls,omitempty"`
	ExactTokens   int     `json:"exact_tokens,omitempty"`
}

// BreadthReport aggregates the breadth lane over a task set. Means are taken over
// all tasks (every task has a target set, so there is no "unreached" skew like the
// first-touch lane's both-reached intersection). Coverage delta is positive when
// the map enumerates more of the target set; token delta is negative when the map
// is the cheaper way to discover it. The Exact* aggregates and their deltas are
// present only when an AroundModel was supplied (HasExact); the exact-vs-map
// deltas are #356's headline: does rendering the neighborhood directly beat
// routing through global communities?
type BreadthReport struct {
	Lane     string `json:"lane"`
	NumTasks int    `json:"num_tasks"`
	K        int    `json:"k"`

	MeanNoMapCoverage float64 `json:"mean_no_map_coverage"`
	MeanMapCoverage   float64 `json:"mean_map_coverage"`
	DeltaCoverage     float64 `json:"delta_coverage"` // map - no-map; positive = map enumerates more

	MeanNoMapTokens float64 `json:"mean_no_map_tokens"`
	MeanMapTokens   float64 `json:"mean_map_tokens"`
	DeltaTokens     float64 `json:"delta_tokens"` // map - no-map; negative = map cheaper

	MeanNoMapCalls float64 `json:"mean_no_map_calls"`
	MeanMapCalls   float64 `json:"mean_map_calls"`

	HasExact                bool    `json:"has_exact,omitempty"`
	NumExact                int     `json:"num_exact,omitempty"`
	MeanExactCoverage       float64 `json:"mean_exact_coverage,omitempty"`
	DeltaExactVsMapCoverage float64 `json:"delta_exact_vs_map_coverage,omitempty"` // exact - map; positive = direct render enumerates more
	MeanExactTokens         float64 `json:"mean_exact_tokens,omitempty"`
	DeltaExactVsMapTokens   float64 `json:"delta_exact_vs_map_tokens,omitempty"` // exact - map; negative = around cheaper than community zooms
	MeanExactCalls          float64 `json:"mean_exact_calls,omitempty"`

	Results []BreadthResult `json:"results"`
}

// ComputeBreadth runs the deterministic lanes over the breadth tasks. k bounds
// how deep the no-map lane scans the ranked list. This is a DISCOVERY metric:
// each lane is charged only for the listing it reads to enumerate the set — the
// no-map find envelope, the map's L0 + covering-cluster L1s, and the exact
// around-region render — never for opening the located files (a shared downstream
// step). cost.FindEnvelope prices the no-map listing; cost.Read is unused here by
// design. A zero-value AroundModel disables the exact lane.
func ComputeBreadth(tasks []BreadthTask, k int, cost CostModel, m BreadthModel, around AroundModel, lane string) BreadthReport {
	rep := BreadthReport{Lane: lane, NumTasks: len(tasks), K: k}
	var nmCov, mCov, nmTok, mTok, nmCalls, mCalls []float64
	var exCov, exTok, exCalls, exMapCovPaired, exMapTokPaired []float64

	for _, t := range tasks {
		gold := make(map[string]bool, len(t.Targets))
		for _, g := range t.Targets {
			gold[g] = true
		}
		total := len(gold)
		res := BreadthResult{Task: t.Task, Targets: total}

		// no-map lane: one find(), enumerate the neighbor files visible in the
		// top-k ranking. The agent reads the ranked path list (the envelope), not
		// the files — membership in the target set is decided from the paths.
		depth := k
		if len(t.Ranked) < depth {
			depth = len(t.Ranked)
		}
		nmFound := map[string]bool{}
		for i := 0; i < depth; i++ {
			if gold[t.Ranked[i]] {
				nmFound[t.Ranked[i]] = true
			}
		}
		res.NoMapFound = len(nmFound)
		res.NoMapTokens = cost.FindEnvelope(t.Ranked)
		res.NoMapCalls = 1

		// map (community) lane: one L0, zoom each DISTINCT cluster naming a target,
		// read the L1 listing. Shared clusters are zoomed once; the listing
		// enumerates the region's members for free (no file opens).
		zoomed := map[int]bool{}
		done := make(map[string]bool, total)
		var mFound int
		mTokens := m.L0Tokens
		for _, g := range t.Targets {
			if done[g] {
				continue
			}
			done[g] = true
			id, l1, ok := m.Cluster(g)
			if !ok {
				continue
			}
			mFound++
			if !zoomed[id] {
				zoomed[id] = true
				mTokens += l1
			}
		}
		res.MapFound = mFound
		res.MapTokens = mTokens
		res.MapCalls = 1 + len(zoomed) // L0 + one zoom per distinct covering cluster

		if total > 0 {
			res.NoMapCoverage = float64(res.NoMapFound) / float64(total)
			res.MapCoverage = float64(res.MapFound) / float64(total)
		}

		// exact (around) lane: one `map --around <seed>` render — the neighborhood
		// by construction. Coverage honors L1 truncation (a target absent from the
		// budgeted text is a real miss), at a flat one-call cost. Skipped when no
		// AroundModel is wired or the seed has no edges to render.
		if around.Region != nil {
			if text, tok, ok := around.Region(t.Task); ok {
				var ef int
				for g := range gold {
					if strings.Contains(text, g) {
						ef++
					}
				}
				res.HasExact = true
				res.ExactFound = ef
				res.ExactTokens = tok
				res.ExactCalls = 1
				if total > 0 {
					res.ExactCoverage = float64(ef) / float64(total)
				}
			}
		}

		rep.Results = append(rep.Results, res)

		nmCov = append(nmCov, res.NoMapCoverage)
		mCov = append(mCov, res.MapCoverage)
		nmTok = append(nmTok, float64(res.NoMapTokens))
		mTok = append(mTok, float64(res.MapTokens))
		nmCalls = append(nmCalls, float64(res.NoMapCalls))
		mCalls = append(mCalls, float64(res.MapCalls))

		if res.HasExact {
			exCov = append(exCov, res.ExactCoverage)
			exTok = append(exTok, float64(res.ExactTokens))
			exCalls = append(exCalls, float64(res.ExactCalls))
			// Pair the community-lane figures only over the tasks the exact lane
			// also scored, so the exact-vs-map deltas compare like with like.
			exMapCovPaired = append(exMapCovPaired, res.MapCoverage)
			exMapTokPaired = append(exMapTokPaired, float64(res.MapTokens))
		}
	}

	rep.MeanNoMapCoverage = meanFloat(nmCov)
	rep.MeanMapCoverage = meanFloat(mCov)
	rep.DeltaCoverage = rep.MeanMapCoverage - rep.MeanNoMapCoverage
	rep.MeanNoMapTokens = meanFloat(nmTok)
	rep.MeanMapTokens = meanFloat(mTok)
	rep.DeltaTokens = rep.MeanMapTokens - rep.MeanNoMapTokens
	rep.MeanNoMapCalls = meanFloat(nmCalls)
	rep.MeanMapCalls = meanFloat(mCalls)

	if len(exCov) > 0 {
		rep.HasExact = true
		rep.NumExact = len(exCov)
		rep.MeanExactCoverage = meanFloat(exCov)
		rep.MeanExactTokens = meanFloat(exTok)
		rep.MeanExactCalls = meanFloat(exCalls)
		rep.DeltaExactVsMapCoverage = rep.MeanExactCoverage - meanFloat(exMapCovPaired)
		rep.DeltaExactVsMapTokens = rep.MeanExactTokens - meanFloat(exMapTokPaired)
	}
	return rep
}

// meanFloat is the arithmetic mean, 0 over an empty slice.
func meanFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// JSON renders the breadth report.
func (r BreadthReport) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// Markdown renders the breadth summary table.
func (r BreadthReport) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## breadth-task lane (%s) — neighborhood enumeration, map vs find\n\n", r.Lane)
	fmt.Fprintf(&b, "Enumerate a hub symbol's call-graph neighborhood (callers ∪ callees). No-map\n")
	fmt.Fprintf(&b, "scans the find() ranking to k=%d; map zooms the covering Louvain clusters.\n", r.K)
	if r.HasExact {
		fmt.Fprintf(&b, "Exact renders `map --around <seed>` — the neighborhood directly (#347/#356).\n")
	}
	fmt.Fprintf(&b, "Discovery cost only (listing read, not file opens). The regime the map should\n")
	fmt.Fprintf(&b, "win — structural breadth, not the first-touch the #349 verdict tested.\n\n")

	if r.HasExact {
		fmt.Fprintf(&b, "| metric | no-map | map (community) | exact (around) |\n|---|---|---|---|\n")
		fmt.Fprintf(&b, "| mean coverage | %.1f%% | %.1f%% | %.1f%% |\n",
			r.MeanNoMapCoverage*100, r.MeanMapCoverage*100, r.MeanExactCoverage*100)
		fmt.Fprintf(&b, "| mean tokens | %.0f | %.0f | %.0f |\n",
			r.MeanNoMapTokens, r.MeanMapTokens, r.MeanExactTokens)
		fmt.Fprintf(&b, "| mean calls | %.2f | %.2f | %.2f |\n",
			r.MeanNoMapCalls, r.MeanMapCalls, r.MeanExactCalls)
		fmt.Fprintf(&b, "\nexact − community: %+.1f%% coverage, %+.0f tokens (over %d seeds with edges).\n",
			r.DeltaExactVsMapCoverage*100, r.DeltaExactVsMapTokens, r.NumExact)
		fmt.Fprintf(&b, "\n%d breadth tasks.\n", r.NumTasks)
		return b.String()
	}

	fmt.Fprintf(&b, "| metric | no-map | map | delta |\n|---|---|---|---|\n")
	fmt.Fprintf(&b, "| mean coverage | %.1f%% | %.1f%% | %+.1f%% |\n",
		r.MeanNoMapCoverage*100, r.MeanMapCoverage*100, r.DeltaCoverage*100)
	fmt.Fprintf(&b, "| mean tokens | %.0f | %.0f | %+.0f |\n", r.MeanNoMapTokens, r.MeanMapTokens, r.DeltaTokens)
	fmt.Fprintf(&b, "| mean calls | %.2f | %.2f | %+.2f |\n",
		r.MeanNoMapCalls, r.MeanMapCalls, r.MeanMapCalls-r.MeanNoMapCalls)
	fmt.Fprintf(&b, "\n%d breadth tasks.\n", r.NumTasks)
	return b.String()
}

// Regressions gates the breadth lane against a committed reference. Map coverage
// is a FLOOR (the map must keep enumerating at least as much of each target set),
// and the map's coverage advantage over find may not erode — breadth is the
// regime the map is supposed to win, so ceding ground here is the failure to catch.
// When the reference carries an exact lane, the exact coverage is held to its own
// floor and the exact-over-community advantage (#356's headline) may not erode.
// absTol is an absolute fraction (e.g. 0.02 = 2 points).
func (r BreadthReport) Regressions(ref BreadthReport, absTol float64) []Regression {
	var regs []Regression
	if ref.MeanMapCoverage-r.MeanMapCoverage > absTol {
		regs = append(regs, Regression{
			Metric: "breadth_map_coverage",
			Was:    ref.MeanMapCoverage,
			Now:    r.MeanMapCoverage,
		})
	}
	if ref.DeltaCoverage-r.DeltaCoverage > absTol {
		regs = append(regs, Regression{
			Metric: "breadth_coverage_advantage",
			Was:    ref.DeltaCoverage,
			Now:    r.DeltaCoverage,
		})
	}
	if ref.HasExact && r.HasExact {
		if ref.MeanExactCoverage-r.MeanExactCoverage > absTol {
			regs = append(regs, Regression{
				Metric: "breadth_exact_coverage",
				Was:    ref.MeanExactCoverage,
				Now:    r.MeanExactCoverage,
			})
		}
		if ref.DeltaExactVsMapCoverage-r.DeltaExactVsMapCoverage > absTol {
			regs = append(regs, Regression{
				Metric: "breadth_exact_vs_map_advantage",
				Was:    ref.DeltaExactVsMapCoverage,
				Now:    r.DeltaExactVsMapCoverage,
			})
		}
	}
	return regs
}
