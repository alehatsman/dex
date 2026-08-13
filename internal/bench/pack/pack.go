// Package pack benchmarks the cost of assembling a modify-symbol working set
// two ways: the primitive multi-call path (locate → read → trace callers →
// trace callees → find tests → read each) versus the one-call context pack
// (ask intent=assemble). It instruments epic #95's two open acceptance
// criteria — AC #2 (materially fewer retrieval calls) and AC #6 (fewer tokens
// without reducing correctness) — as a deterministic, gateable model.
//
// The package is pure: costs and pack outcomes are injected, so it needs no
// index, embedder, or tokenizer and runs in unit tests. The CLI (dex bench
// pack) wires the live Assembler and graph into the same model. It mirrors the
// shape of internal/bench/nav.
//
// Correctness guard: the cost delta is reported over reached tasks (the pack
// returned usable evidence), and mean coverage — the fraction of the true
// call-graph ripple the pack surfaced — is reported alongside as the
// correctness floor. A pack that is cheap because it returned less shows up as
// lower ripple recall, not a hidden gain, and a recall drop fails the
// regression gate. On dex's own repo full ripple coverage is the exception, so
// gating the delta on coverage==1.0 would discard almost every real task; the
// recall floor is the honest form of "without reducing correctness".
package pack

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Task is one modify-symbol scenario: to safely change Symbol, an agent must
// assemble its working set (Gold = def ∪ callers ∪ callees ∪ tests).
type Task struct {
	Symbol string   `json:"symbol"`
	Def    string   `json:"def"`  // S's own file, repo-relative
	Gold   []string `json:"gold"` // required working-set files
}

// CostModel prices the primitive actions. Injected so the package touches no
// disk or tokenizer. Read = tokens to read one file's contents;
// TraceEnvelope = tokens of a locate/trace result envelope the agent scans.
type CostModel struct {
	Read          func(path string) int
	TraceEnvelope func(paths []string) int
}

// PackModel injects the live pack outcome for one task: the files the pack
// surfaced and its rendered token cost. ok=false means the pack missed the
// symbol entirely.
type PackModel struct {
	Surfaced func(symbol string) (files []string, tokens int, ok bool)
}

// Result is the per-task outcome.
type Result struct {
	Symbol          string  `json:"symbol"`
	PrimitiveCalls  int     `json:"primitive_calls"`
	PrimitiveTokens int     `json:"primitive_tokens"`
	PackCalls       int     `json:"pack_calls"`  // 1 on a hit, 0 on a miss
	PackTokens      int     `json:"pack_tokens"` // 0 on a miss
	Coverage        float64 `json:"coverage"`    // |gold ∩ surfaced| / |gold|
	FullyCovered    bool    `json:"fully_covered"`
	Hit             bool    `json:"hit"`
}

// Report aggregates the run. Cost stats are computed over reached tasks (the
// pack returned usable evidence); ripple recall (mean coverage) is reported
// alongside as the correctness floor, so a pack that is cheap because it
// returned less is visible rather than hidden.
type Report struct {
	NumTasks      int     `json:"num_tasks"`
	NumHit        int     `json:"num_hit"`         // pack returned usable evidence
	NumCovered    int     `json:"num_covered"`     // coverage == 1.0 (full ripple)
	ReachRate     float64 `json:"reach_rate"`      // NumHit / NumTasks
	FullCoverRate float64 `json:"full_cover_rate"` // NumCovered / NumHit
	MeanCoverage  float64 `json:"mean_coverage"`   // ripple recall — correctness floor

	// Cost over reached tasks: assembling the true ripple set by hand vs the
	// one-call pack.
	MeanPrimitiveCalls  float64 `json:"mean_primitive_calls"`
	MeanPrimitiveTokens float64 `json:"mean_primitive_tokens"`
	MeanPackCalls       float64 `json:"mean_pack_calls"`
	MeanPackTokens      float64 `json:"mean_pack_tokens"`
	MedianCallsDelta    float64 `json:"median_calls_delta"`  // pack − primitive
	MedianTokensDelta   float64 `json:"median_tokens_delta"` // pack − primitive
	CallsSavedPct       float64 `json:"calls_saved_pct"`     // 1 − pack/primitive
	TokensSavedPct      float64 `json:"tokens_saved_pct"`

	Results []Result `json:"results,omitempty"`
}

