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
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: ask ────────────────────────────────────────────────────────────

// ContextRouter is the exported entry point used by the CLI
// (`dex ask`). It delegates to the MCP-registered handler.
func (s *Server) ContextRouter(ctx context.Context, in ContextInput) (*sdk.CallToolResult, ContextOutput, error) {
	return s.contextRouterStream(ctx, nil, in, nil)
}

// ContextRouterStream is ContextRouter with a token sink: when tokenSink
// is non-nil, answer-synthesis tokens are delivered to it as they arrive
// (the CLI streams them to stdout for a fast first token). A nil sink is
// identical to ContextRouter.
func (s *Server) ContextRouterStream(ctx context.Context, in ContextInput, tokenSink func(string)) (*sdk.CallToolResult, ContextOutput, error) {
	return s.contextRouterStream(ctx, nil, in, tokenSink)
}

// contextRouter satisfies the toolSurface interface used by tool
// registration and the HTTP/remote proxies; those paths never stream, so
// it delegates with a nil sink.
func (s *Server) contextRouter(ctx context.Context, req *sdk.CallToolRequest, in ContextInput) (*sdk.CallToolResult, ContextOutput, error) {
	return s.contextRouterStream(ctx, req, in, nil)
}

// contextRouterCheckStale sets the freshness hint on out and returns the facts
// (stale, indexing, and the index's last-indexed time — zero if unknown) for the
// Trust envelope (#95c/#116). The booleans ride pack.Trust rather than top-level
// out fields, so the router threads them into the AssembleRequest.
func contextRouterCheckStale(ctx context.Context, st *store.Store, out *ContextOutput, root string) (stale, indexing bool, indexedAt time.Time) {
	if stats, statsErr := st.Stats(ctx); statsErr == nil && !stats.LastIndex.IsZero() {
		indexedAt = stats.LastIndex
		if time.Since(stats.LastIndex) > 24*time.Hour {
			stale = true
			out.Hint = appendHint(out.Hint, fmt.Sprintf("index is %s old — run `dex index %s` to refresh.",
				time.Since(stats.LastIndex).Round(time.Hour), root))
		}
	}
	// An active rebuild trumps age: evidence is being rewritten right now, so
	// what we return is partial (#531).
	if inProgress, note := indexingNotice(ctx, st); inProgress {
		stale = true
		indexing = true
		out.Hint = note
	}
	return stale, indexing, indexedAt
}

// loadContextFacts loads the current session task into out. Knowledge-fact
// injection was removed with the L3 subsystem (#205): dex is retrieval over the
// codebase, not agent memory.
func (s *Server) loadContextFacts(ctx context.Context, st *store.Store, in ContextInput, out *ContextOutput) {
	if ss, ok, err := st.SessionGet(ctx); err == nil && ok && ss.Task != "" {
		out.SessionTask = ss.Task
	}
}

// contextRouterStream is the single chokepoint all ask entry points (MCP tool,
// CLI, HTTP) funnel through. It runs the router, then stamps the structured
// `next` step so every path — success, degraded, orient, and error — carries the
// four-verb envelope's `next` key without touching the router's many internal
// returns.
func (s *Server) contextRouterStream(ctx context.Context, req *sdk.CallToolRequest, in ContextInput, tokenSink func(string)) (*sdk.CallToolResult, ContextOutput, error) {
	res, out, err := s.contextRouterStreamImpl(ctx, req, in, tokenSink)
	deriveAskNext(&out)
	return res, out, err
}

// deriveAskNext fills ContextOutput.Next from evidence ask already produced. The
// grounded, non-inferred follow-up is to look at the top suggested read — ask has
// already decided that file is worth opening. It never overwrites a Next the
// router set explicitly, and emits nothing when there is no concrete target.
func deriveAskNext(out *ContextOutput) {
	if len(out.Next) > 0 || len(out.SuggestedReads) == 0 {
		return
	}
	top := out.SuggestedReads[0]
	if strings.TrimSpace(top.Path) == "" {
		return
	}
	target := top.Path
	if top.StartLine > 0 {
		target = top.Path + ":" + strconv.Itoa(top.StartLine)
	}
	why := "open the top suggested read"
	if strings.TrimSpace(top.Reason) != "" {
		why = top.Reason
	}
	out.Next = []NextStep{{
		Verb: "look",
		Args: map[string]any{"target": target},
		Why:  why,
	}}
}
