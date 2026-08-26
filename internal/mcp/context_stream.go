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
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/retrieve"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) contextRouterStreamImpl(ctx context.Context, req *sdk.CallToolRequest, in ContextInput, tokenSink func(string)) (*sdk.CallToolResult, ContextOutput, error) {
	if strings.TrimSpace(in.Question) == "" {
		// Empty question = session-start orientation: return the deterministic
		// L0+L1 map so the agent names the right cluster before any find()
		// (#348 / #316 story 6). No inference, byte-stable, cache-friendly.
		return s.orientResponse(ctx, in)
	}
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, ContextOutput{Status: "error", Hint: hint}, nil
	}

	// Repetition guard: at 7+ identical asks skip the expensive search+LLM
	// pipeline and return just the hint. At 4–6, continue but annotate.
	if throttleHint, earlyReturn := s.searchThrottleHint(in.Question, p.Root); earlyReturn {
		return nil, ContextOutput{Status: "ok", Project: p.Root, Hint: throttleHint}, nil
	} else if throttleHint != "" {
		// Pre-set hint from the repetition guard; applied to out.Hint below.
		hint = throttleHint
	}

	intent, candidates := retrieve.ResolveIntent(in.Question, in.Intent)
	if intent == retrieve.IntentOrient {
		// #135: a whole-repo orientation question ("understand this repo",
		// "overview of the codebase") gets the same deterministic orient bundle
		// as an empty question — the L0/L1 map + build/test commands answer it
		// better than semantic search + LLM synthesis. The classifier keeps this
		// narrow (explicit repo subject); an explicit non-orient intent override
		// still wins because ResolveIntent honours it first.
		return s.orientResponse(ctx, in)
	}
	if intent == retrieve.IntentReview {
		// #144: "review my changes" routes to the per-hunk review composition,
		// not the search lanes. Its result is delta-shaped, so reviewResponse
		// carries it in the discriminated-union ContextOutput.Review field. The
		// auto path reviews the working tree (Review's #137 default); targeted
		// PR/branch/ref review stays on review_diff / `dex review`. An explicit
		// non-review intent override still wins — ResolveIntent honours it first.
		return s.reviewResponse(ctx, in)
	}
	out := ContextOutput{Project: p.Root, Intent: intent}
	if hint != "" {
		out.Hint = hint
	}

	if _, err := os.Stat(p.DBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			out.Status = "no-index"
			out.Hint = fmt.Sprintf("no index for %s — run `dex index %s` first; fall back to grep until then.", p.Root, p.Root)
			return nil, out, nil
		}
		out.Status = "error"
		out.Hint = err.Error()
		return nil, out, nil
	}

	k := in.K
	if k <= 0 {
		k = 8
	}
	k = min(k, 30)

	st, err := s.openStore(p.DBPath)
	if err != nil {
		out.Status = "error"
		out.Hint = fmt.Sprintf("open index: %v", err)
		return nil, out, nil
	}

	stale, indexing, indexedAt := contextRouterCheckStale(ctx, st, &out, p.Root)
	s.loadContextFacts(ctx, st, in, &out)

	// The assembler sets pack.Graph only when it has something to emit; the
	// projection below leaves out.Graph absent otherwise. An absent `graph`
	// key signals "no graph indexed, or this intent surfaced no structural
	// context" — saves bytes over shipping `{nodes:[], edges:[]}` on every
	// response.

	// Load the graph view once per request. Nil view = no graph
	// indexed; intents that need it will note this in `avoid`.
	graphView, _ := s.cachedLoadGraphView(ctx, st, p.DBPath)

	// Query-side expansion (#252) — opt-in, failure-soft. One small-model
	// call turns the question into extra retrieval terms fanned across the
	// lanes; the raw question stays in every lane so a fanciful generation
	// is diluted by RRF rather than amplified. Empty/timeout/error → the
	// un-expanded query. Placed after store-open so a broken repo never
	// pays the GPU call.
	exp := retrieve.ExpandQuery(ctx, s.ExpandClient, in.Question, retrieve.ResolveExpandMode(in.Expand, s.ExpandMode))
	if !exp.Empty() {
		candidates.Identifiers = retrieve.AppendExpansionIdentifiers(candidates.Identifiers, exp.Identifiers)
		out.Expanded = true
	}

	// Evidence assembly (#95a / #103) — the symbol/semantic lanes, graph
	// neighborhood, lane-agreement reweight and suggested-reads ranker now
	// compose in internal/retrieve.Assembler, producing a complete ContextPack.
	// The transport injects the policies it deliberately owns (the call-graph
	// role vocabulary and path classification, both shared across tools; the
	// byte-budget inline presentation pass; the feedback A/B hooks), then
	// projects the pack onto the wire response and applies transport concerns
	// (near-miss, session, throttle, answer synthesis, handles, dedup).
	pack, meta := retrieve.Assembler{
		Service:    retrieve.Service{Embed: s.EmbedClient},
		FormatRole: formatRole,
		IsNonImpl:  isNonImplPath,
		IsTestPath: isTestPath,
	}.Assemble(ctx, st, retrieve.AssembleRequest{
		Intent:       intent,
		Question:     in.Question,
		Candidates:   candidates,
		K:            k,
		Graph:        graphView,
		EmbedText:    retrieve.ExpandedEmbedText(in.Question, exp),
		FTSText:      retrieve.ExpandedFTSText(in.Question, exp),
		Expanded:     out.Expanded,
		ProjectRoot:  p.Root,
		NoInline:     in.NoInline,
		Spread:       st,
		RecordShadow: s.recordShadowPack,
		Reweight:     s.reweightPack,
		Stale:        stale,
		Indexing:     indexing,
		IndexedAt:    indexedAt,
	})
	out.Symbols = fromPackSyms(pack.Symbols)
	out.SemanticHits = fromPackSems(pack.SemanticHits)
	if g := fromPackGraph(pack.Graph); g != nil {
		out.Graph = g
	}
	out.SuggestedReads = fromPackReads(pack.SuggestedReads)
	out.References = fromNeutralRefs(pack.References)
	out.Annotations = fromNeutralAnnotations(pack.Annotations)
	out.RelatedFiles = pack.RelatedFiles
	out.ContentBytesInlined = pack.ContentBytesInlined
	if pack.Concerns.Covered != nil || pack.Concerns.Dropped != nil {
		out.Concerns = &AssembleConcerns{Covered: pack.Concerns.Covered, Dropped: pack.Concerns.Dropped}
	}
	out.NextAction = pack.NextAction
	out.Avoid = pack.Avoid
	out.Trust = fromPackTrust(pack.Trust)

	embedFailed := meta.EmbedFailed
	leanNoEmbedder := s.EmbedClient == nil
	if embedFailed && !leanNoEmbedder {
		out.Endpoint = s.EmbedClient.Endpoint()
	}
	// Probe index emptiness only when both lanes whiffed — keeps the EXISTS
	// query off the hot path, and lets a 0-chunk index (e.g. no index.include)
	// be reported as a config problem rather than a no-match (#161).
	if len(out.Symbols) == 0 && len(out.SemanticHits) == 0 {
		indexEmpty := false
		if empty, err := st.IsEmpty(ctx); err == nil {
			indexEmpty = empty
		}
		if noLaneHits(embedFailed, leanNoEmbedder, indexEmpty, &out) {
			return nil, out, nil
		}
	}

	// Near-miss surface for symbol_lookup whiffs — only when the symbol lane
	// actually whiffed. symbolNearMiss does a substring scan, so without this
	// gate a query that has exact defs but is also a substring of other names
	// (Run ⊂ runBench/RunResult) would emit a contradictory "no exact symbol
	// match" hint alongside the exact symbols[] it found (#533).
	if len(out.Symbols) == 0 {
		if hint := symbolNearMiss(ctx, st, intent, candidates); hint != "" {
			out.Hint = hint
		}
	}
	out.Status = "ok"
	if embedFailed && out.Hint == "" {
		out.Hint = "embed offline; results from symbol lane only."
	}
	// Task-start packs carry the local rules that govern the working set, so the
	// agent sees the constraints before editing. Folded in from brief (#141);
	// assemble is the "starting a task" intent, so only it pays this cost.
	if intent == retrieve.IntentAssemble {
		out.Rules = collectLocalRules(p.Root)
	}
	// Synthesize a grounded prose answer from the evidence just
	// assembled. Best-effort: a missing/unreachable chat client leaves
	// out.Answer empty and the agent falls back to the evidence bundle.
	logTok := resolveLogSink(ctx, tokenSink, req)
	// Loop detection: check after evidence is assembled so a block still
	// fires even when the question changes but the search pattern repeats.
	if blocked := s.applyLoopThrottle(in.Question, &out); blocked {
		return nil, out, nil
	}

	s.maybeAnswerStyle(ctx, logTok, intent, in, &out)

	// Stamp expansion handles on every locator the bundle hands back (#344),
	// after truncation so dropped hits don't get handles.
	stampSemHandles(out.SemanticHits)
	stampSymbolHandles(out.Symbols)
	stampReadHandles(out.SuggestedReads)

	// Cross-turn dedup (#344): mark locators already surfaced to this session on
	// an earlier turn and drop their content, so we don't resend bytes the agent
	// already holds.
	s.applySeenContext(sessionKey(req), &out)

	// Final safety net: keep the whole serialized bundle under a hard ceiling.
	// The inline byte pool bounds suggested_reads + semantic_hits, but the graph
	// is appended outside it, so a dense graph on top of a full exploration pool
	// could still overflow the MCP tool-result limit (#784).
	clampResponseEnvelope(&out, intent)
	return nil, out, nil
}