// Compute runs the primitive-vs-pack model over the tasks.
func Compute(tasks []Task, cost CostModel, pm PackModel) Report {
	rep := Report{NumTasks: len(tasks)}

	var (
		sumCov                  float64
		primCalls, primTokens   []int
		packCalls, packTokens   []int
		callsDelta, tokensDelta []int
	)

	for _, t := range tasks {
		res := Result{Symbol: t.Symbol}

		// Primitive path to assemble the true ripple set: locate(1) +
		// trace callers(1) + trace callees(1) + find tests(1) + one read per
		// distinct gold file. The trace envelopes list the gold neighbours.
		res.PrimitiveCalls = primitiveCalls(t)
		res.PrimitiveTokens = primitiveTokens(t, cost)

		files, toks, ok := pm.Surfaced(t.Symbol)
		res.Hit = ok
		if ok {
			res.PackCalls = 1
			res.PackTokens = toks
			res.Coverage = coverage(t.Gold, files)
			res.FullyCovered = res.Coverage >= 1.0
			rep.NumHit++
			sumCov += res.Coverage
			if res.FullyCovered {
				rep.NumCovered++
			}
			primCalls = append(primCalls, res.PrimitiveCalls)
			primTokens = append(primTokens, res.PrimitiveTokens)
			packCalls = append(packCalls, res.PackCalls)
			packTokens = append(packTokens, res.PackTokens)
			callsDelta = append(callsDelta, res.PackCalls-res.PrimitiveCalls)
			tokensDelta = append(tokensDelta, res.PackTokens-res.PrimitiveTokens)
		}
		rep.Results = append(rep.Results, res)
	}

	if rep.NumTasks > 0 {
		rep.ReachRate = float64(rep.NumHit) / float64(rep.NumTasks)
	}
	if rep.NumHit > 0 {
		rep.FullCoverRate = float64(rep.NumCovered) / float64(rep.NumHit)
		rep.MeanCoverage = sumCov / float64(rep.NumHit)
	}

	rep.MeanPrimitiveCalls, _ = meanMedian(primCalls)
	rep.MeanPrimitiveTokens, _ = meanMedian(primTokens)
	rep.MeanPackCalls, _ = meanMedian(packCalls)
	rep.MeanPackTokens, _ = meanMedian(packTokens)
	_, rep.MedianCallsDelta = meanMedian(callsDelta)
	_, rep.MedianTokensDelta = meanMedian(tokensDelta)

	if rep.MeanPrimitiveCalls > 0 {
		rep.CallsSavedPct = 1 - rep.MeanPackCalls/rep.MeanPrimitiveCalls
	}
	if rep.MeanPrimitiveTokens > 0 {
		rep.TokensSavedPct = 1 - rep.MeanPackTokens/rep.MeanPrimitiveTokens
	}
	return rep
}

// primitiveCalls counts the tool calls to assemble the working set by hand:
// locate S (1), trace callers (1), trace callees (1), find tests (1), plus one
// read per distinct gold file.
func primitiveCalls(t Task) int {
	return 4 + len(distinct(t.Gold))
}

// primitiveTokens sums the read tokens of every gold file plus one trace
// envelope listing the gold neighbours (the agent must scan it to know what to
// open).
func primitiveTokens(t Task, cost CostModel) int {
	total := 0
	gold := distinct(t.Gold)
	for _, f := range gold {
		total += cost.Read(f)
	}
	if cost.TraceEnvelope != nil {
		total += cost.TraceEnvelope(gold)
	}
	return total
}

