package mcp

import (
	"strings"

	"github.com/alehatsman/dex/internal/store"
)

// ecsRerank applies Entropic Context Shaping (ECS) scoring followed by
// Marginal Information Gain (MIG) greedy selection to reorder hits.
//
// ECS composite score:
//
//	ECS(h) = 0.5×task_relevance + 0.3×graph_centrality + 0.2×info_density
//
// MIG greedy selection (λ=0.6) then reorders the hits so each successive
// pick maximises relevance while minimising redundancy with already-selected
// chunks (measured via Jaccard similarity on word tokens).
//
// When taskKWs is empty, task_relevance defaults to 0.5 (neutral) so
// graph_centrality and info_density still shape the ranking.
//
// No-op when len(hits) <= 1.
func ecsRerank(hits []store.Hit, taskKWs []string) []store.Hit {
	if len(hits) <= 1 {
		return hits
	}

	// Compute max graph edges across all candidates for normalisation.
	maxEdges := 1
	for _, h := range hits {
		if e := h.InDegree + h.OutDegree; e > maxEdges {
			maxEdges = e
		}
	}

	type scored struct {
		hit   store.Hit
		ecs   float32
		words map[string]struct{}
	}

	pool := make([]scored, len(hits))
	for i, h := range hits {
		pool[i] = scored{
			hit:   h,
			ecs:   ecsScore(h, taskKWs, maxEdges),
			words: wordTokens(h.Content + " " + h.Name),
		}
	}

	// MIG greedy selection (λ=0.6).
	const lambda = 0.6
	selected := make([]scored, 0, len(pool))
	used := make([]bool, len(pool))

	for len(selected) < len(pool) {
		bestIdx := -1
		var bestMIG float32 = -1e9

		for i, s := range pool {
			if used[i] {
				continue
			}
			redundancy := float32(0)
			for _, sel := range selected {
				if j := jaccardSim(s.words, sel.words); j > redundancy {
					redundancy = j
				}
			}
			mig := s.ecs - lambda*redundancy
			if bestIdx < 0 || mig > bestMIG {
				bestMIG = mig
				bestIdx = i
			}
		}

		used[bestIdx] = true
		selected = append(selected, pool[bestIdx])
	}

	out := make([]store.Hit, len(selected))
	for i, s := range selected {
		out[i] = s.hit
	}
	return out
}

// ecsScore computes the ECS composite for a single hit.
func ecsScore(h store.Hit, taskKWs []string, maxEdges int) float32 {
	const (
		wTask    = 0.5
		wGraph   = 0.3
		wDensity = 0.2
	)
	return wTask*taskRelevance(h.Content, h.Name, taskKWs) +
		wGraph*graphCentrality(h.InDegree, h.OutDegree, maxEdges) +
		wDensity*uniqueWordRatio(h.Content)
}

// taskRelevance returns the fraction of taskKWs that appear in the chunk
// text (content + symbol name, lowercased). Returns 0.5 when taskKWs is
// empty so the term stays neutral in the composite.
func taskRelevance(content, name string, taskKWs []string) float32 {
	if len(taskKWs) == 0 {
		return 0.5
	}
	haystack := strings.ToLower(content + " " + name)
	hits := 0
	for _, kw := range taskKWs {
		if strings.Contains(haystack, kw) {
			hits++
		}
	}
	return float32(hits) / float32(len(taskKWs))
}

// graphCentrality returns the normalised edge count for a chunk's file,
// clamped to [0, 1]. Zero when no graph data is available (both degrees 0).
func graphCentrality(inDeg, outDeg, maxEdges int) float32 {
	if maxEdges <= 0 {
		return 0
	}
	return float32(inDeg+outDeg) / float32(maxEdges)
}

// uniqueWordRatio returns unique_tokens/total_tokens for the text,
// in [0, 1]. Returns 0 on empty input. High ratio → information-dense;
// low ratio → repetitive/boilerplate.
func uniqueWordRatio(text string) float32 {
	words := strings.Fields(text)
	if len(words) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, len(words))
	for _, w := range words {
		seen[strings.ToLower(w)] = struct{}{}
	}
	return float32(len(seen)) / float32(len(words))
}

// wordTokens builds a lowercase word-token set for Jaccard computation.
func wordTokens(text string) map[string]struct{} {
	words := strings.Fields(text)
	out := make(map[string]struct{}, len(words))
	for _, w := range words {
		out[strings.ToLower(w)] = struct{}{}
	}
	return out
}

// jaccardSim returns |A∩B| / |A∪B| for two word-token sets, in [0, 1].
func jaccardSim(a, b map[string]struct{}) float32 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter := 0
	for w := range a {
		if _, ok := b[w]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float32(inter) / float32(union)
}

// extractTaskKWs tokenizes a session task string into lowercase keywords
// (length ≥ 3, skipping common stop words).
func extractTaskKWs(task string) []string {
	stop := map[string]bool{
		"the": true, "and": true, "for": true, "with": true,
		"that": true, "this": true, "from": true, "are": true,
		"was": true, "not": true, "but": true, "have": true,
	}
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(task)) {
		w = strings.Trim(w, ".,;:!?\"'()[]")
		if len(w) < 3 || stop[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}
