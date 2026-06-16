package eval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Report is the aggregated eval output: mean NDCG@k, Recall@k and MRR over
// all golden queries.
type Report struct {
	K              int     `json:"k"`
	N              int     `json:"n"`
	MeanNDCG       float64 `json:"mean_ndcg"`
	MeanRecall     float64 `json:"mean_recall"`
	MeanRecallPool float64 `json:"mean_recall_pool"` // mean recall@candidateK — pool-recall ceiling
	MRR            float64 `json:"mrr"`

	// ByType breaks the aggregate down by store.ClassifyQueryType bucket
	// (nl|symbol|architecture). The calibration uses this to see which lane
	// weighting helps which query class — the RRF weights are query-type
	// adaptive, so an aggregate win can hide a per-bucket regression. Bucket
	// sub-reports carry no Queries detail and are not themselves bucketed.
	ByType map[string]Report `json:"by_type,omitempty"`

	// Queries is the per-query detail, included in JSON output for
	// debugging but omitted from the Markdown summary.
	Queries []QueryResult `json:"queries,omitempty"`

	// Manifest records the experiment identity (golden corpus, lane, model,
	// fusion config, k, HEADs) under which these metrics were produced. It is
	// a pointer so reports written before manifests existed unmarshal as nil;
	// `--check` gates on manifest compatibility when both sides carry one.
	Manifest *EvalManifest `json:"manifest,omitempty"`
}

// aggregate fills the scalar means of a Report from results. It does NOT
// build the per-type breakdown, so it is safe to call on a single bucket.
func aggregate(results []QueryResult, k int) Report {
	rep := Report{K: k, N: len(results)}
	if len(results) == 0 {
		return rep
	}
	var ndcg, recall, recallPool, rr float64
	for _, r := range results {
		ndcg += r.NDCG
		recall += r.Recall
		recallPool += r.RecallPool
		rr += r.RR
	}
	n := float64(len(results))
	rep.MeanNDCG = ndcg / n
	rep.MeanRecall = recall / n
	rep.MeanRecallPool = recallPool / n
	rep.MRR = rr / n
	return rep
}

// Compute aggregates per-query results into a Report, including the
// per-query-type breakdown.
func Compute(results []QueryResult, k int) Report {
	rep := aggregate(results, k)
	rep.Queries = results
	if len(results) == 0 {
		return rep
	}
	buckets := make(map[string][]QueryResult)
	for _, r := range results {
		t := r.Type
		if t == "" {
			t = "unknown"
		}
		buckets[t] = append(buckets[t], r)
	}
	rep.ByType = make(map[string]Report, len(buckets))
	for t, rs := range buckets {
		rep.ByType[t] = aggregate(rs, k) // no Queries, no nested ByType
	}
	return rep
}

// Regression is a single metric that dropped beyond tolerance versus a
// reference report. CILow/CIHigh and Boot are set only by the bootstrap
// comparator (BootstrapRegressions); the fixed-tolerance path leaves them zero.
type Regression struct {
	Metric        string
	Was, Now      float64
	CILow, CIHigh float64 // bootstrap CI on the mean per-query delta (Boot only)
	Boot          bool    // true when produced by the paired-bootstrap comparator
}

func (r Regression) String() string {
	return fmt.Sprintf("%s regressed: was %.3f, now %.3f (delta %.3f)%s", r.Metric, r.Was, r.Now, r.Was-r.Now, r.bootstrapNote())
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
		{"RecallPool@candidateK", ref.MeanRecallPool, rep.MeanRecallPool},
		{"MRR", ref.MRR, rep.MRR},
	} {
		if m.was-m.now > tol {
			out = append(out, Regression{Metric: m.name, Was: m.was, Now: m.now})
		}
	}
	return out
}

// ByTypeRegressions returns per-query-type bucket regressions beyond tol,
// considering only buckets present in BOTH reports with at least minBucket
// queries on each side (small buckets are too noisy to gate on). Each
// regression's Metric is prefixed with the bucket name. The second return
// value lists buckets present in exactly one report — a query-corpus change
// that the QuerySetSHA256 manifest gate should already have caught, surfaced
// here as a warning rather than a silent skip.
//
// This closes the gap where an aggregate win hides a per-bucket regression:
// the report computes ByType but Regressions only looked at the global means.
func (rep Report) ByTypeRegressions(ref Report, tol float64, minBucket int) (regs []Regression, bucketDelta []string) {
	seen := make(map[string]bool)
	for t := range rep.ByType {
		seen[t] = true
	}
	for t := range ref.ByType {
		seen[t] = true
	}
	// Stable order for deterministic output.
	types := make([]string, 0, len(seen))
	for t := range seen {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		now, inNow := rep.ByType[t]
		was, inRef := ref.ByType[t]
		if !inNow || !inRef {
			where := "now"
			if inRef {
				where = "ref"
			}
			bucketDelta = append(bucketDelta, fmt.Sprintf("%q present only in %s", t, where))
			continue
		}
		if now.N < minBucket || was.N < minBucket {
			continue
		}
		for _, r := range now.Regressions(was, tol) {
			r.Metric = t + "/" + r.Metric
			regs = append(regs, r)
		}
	}
	return regs, bucketDelta
}

// JSON serializes the report as indented JSON.
func (rep Report) JSON() ([]byte, error) {
	return json.MarshalIndent(rep, "", "  ")
}

// Markdown renders a compact summary table (per-query detail omitted).
func (rep Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Code-Retrieval Eval — k=%d, n=%d queries\n\n", rep.K, rep.N)
	if m := rep.Manifest; m != nil {
		stale := ""
		if StaleGolden(m.GoldenHead, m.RepoHead) {
			stale = " ⚠ STALE"
		}
		fmt.Fprintf(&b, "_lane=%s mode=%s model=%s/dim=%d fusion=%s α=%.2f graph=%.2f — golden HEAD %s vs repo HEAD %s%s_\n\n",
			m.Lane, m.GoldenMode, m.EmbedModel, m.EmbedDim, m.FusionMode, m.FusionAlpha, m.GraphWeight,
			shortHead(m.GoldenHead), shortHead(m.RepoHead), stale)
	}
	b.WriteString("| Metric | Value |\n")
	b.WriteString("|--------|-------|\n")
	fmt.Fprintf(&b, "| NDCG@%d   | %.3f |\n", rep.K, rep.MeanNDCG)
	fmt.Fprintf(&b, "| Recall@%d | %.3f |\n", rep.K, rep.MeanRecall)
	fmt.Fprintf(&b, "| RecallPool@cK | %.3f |\n", rep.MeanRecallPool)
	fmt.Fprintf(&b, "| MRR      | %.3f |\n", rep.MRR)
	if len(rep.ByType) > 0 {
		b.WriteString("\n### By query type\n\n")
		b.WriteString("| Type | n | NDCG@k | Recall@k | RecallPool | MRR |\n")
		b.WriteString("|------|---|--------|----------|------------|-----|\n")
		// Stable order so diffs across runs are readable.
		for _, t := range []string{"nl", "symbol", "architecture", "unknown"} {
			sub, ok := rep.ByType[t]
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "| %s | %d | %.3f | %.3f | %.3f | %.3f |\n",
				t, sub.N, sub.MeanNDCG, sub.MeanRecall, sub.MeanRecallPool, sub.MRR)
		}
	}
	return b.String()
}
