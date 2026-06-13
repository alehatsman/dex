package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) summarizeModeLines(w summarizeWork, mode string) (*sdk.CallToolResult, SummarizeOutput, error) {
	rest := strings.TrimPrefix(mode, "lines:")
	start, end, ok := parseLinesRange(rest)
	if !ok {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("invalid lines mode %q — expected lines:N-M (e.g. lines:10-40)", w.in.Mode)}, nil
	}
	slice, sliceStart, sliceEnd := sliceLines(w.data, start, end)
	if sliceStart > sliceEnd {
		fileLines := chunk.LineCount(w.data)
		return nil, SummarizeOutput{
			Status: "error",
			Hint:   fmt.Sprintf("line range %d-%d is past EOF (file has %d lines)", start, end, fileLines),
		}, nil
	}
	out := w.out
	out.StartLine = sliceStart
	out.EndLine = sliceEnd
	out.Bytes = len(slice)
	out.Status = "ok"
	out.Etag = w.etag
	out.Content = string(slice)
	s.readCacheMark(w.sessionID, w.relTarget, w.etag)
	s.sessionAutoFile(w.p.DBPath, w.relTarget)
	return nil, out, nil
}

func (s *Server) summarizeModeSignatures(w summarizeWork) (*sdk.CallToolResult, SummarizeOutput, error) {
	st, err := s.openStore(w.p.DBPath)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	syms, err := st.SymbolsByFile(w.ctx, w.relTarget)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("symbol query: %v", err)}, nil
	}
	if len(syms) == 0 {
		out := w.out
		out.Status = "ok"
		out.Hint = "no indexed symbols for this file — run `dex index` first or use mode=full"
		return nil, out, nil
	}
	content := formatSignatures(w.data, syms, w.relTarget, nil)
	if related := graphRelatedHint(w.ctx, st, w.relTarget); related != "" {
		content += related
	}
	content = inlineTaskSymbol(w.ctx, st, w.data, syms, content)
	out := w.out
	out.Status = "ok"
	out.Etag = w.etag
	out.Content = content
	out.Bytes = len(content)
	s.readCacheMark(w.sessionID, w.relTarget, w.etag)
	w.bt.recordCompressed(w.sessionID, w.relTarget)
	s.sessionAutoFile(w.p.DBPath, w.relTarget)
	return nil, out, nil
}

func (s *Server) summarizeModeMap(w summarizeWork) (*sdk.CallToolResult, SummarizeOutput, error) {
	if content, ok := compress.NonCodeMap(w.relTarget, w.data); ok {
		out := w.out
		out.Status = "ok"
		out.Etag = w.etag
		out.Content = content
		out.Bytes = len(content)
		s.readCacheMark(w.sessionID, w.relTarget, w.etag)
		return nil, out, nil
	}
	st, err := s.openStore(w.p.DBPath)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	syms, err := st.SymbolsByFile(w.ctx, w.relTarget)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("symbol query: %v", err)}, nil
	}
	imports, err := st.ImportsForFile(w.ctx, w.relTarget)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("import query: %v", err)}, nil
	}
	if len(syms) == 0 && len(imports) == 0 {
		out := w.out
		out.Status = "ok"
		out.Hint = "no indexed data for this file — run `dex index` first or use mode=full"
		return nil, out, nil
	}
	content := formatMap(w.relTarget, syms, imports)
	if related := graphRelatedHint(w.ctx, st, w.relTarget); related != "" {
		content += related
	}
	if len(syms) > 0 {
		content = inlineTaskSymbol(w.ctx, st, w.data, syms, content)
	}
	out := w.out
	out.Status = "ok"
	out.Etag = w.etag
	out.Content = content
	out.Bytes = len(content)
	s.readCacheMark(w.sessionID, w.relTarget, w.etag)
	w.bt.recordCompressed(w.sessionID, w.relTarget)
	s.sessionAutoFile(w.p.DBPath, w.relTarget)
	return nil, out, nil
}

