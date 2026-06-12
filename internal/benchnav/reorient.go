package benchnav

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ReorientTask is a post-compaction re-orientation goal (issue #351 phase 3,
// gating #346 recap()): an agent worked through a SESSION of several find()s,
// opened the files they surfaced, then context compaction wiped that working
// set. The task is to restore it. Working is the working set (the union of the
// session's reachable gold files — what the agent had in context); Queries is
// the session's find()s to replay, each carrying its ranking and the
// working-set members it was responsible for. Both lanes target the SAME
// working set, so the comparison is honest.
type ReorientTask struct {
	Task    string          `json:"task"`    // human label, e.g. "session 3 (queries 11-15)"
	Working []string        `json:"working"` // working set: reachable gold across the session
	Queries []ReorientQuery `json:"queries"` // the session's find()s, for the re-exploration lane
}

// ReorientQuery is one find() within a session: its ranked result and the
// working-set members the agent originally touched through it.
type ReorientQuery struct {
	Ranked []string `json:"ranked"` // the find()'s ranked paths, best-first
	Gold   []string `json:"gold"`   // this query's working-set members (reachable within k)
}

// ReorientModel injects recap()'s cost, kept free of the live compress/graph
// stack like the other lane models. RecapBudget is the token budget for the one
// recap() call (issue #346 budget plumbing); the digest greedily packs
// working-set entries until it is exhausted, so an oversized working set
// truncates and recap coverage drops — the honest budget tradeoff. Entry prices
// one file's slot in the digest: a compressed signature skeleton (its path plus
// the symbol names it defines), NOT a full re-read. That asymmetry IS the recap
// thesis — restore WHERE you were from a compact digest instead of re-running
// the exploration and re-reading the files.
type ReorientModel struct {
	RecapBudget int
	Entry       func(path string) int
}

// ReorientResult is one session's outcome under both lanes. Coverage is the
// fraction of the working set each lane restores; tokens/calls is the restore
// cost. Re-exploration re-reads the files (full content) after re-finding them,
// so it pays the find envelopes plus full Read tokens across many calls; recap
// pays one budget-bounded digest. Re-exploration coverage is ~100% by
// construction (the working set is defined as reachable), so the headline is the
// token gap — and recap's coverage only when the budget forces truncation.
type ReorientResult struct {
	Task    string `json:"task"`
	Working int    `json:"working"`

	ReexploreFound    int     `json:"reexplore_found"`
	ReexploreCoverage float64 `json:"reexplore_coverage"`
	ReexploreCalls    int     `json:"reexplore_calls"`
	ReexploreTokens   int     `json:"reexplore_tokens"`

	RecapFound    int     `json:"recap_found"`
	RecapCoverage float64 `json:"recap_coverage"`
	RecapCalls    int     `json:"recap_calls"`
	RecapTokens   int     `json:"recap_tokens"`
}

// ReorientReport aggregates the re-orientation lane over a session set. Means
// are over all sessions (every session has a non-empty working set). Token delta
// is negative when recap is the cheaper way to restore context; coverage delta
// is negative when the recap budget cannot hold the whole working set that
// re-exploration would rebuild.
type ReorientReport struct {
	Lane        string `json:"lane"`
	NumSessions int    `json:"num_sessions"`
	K           int    `json:"k"`
	RecapBudget int    `json:"recap_budget"`

	MeanReexploreCoverage float64 `json:"mean_reexplore_coverage"`
	MeanRecapCoverage     float64 `json:"mean_recap_coverage"`
	DeltaCoverage         float64 `json:"delta_coverage"` // recap - reexplore

	MeanReexploreTokens float64 `json:"mean_reexplore_tokens"`
	MeanRecapTokens     float64 `json:"mean_recap_tokens"`
	DeltaTokens         float64 `json:"delta_tokens"` // recap - reexplore; negative = recap cheaper

	MeanReexploreCalls float64 `json:"mean_reexplore_calls"`
	MeanRecapCalls     float64 `json:"mean_recap_calls"`

	Results []ReorientResult `json:"results"`
}

// ComputeReorient runs both deterministic restore lanes over the sessions. k
// bounds how deep the re-exploration lane re-reads each session query's ranking.
// Re-exploration replays the session — find envelope + full Read of the files
// needed to re-cover that query's working-set members — across one find plus one
// read per file opened. Recap issues a single call whose digest greedily packs
// working-set entries (cheapest first, to fit the most) until RecapBudget is
// spent; entries that do not fit are not restored.
func ComputeReorient(tasks []ReorientTask, k int, cost CostModel, m ReorientModel, lane string) ReorientReport {
	rep := ReorientReport{Lane: lane, NumSessions: len(tasks), K: k, RecapBudget: m.RecapBudget}
	var reCov, rcCov, reTok, rcTok, reCalls, rcCalls []float64

	for _, t := range tasks {
		total := len(t.Working)
		res := ReorientResult{Task: t.Task, Working: total}

		// Re-exploration lane: replay every find() the session ran, re-reading
		// top-down until that query's working-set members are re-touched. The
		// agent has no memory of the answers, so it pays the full find + read
		// cost again. Files re-touched are credited once across the session.
		restored := make(map[string]bool, total)
		for _, q := range t.Queries {
			gold := make(map[string]bool, len(q.Gold))
			for _, g := range q.Gold {
				gold[g] = true
			}
			depth := k
			if len(q.Ranked) < depth {
				depth = len(q.Ranked)
			}
			res.ReexploreTokens += cost.FindEnvelope(q.Ranked)
			res.ReexploreCalls++ // the find
			need := len(gold)
			for i := 0; i < depth && need > 0; i++ {
				p := q.Ranked[i]
				res.ReexploreTokens += cost.Read(p)
				res.ReexploreCalls++ // one read per file opened
				if gold[p] && !restored[p] {
					restored[p] = true
				}
				if gold[p] {
					need--
				}
			}
		}
		res.ReexploreFound = len(restored)

		// Recap lane: one call. Pack working-set entries cheapest-first so the
		// fixed budget restores as many files as possible; coverage is what fit.
		entries := make([]int, 0, total)
		for _, f := range t.Working {
			entries = append(entries, m.Entry(f))
		}
		sort.Ints(entries)
		spent, fit := 0, 0
		for _, e := range entries {
			if spent+e > m.RecapBudget {
				break
			}
			spent += e
			fit++
		}
		res.RecapTokens = spent
		res.RecapFound = fit
		res.RecapCalls = 1

		if total > 0 {
			res.ReexploreCoverage = float64(res.ReexploreFound) / float64(total)
			res.RecapCoverage = float64(res.RecapFound) / float64(total)
		}
		rep.Results = append(rep.Results, res)

		reCov = append(reCov, res.ReexploreCoverage)
		rcCov = append(rcCov, res.RecapCoverage)
		reTok = append(reTok, float64(res.ReexploreTokens))
		rcTok = append(rcTok, float64(res.RecapTokens))
		reCalls = append(reCalls, float64(res.ReexploreCalls))
		rcCalls = append(rcCalls, float64(res.RecapCalls))
	}

	rep.MeanReexploreCoverage = meanFloat(reCov)
	rep.MeanRecapCoverage = meanFloat(rcCov)
	rep.DeltaCoverage = rep.MeanRecapCoverage - rep.MeanReexploreCoverage
	rep.MeanReexploreTokens = meanFloat(reTok)
	rep.MeanRecapTokens = meanFloat(rcTok)
	rep.DeltaTokens = rep.MeanRecapTokens - rep.MeanReexploreTokens
	rep.MeanReexploreCalls = meanFloat(reCalls)
	rep.MeanRecapCalls = meanFloat(rcCalls)
	return rep
}

// JSON renders the re-orientation report.
func (r ReorientReport) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// Markdown renders the re-orientation summary table.
func (r ReorientReport) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## re-orientation lane (%s) — restore a working set, recap vs re-explore\n\n", r.Lane)
	fmt.Fprintf(&b, "Post-compaction the agent lost a session's working set. Re-explore replays the\n")
	fmt.Fprintf(&b, "session's find()s and re-reads (full) to k=%d; recap() issues ONE digest call\n", r.K)
	fmt.Fprintf(&b, "fitted to a %d-token budget (path + symbol skeleton per file). The regime #346\n", r.RecapBudget)
	fmt.Fprintf(&b, "recap() must win — cheap restore of WHERE you were, not redoing exploration.\n\n")
	fmt.Fprintf(&b, "| metric | re-explore | recap | delta |\n|---|---|---|---|\n")
	fmt.Fprintf(&b, "| mean coverage | %.1f%% | %.1f%% | %+.1f%% |\n",
		r.MeanReexploreCoverage*100, r.MeanRecapCoverage*100, r.DeltaCoverage*100)
	fmt.Fprintf(&b, "| mean tokens | %.0f | %.0f | %+.0f |\n", r.MeanReexploreTokens, r.MeanRecapTokens, r.DeltaTokens)
	fmt.Fprintf(&b, "| mean calls | %.2f | %.2f | %+.2f |\n",
		r.MeanReexploreCalls, r.MeanRecapCalls, r.MeanRecapCalls-r.MeanReexploreCalls)
	fmt.Fprintf(&b, "\n%d sessions.\n", r.NumSessions)
	return b.String()
}

