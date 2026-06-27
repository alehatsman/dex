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
// When logTok is non-nil, tokens are streamed to it as they arrive, so
// the transport (MCP Log notifications, or the CLI's stdout) sees output
// before the call completes. A cache hit returns the whole answer at once
// and never calls logTok.
func (s *Server) synthesizeAnswer(ctx context.Context, logTok func(string), intent, question string, out *ContextOutput) {
	// assemble (#687) returns the structured working set, not prose — no synthesis.
	if intent == retrieve.IntentAssemble {
		return
	}
	if s.ChatClient == nil {
		return
	}
	evidence := buildAnswerEvidence(intent, out)
	if strings.TrimSpace(evidence) == "" {
		return
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
//
// Exception: for the callers/callees intents the graph edges ARE the
// answer, so they lead — otherwise a budget-filling reads block can
// truncate them out entirely and the model concludes "no callers" while
// graph.edges carries the real ones (issue #535).
// appendGraphEdges renders the structural call/import edges into the evidence
// via write. Returns false when the byte budget is exhausted mid-write so the
// caller can stop early. A no-op (returns true) when there are no edges.
func appendGraphEdges(write func(string) bool, out *ContextOutput) bool {
	if out.Graph == nil || len(out.Graph.Edges) == 0 {
		return true
	}
	if !write("\nGRAPH EDGES:\n") {
		return false
	}
	for _, e := range out.Graph.Edges {
		if !write(fmt.Sprintf("- %s --%s--> %s\n", e.From, e.Kind, e.To)) {
			return false
		}
	}
	return true
}

func buildAnswerEvidence(intent string, out *ContextOutput) string {
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
	// writeHdr writes a section header only when it fits in the remaining
	// budget. Unlike write(), it never truncates mid-header — a partial path
	// or dangling "---" confuses downstream models. Returns false (stop) when
	// the header doesn't fit.
	writeHdr := func(hdr string) bool {
		if len(hdr) > budget {
			return false
		}
		return write(hdr)
	}

	// For callers/callees the graph edges ARE the authoritative answer — lead
	// with them so they survive the byte budget regardless of how rich the
	// reads/semantic lanes are (issue #535). Other intents render them last.
	leadGraph := intent == "callers" || intent == "callees"
	if leadGraph {
		if !appendGraphEdges(write, out) {
			return b.String()
		}
	}

	reads := out.SuggestedReads
	if os.Getenv("DEX_ATTENTION_LAYOUT") == "1" {
		reads = sortSuggestedReadsByAttention(reads)
	}

	if !appendSuggestedReadsSection(writeHdr, write, reads) {
		return b.String()
	}
	if !appendSemanticHitsSection(writeHdr, write, out.SuggestedReads, out.SemanticHits) {
		return b.String()
	}
	if !appendSymbolsSection(write, out.Symbols) {
		return b.String()
	}

	// Graph edges in their default trailing position (callers/callees already led).
	if !leadGraph {
		appendGraphEdges(write, out)
	}

	// Session context appended last: code content forms a stable prefix for
	// LLM provider KV-cache; dynamic task/facts only invalidate the tail.
	if !appendSessionContextSection(write, out.SessionTask, out.KnowledgeFacts) {
		return b.String()
	}

	return b.String()
}

// appendSuggestedReadsSection writes curated reads into the evidence block.
// Returns false when the byte budget is exhausted.
func appendSuggestedReadsSection(writeHdr, write func(string) bool, reads []SuggestedRead) bool {
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
		if !writeHdr(hdr) || !write(r.Content+"\n") {
			return false
		}
	}
	return true
}

// appendSemanticHitsSection writes semantic hits that weren't already promoted
// into suggested_reads. Keyed on (path, startLine) to avoid duplicate slices.
// Returns false when the byte budget is exhausted.
func appendSemanticHitsSection(writeHdr, write func(string) bool, reads []SuggestedRead, hits []SemHit) bool {
	type pathLine struct {
		path string
		line int
	}
	seen := map[pathLine]bool{}
	for _, r := range reads {
		seen[pathLine{r.Path, r.StartLine}] = true
	}
	for _, h := range hits {
		if h.Content == "" || seen[pathLine{h.Path, h.StartLine}] {
			continue
		}
		hdr := fmt.Sprintf("\n--- %s:%d-%d\n", h.Path, h.StartLine, h.EndLine)
		if !writeHdr(hdr) || !write(h.Content+"\n") {
			return false
		}
	}
	return true
}

// appendSymbolsSection writes symbol signatures + docs into the evidence block.
// Returns false when the byte budget is exhausted.
func appendSymbolsSection(write func(string) bool, symbols []SymbolHit) bool {
	if len(symbols) == 0 {
		return true
	}
	if !write("\nSYMBOLS:\n") {
		return false
	}
	for _, sym := range symbols {
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
			return false
		}
	}
	return true
}

// appendSessionContextSection writes the session task + knowledge facts into
// the evidence block. Returns false when the byte budget is exhausted.
func appendSessionContextSection(write func(string) bool, task string, facts []string) bool {
	if task == "" && len(facts) == 0 {
		return true
	}
	if !write("\nSESSION CONTEXT:\n") {
		return false
	}
	if task != "" {
		if !write(fmt.Sprintf("Task: %s\n", task)) {
			return false
		}
	}
	for _, f := range facts {
		if !write(fmt.Sprintf("- %s\n", f)) {
			return false
		}
	}
	return true
}

// maybeAnswerStyle runs the chat synthesis leg when answer_style is "brief".
// Any other value (including the empty-string MCP default) skips synthesis so
// the agent works directly from the evidence bundle.
func (s *Server) maybeAnswerStyle(ctx context.Context, logTok func(string), intent string, in ContextInput, out *ContextOutput) {
	if in.AnswerStyle != "brief" {
		return
	}
	s.synthesizeAnswer(ctx, logTok, intent, in.Question, out)
	// next_action was built deterministically from suggested_reads[0] before
	// the answer existed; realign it so it never points away from the file the
	// answer leads with (#532).
	reconcileNextActionWithAnswer(out)
}
