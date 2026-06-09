package eval

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Report is the aggregated eval output: mean NDCG@k, Recall@k and MRR over
// all golden queries.
type Report struct {
	K          int     `json:"k"`
	N          int     `json:"n"`
	MeanNDCG   float64 `json:"mean_ndcg"`
	MeanRecall float64 `json:"mean_recall"`
	MRR        float64 `json:"mrr"`

	// Queries is the per-query detail, included in JSON output for
	// debugging but omitted from the Markdown summary.
	Queries []QueryResult `json:"queries,omitempty"`
}

// Compute aggregates per-query results into a Report.
func Compute(results []QueryResult, k int) Report {
	rep := Report{K: k, N: len(results), Queries: results}
	if len(results) == 0 {
		return rep
	}
	var ndcg, recall, rr float64
	for _, r := range results {
		ndcg += r.NDCG
		recall += r.Recall
		rr += r.RR
	}
	n := float64(len(results))
	rep.MeanNDCG = ndcg / n
	rep.MeanRecall = recall / n
	rep.MRR = rr / n
	return rep
}

// Regression is a single metric that dropped beyond tolerance versus a
// reference report.
type Regression struct {
	Metric   string
	Was, Now float64
}

func (r Regression) String() string {
	return fmt.Sprintf("%s regressed: was %.3f, now %.3f (delta %.3f)", r.Metric, r.Was, r.Now, r.Was-r.Now)
}

// Regressions returns the metrics (NDCG@k, Recall@k, MRR) that dropped by
// more than tol versus ref. An empty slice means the report did not regress.
func (rep Report) Regressions(ref Report, tol float64) []Regression {
	var out []Regression
	for _, m := range []struct {
		name     string
		was, now float64
	}{
		{"NDCG@k", ref.MeanNDCG, rep.MeanNDCG},
		{"Recall@k", ref.MeanRecall, rep.MeanRecall},
		{"MRR", ref.MRR, rep.MRR},
	} {
		if m.was-m.now > tol {
			out = append(out, Regression{Metric: m.name, Was: m.was, Now: m.now})
		}
	}
	return out
}

// JSON serializes the report as indented JSON.
func (rep Report) JSON() ([]byte, error) {
	return json.MarshalIndent(rep, "", "  ")
}

// Markdown renders a compact summary table (per-query detail omitted).
func (rep Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Code-Retrieval Eval — k=%d, n=%d queries\n\n", rep.K, rep.N)
	b.WriteString("| Metric | Value |\n")
	b.WriteString("|--------|-------|\n")
	fmt.Fprintf(&b, "| NDCG@%d   | %.3f |\n", rep.K, rep.MeanNDCG)
	fmt.Fprintf(&b, "| Recall@%d | %.3f |\n", rep.K, rep.MeanRecall)
	fmt.Fprintf(&b, "| MRR      | %.3f |\n", rep.MRR)
	return b.String()
}
