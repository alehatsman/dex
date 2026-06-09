package locomo

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/store"
)

// QuestionResult holds scores for one question.
type QuestionResult struct {
	QuestionID    string
	Category      string
	Question      string
	Answer        string
	TopK          []store.Hit
	RecallAtK     bool    // any top-k memory Contains(answer)
	BestTokenF1   float64 // max TokenF1 over top-k memories
	AnyExactMatch bool    // any top-k memory ExactMatch(answer)

	// Token economy: how many tokens in the retrieved top-k vs the full transcript.
	TranscriptTokens int
	RetrievedTokens  int
}

// Run ingests the dataset into a temporary store, then for each question
// embeds the question text, retrieves top-k, and scores all three metrics.
// The caller is responsible for the embed client's lifecycle.
func Run(ctx context.Context, em embed.Embedder, d Dataset, k int) ([]QuestionResult, error) {
	// One temp store per run — isolated, no project index contamination.
	dir, err := os.MkdirTemp("", "dex-locomo-*")
	if err != nil {
		return nil, fmt.Errorf("locomo: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	st, err := store.OpenWith(ctx, filepath.Join(dir, "locomo.db"), store.Options{})
	if err != nil {
		return nil, fmt.Errorf("locomo: open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	// Build a lookup: conv_id → conversation so we can count transcript tokens.
	convByID := make(map[string]*Conversation, len(d.Conversations))
	for i := range d.Conversations {
		convByID[d.Conversations[i].ID] = &d.Conversations[i]
	}

	// Ingest every turn from every conversation as a PendingChunk.
	var allTexts []string
	type turnRef struct {
		convID string
		idx    int
	}
	var refs []turnRef
	for _, c := range d.Conversations {
		for i, t := range c.Turns {
			allTexts = append(allTexts, t.Speaker+": "+t.Text)
			refs = append(refs, turnRef{c.ID, i})
		}
	}
	vecs, err := em.Embed(ctx, allTexts)
	if err != nil {
		return nil, fmt.Errorf("locomo: embed turns: %w", err)
	}

	chunks := make([]store.PendingChunk, len(allTexts))
	for i, text := range allTexts {
		chunks[i] = store.PendingChunk{
			Path:       refs[i].convID,
			Kind:       "turn",
			Name:       fmt.Sprintf("turn_%d", refs[i].idx),
			StartLine:  refs[i].idx + 1,
			EndLine:    refs[i].idx + 1,
			ContentSHA: fnvHex(text),
			Content:    text,
			Vec:        vecs[i],
		}
	}
	if err := st.UpsertMany(ctx, chunks, time.Now()); err != nil {
		return nil, fmt.Errorf("locomo: upsert turns: %w", err)
	}

	// Score each question.
	questions := d.Questions()
	qTexts := make([]string, len(questions))
	for i, q := range questions {
		qTexts[i] = q.Text
	}
	qVecs, err := em.Embed(ctx, qTexts)
	if err != nil {
		return nil, fmt.Errorf("locomo: embed questions: %w", err)
	}

	results := make([]QuestionResult, len(questions))
	for i, q := range questions {
		hits, err := st.SearchFused(ctx, qVecs[i], q.Text, k)
		if err != nil {
			return nil, fmt.Errorf("locomo: search q%d: %w", i, err)
		}

		r := QuestionResult{
			QuestionID: q.ID,
			Category:   q.Category,
			Question:   q.Text,
			Answer:     q.Answer,
			TopK:       hits,
		}

		// Compute transcript token count for this conversation.
		if conv, ok := convByID[q.ConvID]; ok {
			for _, t := range conv.Turns {
				r.TranscriptTokens += approxTokens(t.Speaker + ": " + t.Text)
			}
		}

		bestF1 := 0.0
		for _, h := range hits {
			r.RetrievedTokens += approxTokens(h.Content)
			if Contains(h.Content, q.Answer) {
				r.RecallAtK = true
			}
			if ExactMatch(h.Content, q.Answer) {
				r.AnyExactMatch = true
			}
			if f1 := TokenF1(h.Content, q.Answer); f1 > bestF1 {
				bestF1 = f1
			}
		}
		r.BestTokenF1 = bestF1
		results[i] = r
	}
	return results, nil
}

func fnvHex(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%016x", h.Sum64())
}

// approxTokens estimates token count as whitespace-token count / 0.75 (rough
// BPE approximation used throughout lean-ctx).
func approxTokens(s string) int {
	n := len(strings.Fields(s))
	return int(float64(n) / 0.75)
}
