// Package mcp — answer synthesis wiring for the `ask` tool.
//
// The synthesis algorithm (the chat call + answer cache) lives in
// internal/retrieve (retrieve.SynthesizeAnswer). This file is the
// transport side: it renders the wire ContextOutput into an evidence
// text block (buildAnswerEvidence) and adapts the MCP session's Log
// stream into a plain token callback.
//
// Synthesis is best-effort and never blocks a result: if the chat
// client is absent or unreachable, the bundle is returned exactly as
// before (status stays "ok"). The answer leads; next_action / avoid
// remain as cheap, template-built backstops.
package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/retrieve"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// answerMaxEvidenceBytes caps how much evidence text we feed the chat
// model. The inline byte pool bounds the bundle (~20 KB targeted /
// ~40 KB exploration), but a 40 KB block is ~10k tokens — more than
// many locally-served models load (Ollama defaults to a 4096-token
// context). Overflowing the window makes the backend silently truncate
// from the left, dropping the system prompt and producing a degraded or
// empty answer. 12 KB (~3k tokens) leaves headroom for the system
// prompt, question, and answer output inside a 4096 window. Sized for
// the smallest common local context; raise if you serve a long-context
// model.
const answerMaxEvidenceBytes = 12 * 1024

// synthesizeAnswer fills out.Answer (and out.AnswerModel) from the
// evidence already assembled on out. It is a no-op when no chat client
// is wired or no usable evidence text exists. Chat-layer failures are
// swallowed: a missing answer degrades to the evidence-only bundle.
//
// When session is non-nil, tokens are streamed to the client via Log
// notifications as they arrive, so the agent sees output before the
// tool call completes.
func (s *Server) synthesizeAnswer(ctx context.Context, session *sdk.ServerSession, intent, question string, out *ContextOutput) {
	if s.ChatClient == nil {
		return
	}
	evidence := buildAnswerEvidence(out)
	if strings.TrimSpace(evidence) == "" {
		return
	}

	var logTok func(string)
	if session != nil {
		logTok = func(tok string) {
			_ = session.Log(ctx, &sdk.LoggingMessageParams{
				Level:  "debug",
				Logger: "dex/ask",
				Data:   tok,
			})
		}
	}

	ans, model, hintErr := retrieve.SynthesizeAnswer(ctx, s.ChatClient, &s.answerCache, intent, question, evidence, logTok)
	if hintErr != nil {
		out.Hint = strings.TrimSpace(out.Hint + " (answer synthesis skipped: " + hintErr.Error() + ")")
		return
	}
	if ans == "" {
		return
	}
	out.Answer = ans
	out.AnswerModel = model
}

// buildAnswerEvidence renders the bundle into a compact, citation-ready
// text block. Order mirrors how an agent would read: curated reads
// first, then symbol signatures, then the structural graph. Bounded by
// answerMaxEvidenceBytes.
func buildAnswerEvidence(out *ContextOutput) string {
	var b strings.Builder
	budget := answerMaxEvidenceBytes

	write := func(s string) bool {
		if budget <= 0 {
			return false
		}
		if len(s) > budget {
			s = s[:budget]
		}
		b.WriteString(s)
		budget -= len(s)
		return budget > 0
	}

	reads := out.SuggestedReads
	if os.Getenv("DEX_ATTENTION_LAYOUT") == "1" {
		reads = sortSuggestedReadsByAttention(reads)
	}

	// Curated reads carry the richest signal (inlined source slices).
	for _, r := range reads {
		if strings.TrimSpace(r.Content) == "" {
			continue
		}
		hdr := fmt.Sprintf("\n--- %s", r.Path)
		if r.StartLine > 0 {
			hdr += fmt.Sprintf(":%d-%d", r.StartLine, r.EndLine)
		}
		if r.Reason != "" {
			hdr += fmt.Sprintf("  (%s)", r.Reason)
		}
		hdr += "\n"
		if !write(hdr) || !write(r.Content+"\n") {
			return b.String()
		}
	}

	// Semantic hits that weren't already promoted into suggested_reads.
	seen := map[string]bool{}
	for _, r := range out.SuggestedReads {
		seen[r.Path] = true
	}
	for _, h := range out.SemanticHits {
		if h.Content == "" || seen[h.Path] {
			continue
		}
		hdr := fmt.Sprintf("\n--- %s:%d-%d\n", h.Path, h.StartLine, h.EndLine)
		if !write(hdr) || !write(h.Content+"\n") {
			return b.String()
		}
	}

	// Symbol signatures + docs: the API contract without bodies.
	if len(out.Symbols) > 0 {
		if !write("\nSYMBOLS:\n") {
			return b.String()
		}
		for _, sym := range out.Symbols {
			line := fmt.Sprintf("- %s", sym.QualifiedName)
			if sym.Kind != "" {
				line += " (" + sym.Kind + ")"
			}
			if sym.Path != "" {
				line += fmt.Sprintf(" %s:%d", sym.Path, sym.StartLine)
			}
			if sym.Signature != "" {
				line += " — " + strings.TrimSpace(sym.Signature)
			}
			if !write(line + "\n") {
				return b.String()
			}
		}
	}

	// Graph edges: structural context for callers/callees/architecture.
	if out.Graph != nil && len(out.Graph.Edges) > 0 {
		if !write("\nGRAPH EDGES:\n") {
			return b.String()
		}
		for _, e := range out.Graph.Edges {
			if !write(fmt.Sprintf("- %s --%s--> %s\n", e.From, e.Kind, e.To)) {
				return b.String()
			}
		}
	}

	// Session context appended last: code content forms a stable prefix for
	// LLM provider KV-cache; dynamic task/facts only invalidate the tail.
	if out.SessionTask != "" || len(out.KnowledgeFacts) > 0 {
		if !write("\nSESSION CONTEXT:\n") {
			return b.String()
		}
		if out.SessionTask != "" {
			if !write(fmt.Sprintf("Task: %s\n", out.SessionTask)) {
				return b.String()
			}
		}
		for _, f := range out.KnowledgeFacts {
			if !write(fmt.Sprintf("- %s\n", f)) {
				return b.String()
			}
		}
	}

	return b.String()
}
