// Package mcp — answer synthesis for the `ask` tool.
//
// answer.go turns the evidence bundle that contextRouter composes
// (suggested_reads with inlined content, symbol signatures+docs, graph
// edges) into a short grounded prose answer via the chat leg. This is
// what makes `ask` the single tool an agent needs: it returns the
// prepared answer, not just routing instructions.
//
// Synthesis is best-effort and never blocks a result: if the chat
// client is absent or unreachable, the bundle is returned exactly as
// before (status stays "ok"). The answer leads; next_action / avoid
// remain as cheap, template-built backstops.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/alehatsman/dex/internal/chat"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// answerMaxTokens caps synthesis length. Answers are meant to be a
// tight paragraph or two with citations, not an essay — bounding tokens
// also bounds generation time on the (shared) GPU.
const answerMaxTokens = 400

// answerMaxEvidenceBytes caps how much evidence text we feed the chat
// model. The inline byte pool bounds the bundle (~20 KB targeted /
// ~40 KB exploration), but a 40 KB block is ~10k tokens — more than
// many locally-served models load (Ollama defaults to a 4096-token
// context). Overflowing the window makes the backend silently truncate
// from the left, dropping the system prompt and producing a degraded or
// empty answer. 12 KB (~3k tokens) leaves headroom for the system
// prompt, question, and answerMaxTokens of output inside a 4096 window.
// Sized for the smallest common local context; raise if you serve a
// long-context model.
const answerMaxEvidenceBytes = 12 * 1024

// synthesizeAnswer fills out.Answer (and out.AnswerModel) from the
// evidence already assembled on out. It is a no-op when no chat client
// is wired or no usable evidence text exists. Chat-layer failures are
// swallowed: a missing answer degrades to the evidence-only bundle,
// never an error to the caller.
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

	model := s.ChatClient.ModelName()
	key := answerCacheKey(question, intent, model, evidence)
	if cached, ok := answerCacheGet(key); ok {
		out.Answer = cached
		out.AnswerModel = model
		return
	}

	msgs := []chat.Message{
		{Role: "system", Content: answerSystemPrompt},
		{Role: "user", Content: buildAnswerUser(question, intent, evidence)},
	}
	opts := chat.Options{MaxTokens: answerMaxTokens}

	var (
		resp chat.Response
		err  error
	)
	if session != nil {
		resp, err = s.ChatClient.GenerateStream(ctx, msgs, opts, func(tok string) {
			_ = session.Log(ctx, &sdk.LoggingMessageParams{
				Level:  "debug",
				Logger: "dex/ask",
				Data:   tok,
			})
		})
	} else {
		resp, err = s.ChatClient.Generate(ctx, msgs, opts)
	}
	if err != nil {
		// Unreachable / any chat error → leave Answer empty; the agent
		// still has the full evidence bundle and next_action.
		if !errors.Is(err, chat.ErrUnreachable) {
			out.Hint = strings.TrimSpace(out.Hint + " (answer synthesis skipped: " + err.Error() + ")")
		}
		return
	}
	ans := strings.TrimSpace(resp.Content)
	if ans == "" {
		return
	}
	out.Answer = ans
	out.AnswerModel = model
	answerCachePut(key, ans)
}

const answerSystemPrompt = "You are a code-intelligence assistant answering a question about ONE specific " +
	"codebase. Use ONLY the EVIDENCE provided below — code excerpts, symbol signatures, and graph edges. " +
	"Answer in a few concise sentences, concrete and specific to this code. Cite the locations that support " +
	"each claim inline as `path:line`. If the evidence is insufficient to answer fully, say so in one sentence " +
	"and name the most useful file or symbol to read next. Never invent file paths, identifiers, or APIs that " +
	"do not appear in the evidence."

// buildAnswerUser assembles the user turn: the question, the routed
// intent (so the model knows whether it's explaining behavior, listing
// callers, etc.), and the evidence block.
func buildAnswerUser(question, intent, evidence string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "QUESTION: %s\n", strings.TrimSpace(question))
	if intent != "" {
		fmt.Fprintf(&b, "INTENT: %s\n", intent)
	}
	b.WriteString("\nEVIDENCE:\n")
	b.WriteString(evidence)
	return b.String()
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

	// Curated reads carry the richest signal (inlined source slices).
	for _, r := range out.SuggestedReads {
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

// ─── answer cache ─────────────────────────────────────────────────────────
//
// Agents re-ask the same question repeatedly within a session. The
// cache key folds in the evidence text, so a re-index that changes the
// retrieved chunks (or a different model) naturally misses — no explicit
// invalidation needed. Bounded FIFO; correctness doesn't depend on
// retention, only latency/GPU savings.

const answerCacheCap = 256

var (
	answerCacheMu    sync.Mutex
	answerCacheData  = make(map[string]string, answerCacheCap)
	answerCacheOrder = make([]string, 0, answerCacheCap)
)

func answerCacheKey(question, intent, model, evidence string) string {
	h := sha256.New()
	// Length-prefix each field so concatenation can't collide across
	// boundaries (e.g. "ab"+"c" vs "a"+"bc").
	for _, part := range []string{question, intent, model, evidence} {
		h.Write(fmt.Appendf(nil, "%d:", len(part)))
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func answerCacheGet(key string) (string, bool) {
	answerCacheMu.Lock()
	defer answerCacheMu.Unlock()
	v, ok := answerCacheData[key]
	return v, ok
}

func answerCachePut(key, val string) {
	answerCacheMu.Lock()
	defer answerCacheMu.Unlock()
	if _, exists := answerCacheData[key]; exists {
		answerCacheData[key] = val
		return
	}
	if len(answerCacheOrder) >= answerCacheCap {
		oldest := answerCacheOrder[0]
		answerCacheOrder = answerCacheOrder[1:]
		delete(answerCacheData, oldest)
	}
	answerCacheData[key] = val
	answerCacheOrder = append(answerCacheOrder, key)
}
