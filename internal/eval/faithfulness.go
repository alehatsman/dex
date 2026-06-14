package eval

import (
	"fmt"
	"sort"
	"strings"
)

// Faithfulness measures whether a synthesized `ask` answer is grounded in the
// evidence it was given (#550). Retrieval recall (NDCG/Recall/RR) only scores
// whether the right files were FOUND — it says nothing about whether the prose
// the chat model wrote on top of them is supported. A confident-but-wrong
// answer is worse for an agent than raw evidence, so this is a separate gate.
//
// The proxy is deterministic and model-free: split the answer into sentences,
// and for each, measure the fraction of its content tokens that also appear in
// the evidence text. A sentence below the overlap threshold is "ungrounded" —
// it makes claims the evidence does not support. The score is the fraction of
// scorable sentences that are grounded. This is a claim-grounding proxy, not an
// entailment judge: it catches fabricated terms/identifiers, not subtle
// misstatements. It complements the runtime path-citation guard in
// internal/mcp/answer.go (validateAnswerCitations), which checks only paths.

// FaithfulnessOpts tunes the grounding proxy. The zero value is not valid; use
// DefaultFaithfulnessOpts.
type FaithfulnessOpts struct {
	// MinOverlap: a sentence is grounded when at least this fraction of its
	// content tokens appear in the evidence token set.
	MinOverlap float64
	// MinTokens: sentences with fewer content tokens are skipped as untestable
	// (greetings, "Here is the answer:", single-word fragments).
	MinTokens int
}

// DefaultFaithfulnessOpts is the calibrated default: a sentence is grounded when
// half its content tokens are evidence-backed, and sentences under 3 content
// tokens are too short to score.
func DefaultFaithfulnessOpts() FaithfulnessOpts {
	return FaithfulnessOpts{MinOverlap: 0.5, MinTokens: 3}
}

// FaithfulnessResult is the per-answer verdict.
type FaithfulnessResult struct {
	ID          string   `json:"id"`
	Score       float64  `json:"score"`        // grounded / scored sentences; 1.0 when nothing is scorable
	NumScored   int      `json:"num_scored"`   // sentences long enough to test
	NumGrounded int      `json:"num_grounded"` // of those, how many cleared MinOverlap
	Ungrounded  []string `json:"ungrounded,omitempty"`
}

// ScoreFaithfulness grades answer against evidence. An empty answer is vacuously
// faithful (Score 1.0, NumScored 0 — no claims to refute). When the evidence is
// empty there is nothing to ground against, so it is also treated as N/A.
func ScoreFaithfulness(id, answer, evidence string, opts FaithfulnessOpts) FaithfulnessResult {
	res := FaithfulnessResult{ID: id, Score: 1.0}
	evSet := tokenSet(evidence)
	if len(evSet) == 0 {
		return res // no evidence to ground against → N/A (NumScored 0)
	}
	for _, sent := range splitSentences(answer) {
		toks := contentTokens(sent)
		if len(toks) < opts.MinTokens {
			continue // too short to score
		}
		hit := 0
		for _, t := range toks {
			if evSet[t] {
				hit++
			}
		}
		overlap := float64(hit) / float64(len(toks))
		res.NumScored++
		if overlap >= opts.MinOverlap {
			res.NumGrounded++
		} else {
			res.Ungrounded = append(res.Ungrounded, strings.TrimSpace(sent))
		}
	}
	if res.NumScored > 0 {
		res.Score = float64(res.NumGrounded) / float64(res.NumScored)
	}
	return res
}

// FaithfulnessReport aggregates per-answer results into a gateable score.
type FaithfulnessReport struct {
	MeanScore     float64              `json:"mean_score"`   // over answers with >=1 scorable sentence
	NumAnswers    int                  `json:"num_answers"`  // total scored answers
	NumScorable   int                  `json:"num_scorable"` // answers contributing to MeanScore
	TotalGrounded int                  `json:"total_grounded"`
	TotalScored   int                  `json:"total_scored"`
	Opts          FaithfulnessOpts     `json:"opts"`
	Results       []FaithfulnessResult `json:"results"`
}

// AggregateFaithfulness folds per-answer results into a report. Answers with no
// scorable sentences (empty/short answers, no evidence) are excluded from
// MeanScore so they neither inflate nor deflate the gate.
func AggregateFaithfulness(results []FaithfulnessResult, opts FaithfulnessOpts) FaithfulnessReport {
	rep := FaithfulnessReport{NumAnswers: len(results), Opts: opts, Results: results}
	var sum float64
	for _, r := range results {
		if r.NumScored == 0 {
			continue
		}
		rep.NumScorable++
		rep.TotalGrounded += r.NumGrounded
		rep.TotalScored += r.NumScored
		sum += r.Score
	}
	if rep.NumScorable > 0 {
		rep.MeanScore = sum / float64(rep.NumScorable)
	} else {
		rep.MeanScore = 1.0 // nothing scorable → vacuously faithful
	}
	return rep
}

