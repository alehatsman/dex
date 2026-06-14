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
	"path/filepath"
	"regexp"
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

	// Guard against fabricated citations. The contract tells the model to
	// cite ONLY paths from the evidence bundle (all index-derived, so real).
	// A path-shaped token in the answer that is absent from that bundle is
	// ungrounded — a smaller answer model can invent a plausible path the
	// agent then wastes a round-trip read()ing. We don't rewrite the prose
	// (the wording is opaque); we append a deterministic steering note.
	if bad := validateAnswerCitations(ans, out); len(bad) > 0 {
		out.Answer += "\n\n[dex] Unverified path(s) not in the evidence: " +
			strings.Join(bad, ", ") +
			" — these were not provided to the model; do not read() them, rely on suggested_reads / next_action."
	}
}

// answerPathCitation matches repo-relative file-path citations in prose:
// a leading non-path boundary (RE2 has no lookbehind, so we consume one
// char and capture the path in group 1), then one or more "segment/"
// parts and a filename with an extension. The first segment is dot-free
// and the boundary excludes '.', which together drop import paths and
// domains (github.com/foo.go) and version strings (qwen2.5-coder:14b)
// that are not read()-able repo files.
var answerPathCitation = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./])([A-Za-z0-9_-]+(?:/[A-Za-z0-9_.+-]+)+\.[A-Za-z0-9]+)`)

// evidencePathSet collects every index-derived path the model was shown,
// normalized to forward-slash clean form for membership tests.
func evidencePathSet(out *ContextOutput) map[string]bool {
	set := make(map[string]bool)
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p != "" {
			set[filepath.ToSlash(filepath.Clean(p))] = true
		}
	}
	for _, r := range out.SuggestedReads {
		add(r.Path)
	}
	for _, h := range out.SemanticHits {
		add(h.Path)
	}
	for _, sym := range out.Symbols {
		add(sym.Path)
	}
	for p := range out.Annotations {
		add(p)
	}
	return set
}

// validateAnswerCitations returns the path-shaped citations in the answer
// that are absent from the evidence bundle, in first-seen order with
// duplicates collapsed. Returns nil when the answer is empty or the
// evidence set is empty (nothing to validate against).
func validateAnswerCitations(answer string, out *ContextOutput) []string {
	if strings.TrimSpace(answer) == "" {
		return nil
	}
	evidence := evidencePathSet(out)
	if len(evidence) == 0 {
		return nil
	}
	var ungrounded []string
	seen := make(map[string]bool)
	for _, m := range answerPathCitation.FindAllStringSubmatch(answer, -1) {
		raw := m[1]
		norm := filepath.ToSlash(filepath.Clean(raw))
		if evidence[norm] || seen[norm] {
			continue
		}
		seen[norm] = true
		ungrounded = append(ungrounded, raw)
	}
	return ungrounded
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
