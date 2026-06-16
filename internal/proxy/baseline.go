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

// countBodyTokens parses a /v1/messages body and returns the estimated
// input-token count. Best-effort: falls back to a raw-byte count on parse
// failure. Returns 0 on empty input.
func countBodyTokens(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return int64(tokens.CountFor(string(body), tokens.Cl100k))
	}
	fam := tokens.Detect(req.Model)
	var b strings.Builder
	extractText(&b, req.System)
	for _, m := range req.Messages {
		extractText(&b, m.Content)
	}
	n := tokens.CountFor(b.String(), fam)
	if len(req.Tools) > 0 {
		n += tokens.CountFor(string(req.Tools), fam)
	}
	return int64(n)
}

// logRequestMetrics emits a single structured log line with token before/after
// counts and which compression paths fired. Bodies are never logged — only
// counts and path labels, per the no-body-logging posture.
func logRequestMetrics(logger *slog.Logger, r *http.Request, finalBody []byte, before, after int64, paths []string, cache CacheStats, toolDesc ToolDescStats, reReads ReReadStats) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Warn("dex proxy: metrics log panicked; forwarding unaffected", "recover", rec)
		}
	}()

	saved := before - after
	if saved < 0 {
		saved = 0
	}

	var model string
	var msgCount int
	var req anthropicRequest
	if err := json.Unmarshal(finalBody, &req); err == nil {
		model = req.Model
		msgCount = len(req.Messages)
	}

	pass := strings.Join(paths, "+")
	if pass == "" {
		pass = "passthrough"
	}

	attrs := []any{
		"path", r.URL.Path,
		"tokens_before", before,
		"tokens_after", after,
		"tokens_saved", saved,
		"pass", pass,
		"cache_breakpoints", cache.Breakpoints,
		"cache_efficiency", cache.Efficiency(),
		"tool_desc_mode", toolDesc.Mode.String(),
		"tool_descs_compressed", toolDesc.ToolsCompressed,
		"rereads_after_stub", reReads.ReReads,
		"reread_tokens", reReads.ReReadTokens,
	}
	if model != "" {
		attrs = append(attrs, "model", model, "messages", msgCount)
	}
	logger.Info("dex proxy request", attrs...)
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