// Markdown renders the faithfulness summary and the worst-grounded answers.
func (r FaithfulnessReport) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# dex eval — answer faithfulness\n\n")
	fmt.Fprintf(&b, "| metric | value |\n|---|---|\n")
	fmt.Fprintf(&b, "| mean faithfulness | %.3f |\n", r.MeanScore)
	fmt.Fprintf(&b, "| answers scored | %d |\n", r.NumAnswers)
	fmt.Fprintf(&b, "| answers scorable | %d |\n", r.NumScorable)
	fmt.Fprintf(&b, "| grounded / scored sentences | %d / %d |\n", r.TotalGrounded, r.TotalScored)
	fmt.Fprintf(&b, "| overlap threshold | %.2f |\n", r.Opts.MinOverlap)

	// Surface the least-grounded answers — the actionable tail.
	worst := append([]FaithfulnessResult(nil), r.Results...)
	sort.SliceStable(worst, func(i, j int) bool {
		if worst[i].NumScored == 0 || worst[j].NumScored == 0 {
			return worst[j].NumScored == 0 && worst[i].NumScored != 0
		}
		return worst[i].Score < worst[j].Score
	})
	shown := 0
	for _, w := range worst {
		if w.NumScored == 0 || len(w.Ungrounded) == 0 {
			continue
		}
		if shown == 0 {
			fmt.Fprintf(&b, "\n## Least-grounded answers\n\n")
		}
		fmt.Fprintf(&b, "- **%s** (score %.2f, %d/%d grounded)\n", w.ID, w.Score, w.NumGrounded, w.NumScored)
		for _, s := range w.Ungrounded {
			fmt.Fprintf(&b, "  - ungrounded: %s\n", truncateClaim(s, 160))
		}
		shown++
		if shown >= 10 {
			break
		}
	}
	return b.String()
}

// truncateClaim shortens a claim for display without splitting a rune crudely.
func truncateClaim(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// faithfulnessStopwords are high-frequency words that carry no grounding signal;
// counting them would let filler prose mask a fabricated claim.
var faithfulnessStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"to": true, "of": true, "in": true, "on": true, "at": true, "for": true,
	"with": true, "by": true, "from": true, "as": true, "it": true, "its": true,
	"this": true, "that": true, "these": true, "those": true, "which": true,
	"into": true, "via": true, "then": true, "than": true, "so": true,
	"can": true, "will": true, "would": true, "should": true, "may": true,
	"if": true, "when": true, "where": true, "how": true, "what": true,
	"there": true, "here": true, "not": true, "no": true, "do": true, "does": true,
	"each": true, "all": true, "any": true, "some": true, "you": true, "we": true,
}

// contentTokens lowercases and splits a sentence into grounding-bearing tokens:
// stopwords and pure one/two-char fragments are dropped, but identifier-shaped
// tokens (containing a digit or underscore, e.g. fts5, schema_version) are kept
// regardless of length — those are exactly the claims worth grounding.
func contentTokens(s string) []string {
	var out []string
	for _, raw := range strings.FieldsFunc(strings.ToLower(s), splitToken) {
		if faithfulnessStopwords[raw] {
			continue
		}
		if len(raw) < 3 && !hasDigit(raw) {
			continue
		}
		out = append(out, raw)
	}
	return out
}

// tokenSet returns the set of all tokens in text (no stopword/length filter —
// evidence is the reference corpus, so every token can ground a claim).
func tokenSet(text string) map[string]bool {
	set := make(map[string]bool)
	for _, raw := range strings.FieldsFunc(strings.ToLower(text), splitToken) {
		set[raw] = true
	}
	return set
}

// splitToken is the FieldsFunc predicate: tokens are runs of [a-z0-9_], so
// identifiers like snake_case and bge-small split on '-' but keep '_'.
func splitToken(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// splitSentences breaks prose into sentences on terminal punctuation and line
// boundaries (Markdown bullets/headers count as sentence breaks). Crude but
// deterministic — good enough for a token-overlap proxy.
func splitSentences(text string) []string {
	var sents []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			sents = append(sents, s)
		}
		cur.Reset()
	}
	for _, r := range text {
		switch r {
		case '.', '!', '?', '\n':
			cur.WriteRune(r)
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return sents
}
