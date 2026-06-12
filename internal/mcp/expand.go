package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/chat"
)

// Query-side expansion (#252): an opt-in, failure-soft pass that turns the
// raw question into extra retrieval terms before the lanes run. One small
// fast model (DEX_EXPAND_MODEL, e.g. qwen3:4b) produces, in a single call:
//
//   - keywords    → folded into the BM25/FTS text of the semantic lane
//                   (zero extra embedding cost — the documented biggest win)
//   - identifiers → appended to the symbol-lane candidates
//   - hyde        → a hypothetical answer passage embedded into the vector
//                   lane (only in expandFull mode, since it costs a GPU embed)
//
// The raw question always stays in every lane, so a fanciful generation is
// diluted by RRF rather than amplified. Any error, timeout, or empty result
// degrades silently to the un-expanded query.

// expandMode is the opt-in level for query expansion.
type expandMode string

const (
	expandOff  expandMode = "off"  // no expansion call (default)
	expandOn   expandMode = "on"   // keywords + identifiers (no extra embed)
	expandFull expandMode = "full" // expandOn + HyDE passage embedded into the vector lane
)

// expandTimeout caps the single expansion call. On expiry the lanes fall
// back to the raw question — expansion never blocks an answer.
const expandTimeout = 5 * time.Second

// queryExpansion is the structured result of one expansion pass. Every
// field is best-effort; an empty field means "no expansion for that lane".
type queryExpansion struct {
	Keywords    []string `json:"keywords"`
	Identifiers []string `json:"identifiers"`
	Hyde        string   `json:"hyde"`
}

func (e queryExpansion) empty() bool {
	return len(e.Keywords) == 0 && len(e.Identifiers) == 0 && strings.TrimSpace(e.Hyde) == ""
}

const expandSystemPrompt = `You expand a code-search query into retrieval terms for a codebase search engine. Output ONLY compact JSON on a single line, no prose, no markdown.
Schema: {"keywords":[string],"identifiers":[string],"hyde":string}
- keywords: 3-8 lexical terms or synonyms a developer would grep for (domain words, abbreviations, concept/file names). No stopwords, no duplicates.
- identifiers: 0-6 plausible code symbol names (functions, types, methods) that would implement this, in common naming styles (camelCase, PascalCase, snake_case). Bare names only, no parentheses or packages.
- hyde: one short sentence (<=40 words) describing the code that answers the query, phrased like a doc comment. Empty string if unsure.
Return strictly valid JSON.`

// resolveExpandMode maps a request field (possibly empty) to a concrete
// mode. An empty field defers to the server default; an unrecognised value
// is treated as off so a typo can never silently enable GPU work.
func resolveExpandMode(reqValue, serverDefault string) expandMode {
	v := strings.ToLower(strings.TrimSpace(reqValue))
	if v == "" {
		v = strings.ToLower(strings.TrimSpace(serverDefault))
	}
	switch expandMode(v) {
	case expandOn:
		return expandOn
	case expandFull:
		return expandFull
	default:
		return expandOff
	}
}

// expandQuery runs the expansion pass. It returns the zero queryExpansion
// (and never an error) when expansion is off, unconfigured, or fails —
// callers treat an empty result as "use the raw question".
func (s *Server) expandQuery(ctx context.Context, question string, mode expandMode) queryExpansion {
	if mode == expandOff || s.ExpandClient == nil || strings.TrimSpace(question) == "" {
		return queryExpansion{}
	}
	cctx, cancel := context.WithTimeout(ctx, expandTimeout)
	defer cancel()

	resp, err := s.ExpandClient.Generate(cctx, []chat.Message{
		{Role: "system", Content: expandSystemPrompt},
		{Role: "user", Content: question},
		// ReasoningEffort "none" disables the reasoning trace on thinking
		// models (qwen3.x via ollama) so the JSON lands in content, not a
		// separate reasoning channel — and keeps the call fast.
	}, chat.Options{Temperature: 0, MaxTokens: 256, ReasoningEffort: "none"})
	if err != nil {
		return queryExpansion{} // failure-soft
	}

	exp := parseExpansion(resp.Content)
	if mode != expandFull {
		exp.Hyde = "" // only pay the embed cost in full mode
	}
	return exp
}

// parseExpansion extracts a queryExpansion from a model reply that may be
// wrapped in a reasoning trace, markdown fences, or surrounding prose. It
// strips <think> blocks, isolates the outermost JSON object, and sanitises
// the fields. A reply it cannot parse yields the zero value.
func parseExpansion(raw string) queryExpansion {
	s := stripThink(raw)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return queryExpansion{}
	}
	var exp queryExpansion
	if err := json.Unmarshal([]byte(s[start:end+1]), &exp); err != nil {
		return queryExpansion{}
	}
	exp.Keywords = sanitizeTerms(exp.Keywords, 8)
	exp.Identifiers = sanitizeTerms(exp.Identifiers, 6)
	exp.Hyde = strings.TrimSpace(exp.Hyde)
	if len(exp.Hyde) > 400 {
		exp.Hyde = exp.Hyde[:400]
	}
	return exp
}

// stripThink removes a leading <think>...</think> reasoning block (qwen3 and
// similar) so JSON extraction sees only the answer. Tolerates an unclosed
// block by dropping everything up to the first '{' the caller finds.
func stripThink(s string) string {
	open := strings.Index(s, "<think>")
	if open < 0 {
		return s
	}
	if close := strings.Index(s, "</think>"); close > open {
		return s[:open] + s[close+len("</think>"):]
	}
	return s[:open]
}

// expandedFTSText folds expansion keywords and identifiers into the BM25/FTS
// text of the semantic lane. The raw question leads so its terms keep their
// weight; extra terms only widen recall. Costs nothing extra to embed.
func expandedFTSText(question string, exp queryExpansion) string {
	extra := append(append([]string{}, exp.Keywords...), exp.Identifiers...)
	if len(extra) == 0 {
		return question
	}
	return question + " " + strings.Join(extra, " ")
}

// expandedEmbedText returns the text fed to the embedder. It stays the raw
// question unless a HyDE passage is present (full mode), in which case the
// passage is appended so the vector leans toward answer-space.
func expandedEmbedText(question string, exp queryExpansion) string {
	if strings.TrimSpace(exp.Hyde) == "" {
		return question
	}
	return question + "\n\n" + exp.Hyde
}

// appendExpansionIdentifiers appends model-guessed identifiers after the
// resolved ones, skipping case-insensitive duplicates so the exact-match
// candidates keep priority in the symbol lane.
func appendExpansionIdentifiers(existing, add []string) []string {
	if len(add) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(add))
	for _, id := range existing {
		seen[strings.ToLower(id)] = struct{}{}
	}
	out := existing
	for _, id := range add {
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
	}
	return out
}

// sanitizeTerms trims, drops empties, de-duplicates case-insensitively, and
// caps the list — keeping order so the model's ranking survives.
func sanitizeTerms(in []string, max int) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		key := strings.ToLower(t)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, t)
		if len(out) >= max {
			break
		}
	}
	return out
}
