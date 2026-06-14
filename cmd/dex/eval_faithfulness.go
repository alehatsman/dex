package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/eval"
	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/store"
	"golang.org/x/sync/errgroup"
)

// faithfulnessEvidenceCap bounds the evidence text fed to the chat model per
// query, matching the spirit of the ask path's evidence cap.
const faithfulnessEvidenceCap = 12000

// faithfulnessConcurrency bounds parallel answer synthesis. Each call is a chat
// round-trip (GPU-bound); a small fan-out keeps the backend busy without
// flooding it.
const faithfulnessConcurrency = 4

// runFaithfulnessEval synthesizes an `ask`-style answer for each golden query
// from the retrieved evidence, then scores how well each answer is grounded in
// that evidence (#550). It is the answer-quality gate that retrieval metrics
// (NDCG/Recall/RR) cannot provide. Requires a chat model; without one every
// synthesis is empty and the run errors with a needs-chat hint.
func runFaithfulnessEval(ctx context.Context, st *store.Store, em embed.Embedder, gs eval.GoldenSet, k int, outputFmt, checkPath string) error {
	client := newChatClient()

	// Embed all queries up front (nil vectors → BM25-only lane when em == nil).
	embedTexts := make([]string, len(gs.Queries))
	for i, q := range gs.Queries {
		embedTexts[i] = q.Query
	}
	vecs := make([][]float32, len(gs.Queries))
	if em != nil {
		var err error
		vecs, err = em.Embed(ctx, embedTexts)
		if err != nil {
			return fmt.Errorf("embed queries: %w", err)
		}
	}

	var cache retrieve.AnswerCache
	results := make([]eval.FaithfulnessResult, len(gs.Queries))
	synthErrs := make([]error, len(gs.Queries))
	answered := make([]bool, len(gs.Queries))

	eg, egctx := errgroup.WithContext(ctx)
	eg.SetLimit(faithfulnessConcurrency)
	for i, q := range gs.Queries {
		i, q := i, q
		eg.Go(func() error {
			pool := k * 5
			if pool < 30 {
				pool = 30
			}
			hits, err := st.Search(egctx, vecs[i], q.Query, pool)
			if err != nil {
				synthErrs[i] = fmt.Errorf("search: %w", err)
				results[i] = eval.FaithfulnessResult{ID: q.ID, Score: 1.0}
				return nil
			}
			evidence := buildFaithfulnessEvidence(hits)
			answer, _, hintErr := retrieve.SynthesizeAnswer(egctx, client, &cache, "", q.Query, evidence, func(string) {})
			if hintErr != nil {
				synthErrs[i] = hintErr
			}
			if strings.TrimSpace(answer) != "" {
				answered[i] = true
			}
			results[i] = eval.ScoreFaithfulness(q.ID, answer, evidence, eval.DefaultFaithfulnessOpts())
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	// If nothing was answered, the chat backend is almost certainly missing or
	// unreachable — fail loudly rather than report a vacuous 1.0.
	anyAnswered := false
	for _, a := range answered {
		if a {
			anyAnswered = true
			break
		}
	}
	if !anyAnswered {
		hint := "no chat model produced an answer"
		for _, e := range synthErrs {
			if e != nil {
				hint = e.Error()
				break
			}
		}
		return fmt.Errorf("faithfulness: %s — set DEX_CHAT_URL/DEX_CHAT_MODEL (status=needs-chat)", hint)
	}

	rep := eval.AggregateFaithfulness(results, eval.DefaultFaithfulnessOpts())
	switch outputFmt {
	case "json":
		out, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(rep.Markdown())
	}

	if checkPath != "" {
		if err := checkFaithfulnessRegression(rep, checkPath); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "dex bench eval: faithfulness regression check passed")
	}
	return nil
}

// buildFaithfulnessEvidence renders retrieved hits into the evidence text the
// answer should be grounded in: a `path (Lstart-Lend)` header per hit followed
// by its content, capped at faithfulnessEvidenceCap bytes.
func buildFaithfulnessEvidence(hits []store.Hit) string {
	var b strings.Builder
	for _, h := range hits {
		if b.Len() >= faithfulnessEvidenceCap {
			break
		}
		fmt.Fprintf(&b, "%s (L%d-%d)\n", h.Path, h.StartLine, h.EndLine)
		b.WriteString(h.Content)
		b.WriteString("\n\n")
	}
	s := b.String()
	if len(s) > faithfulnessEvidenceCap {
		s = s[:faithfulnessEvidenceCap]
	}
	return s
}

// checkFaithfulnessRegression fails if mean faithfulness dropped by more than
// the tolerance versus a committed reference report.
func checkFaithfulnessRegression(current eval.FaithfulnessReport, refPath string) error {
	data, err := os.ReadFile(refPath)
	if err != nil {
		return fmt.Errorf("read reference %q: %w", refPath, err)
	}
	var ref eval.FaithfulnessReport
	if err := json.Unmarshal(data, &ref); err != nil {
		return fmt.Errorf("parse reference: %w", err)
	}
	const tol = 0.02
	if current.MeanScore < ref.MeanScore-tol {
		return fmt.Errorf("faithfulness regressed: mean %.3f < reference %.3f - %.2f",
			current.MeanScore, ref.MeanScore, tol)
	}
	return nil
}