func (s *Server) summarizeModeAggressive(w summarizeWork) (*sdk.CallToolResult, SummarizeOutput, error) {
	ext := filepath.Ext(w.realTarget)
	strict := profiles.Active(w.p.Root).StrictAnchors()
	content := compress.CompressCode(string(w.data), ext, strict)
	if w.in.Task != "" {
		content = applySemanticChunkOrder(content, w.in.Task)
	}
	out := w.out
	out.Status = "ok"
	out.Etag = w.etag
	out.Content = content
	out.Bytes = len(content)
	origLines := bytes.Count(w.data, []byte("\n")) + 1
	compLines := strings.Count(content, "\n") + 1
	if origLines > compLines {
		out.Hint = fmt.Sprintf("aggressive: %d → %d lines (%.0f%% reduction)",
			origLines, compLines, float64(origLines-compLines)*100/float64(origLines))
	}
	s.readCacheMark(w.sessionID, w.relTarget, w.etag)
	w.bt.recordCompressed(w.sessionID, w.relTarget)
	s.sessionAutoFile(w.p.DBPath, w.relTarget)
	return nil, out, nil
}

func (s *Server) summarizeModeSkeleton(w summarizeWork) (*sdk.CallToolResult, SummarizeOutput, error) {
	st, err := s.openStore(w.p.DBPath)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	syms, err := st.SymbolsByFile(w.ctx, w.relTarget)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("symbol query: %v", err)}, nil
	}
	if len(syms) == 0 {
		out := w.out
		out.Status = "ok"
		out.Hint = "no indexed symbols for this file — run `dex index` first or use mode=full"
		return nil, out, nil
	}
	scopes := make([]compress.BodyScope, 0, len(syms))
	for _, sym := range syms {
		exported := len(sym.Name) > 0 && sym.Name[0] >= 'A' && sym.Name[0] <= 'Z'
		scopes = append(scopes, compress.BodyScope{
			Name:      sym.QualifiedName,
			Kind:      sym.Kind,
			Exported:  exported,
			StartLine: sym.StartLine,
			EndLine:   sym.EndLine,
		})
	}
	res := compress.SkeletonPass(w.data, w.relTarget, scopes)
	s.registerBodyHandles(w.sessionID, w.relTarget, w.etag, res.Bodies)
	out := w.out
	out.Status = "ok"
	out.Etag = w.etag
	out.Content = res.Text
	out.Bytes = len(res.Text)
	s.readCacheMark(w.sessionID, w.relTarget, w.etag)
	w.bt.recordCompressed(w.sessionID, w.relTarget)
	s.sessionAutoFile(w.p.DBPath, w.relTarget)
	return nil, out, nil
}

// summarizeModeRaw returns the file's raw content — no LLM, no compression.
// This is the `full` mode (the default): predictable, cheap, exact bytes.
// StartLine/EndLine slice a sub-range; the whole file otherwise.
func (s *Server) summarizeModeRaw(w summarizeWork) (*sdk.CallToolResult, SummarizeOutput, error) {
	slice, sliceStart, sliceEnd := sliceLines(w.data, w.in.StartLine, w.in.EndLine)
	out := w.out
	out.StartLine = sliceStart
	out.EndLine = sliceEnd
	if len(slice) > maxSummarizeBytes {
		slice = slice[:maxSummarizeBytes]
		out.Truncated = true
	}
	out.Status = "ok"
	out.Etag = w.etag
	out.Bytes = len(slice)
	out.Content = string(slice)
	if lineCount := bytes.Count(w.data, []byte("\n")) + 1; lineCount > 250 {
		out.Hint = fmt.Sprintf("⚠ Large file (%d lines): pass mode=skeleton, mode=signatures, or mode=map to reduce tokens, or mode=summary for an LLM digest.", lineCount)
	}
	s.readCacheMark(w.sessionID, w.relTarget, w.etag)
	return nil, out, nil
}

