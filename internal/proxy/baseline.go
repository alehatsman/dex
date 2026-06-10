package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// anthropicRequest is the minimal slice of the /v1/messages body we parse for
// the input-token baseline. Everything else is forwarded verbatim and ignored
// here. System and Content are json.RawMessage because each is "string OR
// array of content blocks" in the Anthropic schema.
type anthropicRequest struct {
	Model    string          `json:"model"`
	System   json.RawMessage `json:"system"`
	Tools    json.RawMessage `json:"tools"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// logInputBaseline parses the request body's messages array and logs a
// per-request input-token count so later tickets can measure a before/after
// delta. It is best-effort: a parse failure logs a coarse fallback (token
// count over the whole body) and never affects forwarding. Bodies are never
// logged — only counts and shape, per the no-body-logging posture.
func logInputBaseline(logger *slog.Logger, r *http.Request, body []byte) {
	defer func() {
		// Token counting walks untrusted JSON; a panic here must never take
		// the request down. Fail open.
		if rec := recover(); rec != nil {
			logger.Warn("dex proxy: token baseline panicked; forwarding unaffected", "recover", rec)
		}
	}()

	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// Couldn't parse the Anthropic shape — still emit an honest coarse
		// count so the baseline isn't a silent gap.
		fam := tokens.Cl100k
		logger.Info("dex proxy request",
			"path", r.URL.Path,
			"input_tokens", tokens.CountFor(string(body), fam),
			"counted", "raw_body_fallback",
			"parse_err", err.Error())
		return
	}

	fam := tokens.Detect(req.Model) // Claude → cl100k; honest per-family count
	var b strings.Builder
	extractText(&b, req.System)
	for _, m := range req.Messages {
		extractText(&b, m.Content)
	}
	msgTokens := tokens.CountFor(b.String(), fam)

	var toolTokens int
	if len(req.Tools) > 0 {
		toolTokens = tokens.CountFor(string(req.Tools), fam)
	}

	logger.Info("dex proxy request",
		"model", req.Model,
		"messages", len(req.Messages),
		"input_tokens", msgTokens+toolTokens,
		"message_tokens", msgTokens,
		"tool_tokens", toolTokens,
		"tokenizer", fam.String())
}

// extractText flattens an Anthropic "string OR array of content blocks" field
// into plain text for token counting. Strings pass through; arrays contribute
// the human-readable leaves of each block (text, tool_result content, the
// JSON of tool_use input). Unparseable input contributes its raw bytes rather
// than vanishing — a baseline that silently drops content would understate the
// real token cost the follow-up compression must beat.
func extractText(b *strings.Builder, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	// Fast path: a bare JSON string ("system": "you are...").
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		b.WriteString(s)
		b.WriteByte('\n')
		return
	}
	// Array of content blocks.
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		// Unknown shape — count its bytes rather than dropping it.
		b.Write(raw)
		b.WriteByte('\n')
		return
	}
	for _, blk := range blocks {
		writeBlockText(b, blk)
	}
}

// writeBlockText pulls the token-bearing leaves out of one content block.
func writeBlockText(b *strings.Builder, blk map[string]json.RawMessage) {
	// text blocks: {"type":"text","text":"..."}
	if txt, ok := blk["text"]; ok {
		var s string
		if json.Unmarshal(txt, &s) == nil {
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	// tool_use blocks carry their args under "input" (arbitrary JSON).
	if in, ok := blk["input"]; ok {
		b.Write(in)
		b.WriteByte('\n')
	}
	// tool_result blocks nest "content" — itself string OR array of blocks.
	if c, ok := blk["content"]; ok {
		extractText(b, c)
	}
}
