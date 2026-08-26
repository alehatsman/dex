// Package mcp — the `ask` tool.
//
// context.go wires up `ask`, a query planner for code understanding.
// The goal is to be the single entry point an agent reaches for
// instead of fanning out to grep / Read / search_semantic loops.
// Given a project and a free-text question (plus optional intent
// override), the router picks a strategy, runs the right combination
// of legs (search_semantic, search_symbol, graph queries) and
// returns a compact bundle with `suggested_reads`, a prose
// `next_action`, and an `avoid` line.
//
// Graph integration: callers/callees use the `calls` edges from
// internal/graph (Go-only). Other languages get a BM25 chunk search
// over the bare symbol name as a fallback (see runReferencesLane).
package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/throttle"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// envelopeCeilingBytes is the hard cap on one ask response's serialized size,
// derived from the intent's inline pool budget plus headroom for the lanes that
// sit outside the pool (graph, knowledge_facts, annotations, answer). It stays
// well under the MCP tool-result token ceiling — a ~62 KB bundle has overflowed
// it in practice (#784).
func envelopeCeilingBytes(intent string) int {
	const outOfPoolHeadroom = 10 * 1024
	return retrieve.InlineCapsFor(intent).TotalBytesCap + outOfPoolHeadroom
}

// clampResponseEnvelope trims the bundle to fit envelopeCeilingBytes, shedding
// the lowest-value lanes first. The graph is a structural hint the agent can
// rebuild with trace(), so it goes before any inlined code: edges first (the
// bulk), then the nodes. If that is still not enough, inlined Content is
// dropped from the lowest-ranked semantic hits — tail first, never the top
// hit — leaving their locators so the agent can Read them. A no-op in the
// common case, where the bundle is already under budget.
func clampResponseEnvelope(out *ContextOutput, intent string) {
	ceiling := envelopeCeilingBytes(intent)
	if envelopeSizeBytes(out) <= ceiling {
		return
	}
	trimmed := false
	if out.Graph != nil && len(out.Graph.Edges) > 0 {
		out.Graph.Edges = nil
		trimmed = true
	}
	if out.Graph != nil && envelopeSizeBytes(out) > ceiling {
		out.Graph = nil
		trimmed = true
	}
	for envelopeSizeBytes(out) > ceiling {
		i := lastInlinedSemHit(out.SemanticHits)
		if i < 0 {
			break // nothing left to shed but the top hit
		}
		out.SemanticHits[i].Content = ""
		out.SemanticHits[i].Truncated = true
		trimmed = true
	}
	if trimmed {
		out.NextAction = strings.TrimSpace(out.NextAction +
			" [dex] Response trimmed to fit the size budget — graph and/or inlined tails dropped; trace() or Read the locators for the rest.")
	}
}

// envelopeSizeBytes is the serialized length the caller will receive. A marshal
// error (which should not happen for this struct) returns 0 so the clamp is a
// no-op rather than trimming a bundle it cannot measure.
func envelopeSizeBytes(out *ContextOutput) int {
	b, err := json.Marshal(out)
	if err != nil {
		return 0
	}
	return len(b)
}

// lastInlinedSemHit returns the index of the lowest-ranked semantic hit that
// still carries inlined Content, or -1 when only the top hit (index 0) does.
// Trimming tail-first preserves the highest-scoring evidence.
func lastInlinedSemHit(hits []SemHit) int {
	for i := len(hits) - 1; i >= 1; i-- {
		if hits[i].Content != "" {
			return i
		}
	}
	return -1
}

// resolveLogSink returns the token sink to use for answer streaming.
// An explicit sink (CLI path) wins; for MCP sessions the sink wraps
// session.Log so tokens arrive as Log notifications; otherwise nil.
func resolveLogSink(ctx context.Context, tokenSink func(string), req *sdk.CallToolRequest) func(string) {
	if tokenSink != nil {
		return tokenSink
	}
	if req == nil || req.Session == nil {
		return nil
	}
	session := req.Session
	return func(tok string) {
		_ = session.Log(ctx, &sdk.LoggingMessageParams{
			Level:  "debug",
			Logger: "dex/ask",
			Data:   tok,
		})
	}
}

// noLaneHits sets out.Status/Hint and returns true when both retrieval lanes
// returned nothing. The caller should return immediately on true.
func noLaneHits(embedFailed, leanNoEmbedder, indexEmpty bool, out *ContextOutput) bool {
	if len(out.Symbols) > 0 || len(out.SemanticHits) > 0 {
		return false
	}
	// An empty index is the dominant, retry-proof cause — no query and no
	// embedder state can conjure a match from 0 chunks. Report it ahead of the
	// embed-failed / no-match branches so the agent fixes the config, not the
	// phrasing (#161).
	if indexEmpty {
		out.Status = "index-empty"
		out.Hint = "index is empty (0 chunks) — likely no index.include in .dex/config.yml; run `dex doctor` for the diagnosis, then `dex index`."
		out.NextAction = "This repo's index has 0 chunks, so no query can match. Add an index.include allow-list (see dex doctor), re-run dex index, then retry — do not rephrase."
		return true
	}
	if embedFailed {
		if leanNoEmbedder {
			out.Status = "lean-no-embedder"
			out.Hint = "lean profile (DEX_EMBED_ENGINE=none): no semantic lane — use lookup, grep, or the trace/impact graph tools."
		} else {
			out.Status = "embedding-service-unreachable"
			out.Hint = "the local embedding service is offline — fall back to grep / Glob / ripgrep for this query."
		}
		return true
	}
	out.Status = "ok"
	out.Hint = "no matches; try broader phrasing or a more specific identifier."
	out.NextAction = "Try rephrasing the question with concrete keywords from the codebase, or fall back to grep."
	return true
}

// symbolNearMiss returns a hint string when the question is a symbol_lookup
// with no exact hits. It scans chunks for substring candidates so the agent
// gets names without a follow-up tool call.
func symbolNearMiss(ctx context.Context, st store.Searcher, intent string, candidates retrieve.IntentCandidates) string {
	if intent != retrieve.IntentSymbolLookup || len(candidates.Identifiers) == 0 {
		return ""
	}
	var cands []string
	for _, id := range candidates.Identifiers {
		bare := id
		if i := strings.LastIndex(bare, "."); i >= 0 {
			bare = bare[i+1:]
		}
		names, err := st.FindSymbolCandidates(ctx, bare, 5)
		if err != nil {
			continue
		}
		cands = append(cands, names...)
		if len(cands) >= 5 {
			cands = cands[:5]
			break
		}
	}
	if len(cands) == 0 {
		return ""
	}
	return "no exact symbol match — did you mean: " + strings.Join(cands, ", ") + "?"
}

// applyLoopThrottle applies loop-detection throttling. It returns true when
// the call is blocked (caller should return early with out unchanged).
func (s *Server) applyLoopThrottle(question string, out *ContextOutput) bool {
	ldLevel, ldHint := s.ld().Check("ask", throttle.ArgsKey(question), true)
	if ldLevel == throttle.Block {
		out.Status = "loop-blocked"
		out.Hint = ldHint
		out.SemanticHits = nil
		out.Symbols = nil
		return true
	}
	if ldLevel == throttle.Reduce {
		if len(out.SemanticHits) > 3 {
			out.SemanticHits = out.SemanticHits[:3]
		}
		if len(out.Symbols) > 3 {
			out.Symbols = out.Symbols[:3]
		}
		out.Hint = ldHint + " [reduced]"
	} else if ldHint != "" && out.Hint == "" {
		out.Hint = ldHint
	}
	return false
}
