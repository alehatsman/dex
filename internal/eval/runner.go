package eval

import (
	"context"
	"fmt"
	"strings"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/store"
)

// QueryResult holds per-query retrieval scores.
type QueryResult struct {
	ID          string   `json:"id"`
	Query       string   `json:"query"`
	Relevant    []string `json:"relevant"`
	RankedFiles []string `json:"ranked_files"` // top unique files in rank order
	NDCG        float64  `json:"ndcg"`
	Recall      float64  `json:"recall"`
	RR          float64  `json:"rr"` // reciprocal rank
}

// Run scores the live Search path against the golden set using an already
// open project store. For each query it embeds the query text, retrieves a
// pool of hits, collapses them to a ranked list of unique source files, and
// computes NDCG@k / Recall@k / reciprocal-rank against the relevant set.
//
// git_commit chunks are always dropped from the candidate hits: the query is
// derived from a commit subject, so the matching git_commit chunk (which
// contains that subject verbatim) would be a trivial leak. Summary chunks are
// dropped too by default — keepSummaries=true retains them, which is required
// when A/B-testing an index feature that produces summary chunks (otherwise a
// file surfaced via its summary is invisible and the feature looks like a
// no-op). Summaries carry no commit-subject leak (they derive from file
// content), so retaining them is safe.
func Run(ctx context.Context, em embed.Embedder, st *store.Store, gs GoldenSet, k int, keepSummaries bool) ([]QueryResult, error) {
	if len(gs.Queries) == 0 {
		return nil, fmt.Errorf("eval: golden set is empty")
	}

	texts := make([]string, len(gs.Queries))
	for i, q := range gs.Queries {
		texts[i] = q.Query
	}
	vecs, err := em.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("eval: embed queries: %w", err)
	}

	results := make([]QueryResult, len(gs.Queries))
	for i, q := range gs.Queries {
		// Pool wider than k so unique-file collapse still yields k files
		// even when several top hits share a file.
		pool := k * 5
		if pool < 30 {
			pool = 30
		}
		hits, err := st.Search(ctx, vecs[i], q.Query, pool)
		if err != nil {
			return nil, fmt.Errorf("eval: search q%d (%s): %w", i, q.ID, err)
		}

		ranked := uniqueFiles(hits, k, keepSummaries)
		relevant := make(map[string]bool, len(q.RelevantFiles))
		for _, f := range q.RelevantFiles {
			relevant[f] = true
		}

		results[i] = QueryResult{
			ID:          q.ID,
			Query:       q.Query,
			Relevant:    q.RelevantFiles,
			RankedFiles: ranked,
			NDCG:        NDCG(ranked, relevant, k),
			Recall:      RecallAtK(ranked, relevant, k),
			RR:          MRR(ranked, relevant),
		}
	}
	return results, nil
}

// uniqueFiles collapses hits to the first-seen (best-ranked) occurrence of
// each source file and returns the top limit files in rank order. git_commit
// chunks are always dropped (commit-subject leak); summary chunks are dropped
// unless keepSummaries is set.
func uniqueFiles(hits []store.Hit, limit int, keepSummaries bool) []string {
	seen := make(map[string]bool)
	var files []string
	for _, h := range hits {
		if strings.HasPrefix(h.Path, "git:") {
			continue
		}
		if !keepSummaries && chunk.IsSummaryKind(h.Kind) {
			continue
		}
		if seen[h.Path] {
			continue
		}
		seen[h.Path] = true
		files = append(files, h.Path)
		if len(files) >= limit {
			break
		}
	}
	return files
}