// summarizeModeSummary is the LLM path (`mode=summary`): it sends the file
// slice to the chat model and returns the generated digest. On chat error it
// degrades to raw content.
func (s *Server) summarizeModeSummary(w summarizeWork) (*sdk.CallToolResult, SummarizeOutput, error) {
	slice, sliceStart, sliceEnd := sliceLines(w.data, w.in.StartLine, w.in.EndLine)
	out := w.out
	out.StartLine = sliceStart
	out.EndLine = sliceEnd
	if len(slice) > maxSummarizeBytes {
		slice = slice[:maxSummarizeBytes]
		out.Truncated = true
	}
	out.Bytes = len(slice)

	system := buildSummarizeSystem(w.in.Focus)
	cleaned := compress.LightweightCleanup(string(slice))
	userContent := fmt.Sprintf("FILE: %s (lines %d-%d)\n\n```\n%s\n```",
		w.relTarget, sliceStart, sliceEnd, cleaned)

	chatMsgs := []chat.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: userContent},
	}
	chatOpts := chat.Options{Temperature: w.in.Temperature, MaxTokens: w.in.MaxTokens}
	var resp chat.Response
	var err error
	if w.req != nil && w.req.Session != nil {
		sess := w.req.Session
		resp, err = s.ChatClient.GenerateStream(w.ctx, chatMsgs, chatOpts, func(tok string) {
			_ = sess.Log(w.ctx, &sdk.LoggingMessageParams{
				Level:  "debug",
				Logger: "dex/file_view",
				Data:   tok,
			})
		})
	} else {
		resp, err = s.ChatClient.Generate(w.ctx, chatMsgs, chatOpts)
	}
	if err != nil {
		hint := fmt.Sprintf("chat error (%v) — showing raw content", err)
		if errors.Is(err, chat.ErrUnreachable) {
			hint = "chat service offline — showing raw content"
		}
		out.Status = "ok"
		out.Etag = w.etag
		out.Content = string(slice)
		out.Hint = hint
		s.readCacheMark(w.sessionID, w.relTarget, w.etag)
		return nil, out, nil
	}

	out.Status = "ok"
	out.Etag = w.etag
	out.Content = resp.Content
	out.FinishReason = resp.FinishReason
	if resp.Model != "" {
		out.Model = resp.Model
	}
	s.readCacheMark(w.sessionID, w.relTarget, w.etag)
	s.activityRecord(w.p.Root, 1)
	return nil, out, nil
}

// graphRelatedHint returns a compact "Related (call graph): ..." line
// listing files graph-adjacent to relPath, or "" when the graph is absent
// or has no neighbors. Never fails — graph errors are silently swallowed.
func graphRelatedHint(ctx context.Context, st *store.Store, relPath string) string {
	neighbors, err := st.GraphNeighborFiles(ctx, []string{relPath}, 8)
	if err != nil || len(neighbors) == 0 {
		return ""
	}
	return "\n# Related (call graph): " + strings.Join(neighbors, ", ") + "\n"
}

// inlineTaskSymbol appends the body of the symbol most relevant to the active
// session task (if any) to content, so a task-focused read surfaces the code
// that matters even under a compressed mode. Moved here from the removed
// server_compose.go (#429) — it is the only live consumer.
func inlineTaskSymbol(ctx context.Context, st *store.Store, data []byte, syms []store.GraphSymbol, content string) string {
	sess, ok, err := st.SessionGet(ctx)
	if err != nil || !ok || sess.Task == "" {
		return content
	}
	queryTokens := tokenizeWords(sess.Task)
	if len(queryTokens) == 0 {
		return content
	}
	var bestSym store.GraphSymbol
	bestScore := 0
	for _, sym := range syms {
		if sc := symbolQueryScore(queryTokens, sym); sc > bestScore {
			bestScore = sc
			bestSym = sym
		}
	}
	if bestScore == 0 || data == nil {
		return content
	}
	endLine := bestSym.EndLine
	if endLine-bestSym.StartLine > 60 {
		endLine = bestSym.StartLine + 59
	}
	body, sLine, eLine := sliceLines(data, bestSym.StartLine, endLine)
	if len(body) == 0 {
		return content
	}
	return content + fmt.Sprintf("\n# Task-relevant: %s %s (lines %d-%d)\n```\n%s```\n",
		bestSym.Kind, bestSym.QualifiedName, sLine, eLine, string(body))
}

// symbol's qualified name tokens. 0 means no overlap.
func symbolQueryScore(queryTokens []string, sym store.GraphSymbol) int {
	symTokens := tokenizeWords(sym.QualifiedName)
	score := 0
	for _, qt := range queryTokens {
		for _, st := range symTokens {
			if qt == st {
				score++
			}
		}
	}
	return score
}

// tokenizeWords splits text into lowercase tokens (length > 2) breaking on
// non-alphanumeric characters and camelCase boundaries.
func tokenizeWords(s string) []string {
	var tokens []string
	var cur strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			cur.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			if cur.Len() > 2 {
				tokens = append(tokens, cur.String())
			}
			cur.Reset()
			cur.WriteRune(r + 32) // toLower
		case r >= '0' && r <= '9':
			cur.WriteRune(r)
		default:
			if cur.Len() > 2 {
				tokens = append(tokens, cur.String())
			}
			cur.Reset()
		}
	}
	if cur.Len() > 2 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}
