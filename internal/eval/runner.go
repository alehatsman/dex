package eval

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/store"
	"golang.org/x/sync/errgroup"
)

// defaultEvalConcurrency bounds how many queries eval.Run scores in parallel.
// The work is latency-bound (embed/rerank round-trips), so a modest fan-out
// saturates the backend GPU that a serial loop leaves idle.
const defaultEvalConcurrency = 8

// evalConcurrency returns the query-scoring fan-out, overridable via
// DEX_EVAL_CONCURRENCY (values < 1 fall back to the default).
func evalConcurrency() int {
	if v, err := strconv.Atoi(os.Getenv("DEX_EVAL_CONCURRENCY")); err == nil && v >= 1 {
		return v
	}
	return defaultEvalConcurrency
}

// QueryResult holds per-query retrieval scores.
type QueryResult struct {
	ID          string   `json:"id"`
	Query       string   `json:"query"`
	Relevant    []string `json:"relevant"`
	RankedFiles []string `json:"ranked_files"` // top unique files in rank order
	NDCG        float64  `json:"ndcg"`
	Recall      float64  `json:"recall"`
	RecallPool  float64  `json:"recall_pool"` // recall@candidateK — pool-recall ceiling (see below)
	RR          float64  `json:"rr"`          // reciprocal rank
	Type        string   `json:"type"`        // store.ClassifyQueryType bucket: nl|symbol|architecture
}

// Run scores the live Search path against the golden set using an already
// open project store. For each query it embeds the query text, retrieves a
// pool of hits, collapses them to a ranked list of unique source files, and
// computes NDCG@k / Recall@k / reciprocal-rank against the relevant set.
func Run(ctx context.Context, em embed.Embedder, st *store.Store, gs GoldenSet, k int) ([]QueryResult, error) {
	return RunWithRewrite(ctx, em, st, gs, k, nil)
}

// Rewrite optionally transforms a query before scoring, returning the text to
// embed (vector lane) and the text for the BM25/FTS leg. Used to A/B query-
// side expansion (#252); both default to the raw query when rw is nil.
type Rewrite func(ctx context.Context, query string) (embedText, ftsText string)

// RunWithRewrite is Run with an optional per-query Rewrite applied before
// scoring. See Run for the scoring contract.
func RunWithRewrite(ctx context.Context, em embed.Embedder, st *store.Store, gs GoldenSet, k int, rw Rewrite) ([]QueryResult, error) {
	if len(gs.Queries) == 0 {
		return nil, fmt.Errorf("eval: golden set is empty")
	}

	embedTexts := make([]string, len(gs.Queries))
	ftsTexts := make([]string, len(gs.Queries))
	for i, q := range gs.Queries {
		embedTexts[i], ftsTexts[i] = q.Query, q.Query
	}
	// Apply the rewrite concurrently — each call is an independent model
	// round-trip, latency-bound like the scoring loop below.
	if rw != nil {
		eg, egctx := errgroup.WithContext(ctx)
		eg.SetLimit(evalConcurrency())
		for i, q := range gs.Queries {
			i, q := i, q
			eg.Go(func() error {
				embedTexts[i], ftsTexts[i] = rw(egctx, q.Query)
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, fmt.Errorf("eval: rewrite: %w", err)
		}
	}
	vecs := make([][]float32, len(embedTexts)) // nil slices → BM25-only lane when em == nil
	if em != nil {
		var err error
		vecs, err = em.Embed(ctx, embedTexts)
		if err != nil {
			return nil, fmt.Errorf("eval: embed queries: %w", err)
		}
	}

	results := make([]QueryResult, len(gs.Queries))
	// Score queries concurrently. Each Search is an independent read on a
	// pooled *sql.DB plus embed/rerank round-trips, so a serial loop is
	// latency-bound and leaves the GPU idle. A bounded errgroup saturates the
	// backend without unbounded fan-out; results[i] is written by a unique
	// index so the slice needs no synchronization. Tune via DEX_EVAL_CONCURRENCY.
	eg, egctx := errgroup.WithContext(ctx)
	eg.SetLimit(evalConcurrency())
	for i, q := range gs.Queries {
		i, q := i, q
		eg.Go(func() error {
			// Pool wider than k so unique-file collapse still yields k files
			// even when several top hits share a file.
			pool := k * 5
			if pool < 30 {
				pool = 30
			}
			hits, err := st.Search(egctx, vecs[i], ftsTexts[i], pool)
			if err != nil {
				return fmt.Errorf("eval: search q%d (%s): %w", i, q.ID, err)
			}

			// For blast-radius queries the anchor file is the "given" — exclude it
			// from the ranked list so it neither earns credit nor occupies a slot.
			ranked := uniqueFiles(hits, k, q.Anchor)
			// recall@candidateK: collapse the SAME hits at the full pool depth. This
			// is the pool-recall ceiling — the fraction of relevant files present
			// anywhere in the candidate pool the fusion/rerank stage sees. Top-k
			// Recall can never exceed it, so the gap (RecallPool − Recall) isolates
			// a ranking failure (doc was in the pool, fusion buried it) from a
			// retrieval failure (doc never made the pool). No extra search.
			rankedPool := uniqueFiles(hits, pool, q.Anchor)
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
				RecallPool:  RecallAtK(rankedPool, relevant, pool),
				RR:          MRR(ranked, relevant),
				Type:        store.ClassifyQueryType(q.Query),
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// uniqueFiles collapses hits to the first-seen (best-ranked) occurrence of
// each source file and returns the top limit files in rank order. The exclude
// path (a blast-radius anchor, "" for none) is dropped so the query's own
// file never counts.
func uniqueFiles(hits []store.Hit, limit int, exclude string) []string {
	seen := make(map[string]bool)
	var files []string
	for _, h := range hits {
		if exclude != "" && h.Path == exclude {
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
