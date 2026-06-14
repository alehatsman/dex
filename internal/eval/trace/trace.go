// Package trace scores the indexed call graph against a hand-verified gold
// set of callers/callees, reporting per-(repo, language) precision/recall.
//
// This is the measurement instrument #468 requires before swapping the
// per-language tree-sitter extractors for a uniform substrate: the existing
// nav-bench golden is Go-only (mined from dex's own git history), so it cannot
// detect a cross-language trace regression. A trace gold set is a small,
// committable JSON file of probes — each a symbol + direction + the verified
// set of peers on the other end of its `calls` edges — that the scorer runs
// against a loaded graphquery.View and grades by set overlap.
//
// The scorer never touches the store or embedder beyond the caller loading the
// View; it is intentionally language-agnostic — the same probe shape grades Go
// (go/types edges) and Python/Rust/TS (tree-sitter edges) identically, so a
// before/after comparison across the #468 swap is apples-to-apples.
package trace

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// Direction selects which side of a node's `calls` edges a probe grades.
const (
	DirectionCallers = "callers" // incoming calls: who calls Symbol
	DirectionCallees = "callees" // outgoing calls: what Symbol calls
)

// Probe is one hand-verified expectation: resolve Symbol to graph nodes, walk
// its `calls` edges in Direction, and compare the peer set to Expected.
type Probe struct {
	// Symbol is the input name to resolve, in any shape ResolveCallTargets
	// accepts: bare ("Foo"), receiver-qualified ("(*T).Foo"), or
	// package-tail-qualified ("pkg.Foo").
	Symbol string `json:"symbol"`
	// Package optionally disambiguates when Symbol is defined in several
	// packages; empty means no filter.
	Package string `json:"package,omitempty"`
	// Direction is DirectionCallers or DirectionCallees.
	Direction string `json:"direction"`
	// Expected is the verified set of peer qualified names. Order-independent;
	// scored as a set.
	Expected []string `json:"expected"`
}

// Gold is a committable trace gold set, pinned to a commit so a re-verify can
// tell drift (the repo moved) from a real regression (the extractor changed).
type Gold struct {
	Repo   string  `json:"repo"`           // repo basename, informational
	Lang   string  `json:"lang"`           // primary language, for rollups
	Commit string  `json:"commit"`         // pinned commit the probes were verified against
	Probes []Probe `json:"probes"`         //
	Note   string  `json:"note,omitempty"` // free-form provenance / how verified
}

// ProbeResult is the graded outcome of one probe.
type ProbeResult struct {
	Probe     Probe    `json:"probe"`
	Got       []string `json:"got"`       // peer qualified names the graph returned (sorted)
	Resolved  bool     `json:"resolved"`  // did Symbol resolve to at least one node
	TP        int      `json:"tp"`        // |Got ∩ Expected|
	FP        int      `json:"fp"`        // |Got \ Expected|
	FN        int      `json:"fn"`        // |Expected \ Got|
	Precision float64  `json:"precision"` // TP / (TP+FP); 1.0 when nothing returned and nothing expected
	Recall    float64  `json:"recall"`    // TP / (TP+FN); 1.0 when nothing expected
	F1        float64  `json:"f1"`        //
}

// Report aggregates probe results for one gold set. Aggregates are macro
// (mean over probes) so a probe with many expected peers doesn't dominate one
// with few — every probe is an equally weighted question about resolution.
type Report struct {
	Repo           string        `json:"repo"`
	Lang           string        `json:"lang"`
	Set            string        `json:"set,omitempty"` // gold-set label (e.g. file basename) when a repo has several
	Probes         int           `json:"probes"`
	Unresolved     int           `json:"unresolved"`      // probes whose Symbol resolved to no node
	MacroPrecision float64       `json:"macro_precision"` //
	MacroRecall    float64       `json:"macro_recall"`    //
	MacroF1        float64       `json:"macro_f1"`        //
	Results        []ProbeResult `json:"results"`
}