// coverage is the fraction of gold files the pack surfaced.
func coverage(gold, surfaced []string) float64 {
	g := distinct(gold)
	if len(g) == 0 {
		return 1.0
	}
	have := make(map[string]bool, len(surfaced))
	for _, f := range surfaced {
		have[f] = true
	}
	hit := 0
	for _, f := range g {
		if have[f] {
			hit++
		}
	}
	return float64(hit) / float64(len(g))
}

func distinct(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	var out []string
	for _, x := range xs {
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}

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
	fmt.Fprintf(&b, "# dex bench pack — modify-symbol working set\n\n")
	fmt.Fprintf(&b, "Cost to assemble S's working set: primitive multi-call path vs one ask(assemble) pack.\n\n")
	fmt.Fprintf(&b, "| metric | value |\n|---|---|\n")
	fmt.Fprintf(&b, "| tasks | %d |\n", r.NumTasks)
	fmt.Fprintf(&b, "| pack reached symbol | %d (%.1f%%) |\n", r.NumHit, r.ReachRate*100)
	fmt.Fprintf(&b, "| ripple recall (mean coverage) | %.1f%% |\n", r.MeanCoverage*100)
	fmt.Fprintf(&b, "| fully covered the ripple | %d (%.1f%% of reached) |\n", r.NumCovered, r.FullCoverRate*100)
	fmt.Fprintf(&b, "\n## Cost over reached tasks — assemble the ripple by hand vs one pack call\n\n")
	fmt.Fprintf(&b, "| workflow | mean calls | mean tokens |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| primitive | %.2f | %.0f |\n", r.MeanPrimitiveCalls, r.MeanPrimitiveTokens)
	fmt.Fprintf(&b, "| pack | %.2f | %.0f |\n", r.MeanPackCalls, r.MeanPackTokens)
	fmt.Fprintf(&b, "| **saved** | **%.1f%%** | **%.1f%%** |\n", r.CallsSavedPct*100, r.TokensSavedPct*100)
	fmt.Fprintf(&b, "\nSaved = 1 − pack/primitive. Ripple recall is the correctness floor: the cost win is "+
		"honest only insofar as the pack surfaces the working set the primitive path would assemble — "+
		"a cheap pack that returned less shows up as lower recall, not a hidden gain (AC #6).\n")
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

// Regressions compares this report against a committed reference. Coverage and
// reach must not fall by more than absTol; pack mean calls and mean tokens (the
// cost the pack is meant to hold down) must not rise by more than relTol.
func (r Report) Regressions(ref Report, absTol, relTol float64) []Regression {
	var regs []Regression
	if d := ref.MeanCoverage - r.MeanCoverage; d > absTol {
		regs = append(regs, Regression{"mean_coverage", ref.MeanCoverage, r.MeanCoverage})
	}
	if d := ref.FullCoverRate - r.FullCoverRate; d > absTol {
		regs = append(regs, Regression{"full_cover_rate", ref.FullCoverRate, r.FullCoverRate})
	}
	if d := ref.ReachRate - r.ReachRate; d > absTol {
		regs = append(regs, Regression{"reach_rate", ref.ReachRate, r.ReachRate})
	}
	if worse(ref.MeanPackCalls, r.MeanPackCalls, relTol) {
		regs = append(regs, Regression{"mean_pack_calls", ref.MeanPackCalls, r.MeanPackCalls})
	}
	if worse(ref.MeanPackTokens, r.MeanPackTokens, relTol) {
		regs = append(regs, Regression{"mean_pack_tokens", ref.MeanPackTokens, r.MeanPackTokens})
	}
	return regs
}

// worse reports whether now exceeds was by more than relTol (a fraction).
func worse(was, now, relTol float64) bool {
	if was <= 0 {
		return now > 0
	}
	return (now-was)/was > relTol
}