// Regressions gates the re-orientation lane against a committed reference. Recap
// coverage is a FLOOR (recap must keep restoring at least as much of the working
// set within its budget), and recap's token advantage over re-exploration may
// not erode — re-orientation is the regime recap is built to win, so ceding the
// token edge is the failure to catch. Mirrors the map-advantage gate: the edge
// is only enforced once it is MATERIAL (beyond 5% of the re-explore cost), so a
// marginal advantage stays within run-to-run noise instead of flaking. absTol is
// an absolute coverage fraction (e.g. 0.02 = 2 points); relTol bounds erosion.
func (r ReorientReport) Regressions(ref ReorientReport, absTol, relTol float64) []Regression {
	var regs []Regression
	if ref.MeanRecapCoverage-r.MeanRecapCoverage > absTol {
		regs = append(regs, Regression{
			Metric: "reorient_recap_coverage",
			Was:    ref.MeanRecapCoverage,
			Now:    r.MeanRecapCoverage,
		})
	}
	material := ref.DeltaTokens < -0.05*ref.MeanReexploreTokens
	if material {
		if ceil := ref.DeltaTokens * (1 - relTol); r.DeltaTokens > ceil {
			regs = append(regs, Regression{
				Metric: "reorient_advantage_tokens",
				Was:    ref.DeltaTokens,
				Now:    r.DeltaTokens,
			})
		}
	}
	return regs
}