// Score grades every probe in gold against view and returns the aggregated
// report. An unresolved Symbol scores zero precision/recall (it is a real
// failure of the graph, not a skip) and is also counted in Unresolved so a
// reviewer can separate "resolved but wrong edges" from "symbol missing".
func Score(view *graphquery.View, gold Gold) Report {
	rep := Report{Repo: gold.Repo, Lang: gold.Lang, Probes: len(gold.Probes)}
	var sumP, sumR, sumF float64
	for _, p := range gold.Probes {
		res := scoreProbe(view, p)
		if !res.Resolved {
			rep.Unresolved++
		}
		sumP += res.Precision
		sumR += res.Recall
		sumF += res.F1
		rep.Results = append(rep.Results, res)
	}
	if n := float64(len(gold.Probes)); n > 0 {
		rep.MacroPrecision = sumP / n
		rep.MacroRecall = sumR / n
		rep.MacroF1 = sumF / n
	}
	return rep
}

func scoreProbe(view *graphquery.View, p Probe) ProbeResult {
	targets := graphquery.ResolveCallTargets(view, p.Symbol, p.Package)
	got := peers(view, targets, p.Direction == DirectionCallers)

	gotSet := toSet(got)
	wantSet := toSet(p.Expected)
	var tp, fp int
	for g := range gotSet {
		if wantSet[g] {
			tp++
		} else {
			fp++
		}
	}
	fn := len(wantSet) - tp

	res := ProbeResult{
		Probe:    p,
		Got:      sortedKeys(gotSet),
		Resolved: len(targets) > 0,
		TP:       tp,
		FP:       fp,
		FN:       fn,
	}
	res.Precision = ratio(tp, tp+fp, len(gotSet) == 0 && len(wantSet) == 0)
	res.Recall = ratio(tp, tp+fn, len(wantSet) == 0)
	res.F1 = f1(res.Precision, res.Recall)
	return res
}

// peers walks the `calls` edges of every resolved target and returns the
// deduplicated qualified names on the far side. callers=true follows incoming
// edges (EdgesByDst → SrcID); callers=false follows outgoing (EdgesBySrc →
// DstID). Targets are unioned, mirroring the production callers/callees tool
// which aggregates across all interpretations of an ambiguous name.
func peers(view *graphquery.View, targets []graphquery.Node, callers bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range targets {
		var edges []graphquery.Edge
		if callers {
			edges = view.EdgesByDst[t.ID]
		} else {
			edges = view.EdgesBySrc[t.ID]
		}
		for _, e := range edges {
			if e.Kind != graph.EdgeCalls {
				continue
			}
			peerID := e.DstID
			if callers {
				peerID = e.SrcID
			}
			n, ok := view.NodesByID[peerID]
			if !ok {
				continue
			}
			qn := peerKey(n)
			if !seen[qn] {
				seen[qn] = true
				out = append(out, qn)
			}
		}
	}
	return out
}

// peerKey is the comparison key for a peer node. dex's Go graph stores
// QualifiedName as the BARE symbol name (package tracked separately), so a bare
// key collides across packages and — fatally for a cross-language instrument —
// across languages (every language has an `init`, `new`, `get`). We disambiguate
// with the last segment of the package path: "trace.scoreProbe",
// "graphquery.ResolveCallTargets". Nodes with no package path (e.g. the
// synthetic test view) fall back to the bare qualified name, so gold authored
// against single-package fixtures still reads naturally.
func peerKey(n graphquery.Node) string {
	name := n.QualifiedName
	if name == "" {
		name = n.Name
	}
	if n.PackagePath == "" {
		return name
	}
	seg := n.PackagePath
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	return seg + "." + name
}

// ratio returns num/den, or 1.0 for the vacuous case (whenTrue) where there is
// nothing to be right or wrong about (e.g. recall when Expected is empty).
func ratio(num, den int, vacuous bool) float64 {
	if vacuous {
		return 1.0
	}
	if den == 0 {
		return 0.0
	}
	return float64(num) / float64(den)
}

func f1(p, r float64) float64 {
	if p+r == 0 {
		return 0.0
	}
	return 2 * p * r / (p + r)
}

func toSet(xs []string) map[string]bool {
	s := make(map[string]bool, len(xs))
	for _, x := range xs {
		s[x] = true
	}
	return s
}

func sortedKeys(s map[string]bool) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LoadGold reads a trace gold set from a JSON file.
func LoadGold(path string) (Gold, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Gold{}, err
	}
	var g Gold
	if err := json.Unmarshal(data, &g); err != nil {
		return Gold{}, fmt.Errorf("parse trace gold %q: %w", path, err)
	}
	return g, nil
}
