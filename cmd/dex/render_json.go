// Output rendering for the CLI subcommands. Splitting these out keeps
// main.go focused on dispatch + env wiring, and makes it obvious which
// pieces are "presentation only" vs "real work".
package main

import (
	"time"

	"github.com/alehatsman/dex/internal/output"
	"github.com/alehatsman/dex/internal/store"
)

type queryJSONHit struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	// SortScore is the authoritative key the hits are ordered by — compare
	// this across hits, not Score. Folds rerank/cross-encoder/RRF into one
	// monotonic value; the per-lane fields below are diagnostics.
	SortScore   float32  `json:"sort_score"`
	Score       float32  `json:"score"`
	BM25Score   float32  `json:"bm25_score,omitempty"`
	RRFScore    float32  `json:"rrf_score,omitempty"`
	RerankScore float32  `json:"rerank_score,omitempty"`
	Lanes       []string `json:"lanes,omitempty"` // retrieval lanes that surfaced this hit (#707)
	Content     string   `json:"content"`
}

// findJSONOutput wraps the ranked hits in the shared output envelope (#816).
// Hits stay the rich, find-specific payload (scores, lanes, content); the
// embedded envelope is the uniform machine contract a consumer reads without
// knowing find's bespoke shape — evidence carries the lean spans, confidence
// reports trust, stale flags index age, next_calls suggests the follow-up.
type findJSONOutput struct {
	Hits []queryJSONHit `json:"hits"`
	output.Envelope
}

// buildFindEnvelope derives the envelope from the ranked hits and the index's
// last-indexed time. Evidence mirrors the hit locations as lean spans (no
// content — the hits carry that); next_calls points at reading the top hit.
func buildFindEnvelope(hits []store.Hit, lastIndexed time.Time) output.Envelope {
	ev := make([]output.EvidenceSpan, 0, len(hits))
	for _, h := range hits {
		ev = append(ev, output.EvidenceSpan{
			Path: h.Path, Start: h.StartLine, End: h.EndLine,
			Symbol: h.Name, Kind: output.SpanExact,
		})
	}
	env := output.Envelope{
		Confidence: output.Confidence{
			Level: output.LevelHigh,
			Basis: []string{"hybrid retrieval: semantic + BM25 + symbol fusion"},
		},
		Evidence: ev,
		Stale:    output.AgeStale(lastIndexed),
	}
	if len(hits) > 0 {
		top := hits[0]
		env.NextCalls = []output.NextCall{{
			Tool:   "read",
			Args:   top.Path,
			Reason: "read the top-ranked hit in full",
		}}
		if top.Name != "" {
			env.NextCalls = append(env.NextCalls, output.NextCall{
				Tool:   "trace",
				Args:   top.Name,
				Reason: "follow the call graph around the top symbol",
			})
		}
	}
	env.Normalize()
	return env
}

// readJSONOutput wraps the read payload in the shared envelope (#816). The
// scalar fields are the read-specific payload; the embedded envelope is the
// uniform contract.
type readJSONOutput struct {
	Path        string `json:"path"`
	Mode        string `json:"mode"`
	Start       int    `json:"start,omitempty"`
	End         int    `json:"end,omitempty"`
	TotalLines  int    `json:"total_lines"`
	OutputLines int    `json:"output_lines"`
	Content     string `json:"content"`
	output.Envelope
}

// buildReadEnvelope derives the envelope for a read. Evidence is the single
// span actually returned (the requested range, or the whole file). Lossy modes
// (signatures/aggressive/entropy) drop to medium confidence with a gap naming
// the compression. The local read path serves live working-tree (or --ref)
// bytes and never consults the index, so staleness coverage is unknown and
// is_stale is false — the returned bytes are current, whatever the index age.
func buildReadEnvelope(path, mode string, start, end, totalLines int) output.Envelope {
	spanStart, spanEnd := start, end
	if spanStart <= 0 {
		spanStart = 1
	}
	if spanEnd <= 0 {
		spanEnd = totalLines
	}
	if spanEnd < spanStart {
		spanEnd = spanStart
	}
	conf := output.Confidence{Level: output.LevelHigh, Basis: []string{"exact working-tree bytes"}}
	var next []output.NextCall
	switch mode {
	case "signatures", "aggressive", "entropy":
		conf.Level = output.LevelMedium
		conf.Gaps = []string{"content compressed (mode=" + mode + ") — bodies elided"}
		next = []output.NextCall{{
			Tool:   "read",
			Args:   path + " --mode full",
			Reason: "read the full uncompressed content",
		}}
	}
	env := output.Envelope{
		Confidence: conf,
		Evidence: []output.EvidenceSpan{{
			Path: path, Start: spanStart, End: spanEnd, Kind: output.SpanExact,
		}},
		Stale:     output.StaleStatus{Coverage: output.CoverageUnknown},
		NextCalls: next,
	}
	env.Normalize()
	return env
}

func hitsToJSON(hits []store.Hit) []queryJSONHit {
	out := make([]queryJSONHit, len(hits))
	for i, h := range hits {
		out[i] = queryJSONHit{
			Path:        h.Path,
			Kind:        h.Kind,
			StartLine:   h.StartLine,
			EndLine:     h.EndLine,
			SortScore:   h.DisplayScore(),
			Score:       h.Score,
			BM25Score:   h.BM25Score,
			RRFScore:    h.RRFScore,
			RerankScore: h.RerankScore,
			Lanes:       h.Lanes.Names(),
			Content:     h.Content,
		}
	}
	return out
}
