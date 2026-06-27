package mcp

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/summarize"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) summarizeModeLines(w summarizeWork, mode ReadMode) (*sdk.CallToolResult, SummarizeOutput, error) {
	rest := strings.TrimPrefix(string(mode), "lines:")
	start, end, ok := summarize.ParseLinesRange(rest)
	if !ok {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("invalid lines mode %q — expected lines:N-M, lines:N- (to EOF), lines:-M (first M), or lines:N (single), e.g. lines:10-40", w.in.Mode)}, nil
	}
	slice, sliceStart, sliceEnd := summarize.SliceLines(w.data, start, end)
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
	s.readCacheMark(w.sessionID, w.relTarget, w.etag, w.in.Mode)
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
	content := summarize.SignaturesView(w.ctx, st, w.data, syms, w.relTarget)
	out := w.out
	out.Status = "ok"
	out.Etag = w.etag
	out.Content = content
	out.Bytes = len(content)
	s.readCacheMark(w.sessionID, w.relTarget, w.etag, w.in.Mode)
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
		s.readCacheMark(w.sessionID, w.relTarget, w.etag, w.in.Mode)
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
	content := summarize.MapView(w.ctx, st, w.data, syms, imports, w.relTarget)
	out := w.out
	out.Status = "ok"
	out.Etag = w.etag
	out.Content = content
	out.Bytes = len(content)
	s.readCacheMark(w.sessionID, w.relTarget, w.etag, w.in.Mode)
	w.bt.recordCompressed(w.sessionID, w.relTarget)
	s.sessionAutoFile(w.p.DBPath, w.relTarget)
	return nil, out, nil
}

func (s *Server) summarizeModeAggressive(w summarizeWork) (*sdk.CallToolResult, SummarizeOutput, error) {
	ext := filepath.Ext(w.realTarget)
	strict := profiles.Active(w.p.Root).StrictAnchors()
	content := compress.CompressCode(string(w.data), ext, strict)
	if w.in.Task != "" {
		content = summarize.SemanticChunkOrder(content, w.in.Task)
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
	s.readCacheMark(w.sessionID, w.relTarget, w.etag, w.in.Mode)
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
	res := compress.SkeletonPass(w.data, w.relTarget, bodyScopesForSymbols(syms))
	_, remap := s.registerBodyHandles(w.sessionID, w.relTarget, w.etag, res.Bodies)
	out := w.out
	out.Status = "ok"
	out.Etag = w.etag
	out.Content = remap.Replace(res.Text)
	out.Bytes = len(out.Content)
	s.readCacheMark(w.sessionID, w.relTarget, w.etag, w.in.Mode)
	w.bt.recordCompressed(w.sessionID, w.relTarget)
	s.sessionAutoFile(w.p.DBPath, w.relTarget)
	return nil, out, nil
}

// summarizeModeHandle is the cheapest renderable terminal of the budget
// downgrade chain (#487): when even `map` exceeds budget_tokens,
// selectAffordableMode lands here. It emits a compact reference stub — the
// path, symbol count, and @Bn body handles — never the full raw file. It is
// deterministic (no LLM) and consistent with estimateModeTokens(handle)=25.
func (s *Server) summarizeModeHandle(w summarizeWork) (*sdk.CallToolResult, SummarizeOutput, error) {
	out := w.out
	out.Status = "ok"
	out.Etag = w.etag
	lineCount := bytes.Count(w.data, []byte("\n")) + 1

	st, err := s.openStore(w.p.DBPath)
	if err == nil {
		if syms, err := st.SymbolsByFile(w.ctx, w.relTarget); err == nil && len(syms) > 0 {
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
			remapped, _ := s.registerBodyHandles(w.sessionID, w.relTarget, w.etag, res.Bodies)
			// Compact by design: a one-line pointer plus a small sample of body
			// handles, never the full per-symbol list (that would blow the very
			// budget that routed us here). All handles remain resolvable via the
			// registry above; estimateModeTokens(handle)=25.
			const sample = 5
			names := make([]string, 0, sample)
			for _, b := range remapped {
				if len(names) == sample {
					break
				}
				names = append(names, fmt.Sprintf("%s @B%d", b.Name, b.N))
			}
			more := ""
			if len(remapped) > len(names) {
				more = fmt.Sprintf(" (+%d more handles @B%d…@B%d)", len(remapped)-len(names), len(names)+1, len(remapped))
			}
			out.Content = fmt.Sprintf("HANDLE %s (%d lines, %d symbols) — budget too small for a fuller view; request a body by @Bn handle or re-read with a larger budget_tokens\n%s%s",
				w.relTarget, lineCount, len(syms), strings.Join(names, "\n"), more)
			out.Bytes = len(out.Content)
			s.readCacheMark(w.sessionID, w.relTarget, w.etag, w.in.Mode)
			w.bt.recordCompressed(w.sessionID, w.relTarget)
			s.sessionAutoFile(w.p.DBPath, w.relTarget)
			return nil, out, nil
		}
	}

	// No index/symbols: still emit a compact pointer, never the raw file.
	out.Content = fmt.Sprintf("HANDLE %s (%d lines) — budget too small for a fuller view; re-read with a larger budget_tokens or mode=map",
		w.relTarget, lineCount)
	out.Bytes = len(out.Content)
	s.readCacheMark(w.sessionID, w.relTarget, w.etag, w.in.Mode)
	w.bt.recordCompressed(w.sessionID, w.relTarget)
	return nil, out, nil
}

// summarizeModeRaw returns the file's raw content — no LLM, no compression.
// This is the `full` mode (the default): predictable, cheap, exact bytes.
// StartLine/EndLine slice a sub-range; the whole file otherwise.
func (s *Server) summarizeModeRaw(w summarizeWork) (*sdk.CallToolResult, SummarizeOutput, error) {
	slice, sliceStart, sliceEnd := summarize.SliceLines(w.data, w.in.StartLine, w.in.EndLine)
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
	s.readCacheMark(w.sessionID, w.relTarget, w.etag, w.in.Mode)
	return nil, out, nil
}

// summarizeModeSummary is the LLM path (`mode=summary`): it sends the file
// slice to the chat model and returns the generated digest. On chat error it
// degrades to raw content.
func (s *Server) summarizeModeSummary(w summarizeWork) (*sdk.CallToolResult, SummarizeOutput, error) {
	slice, sliceStart, sliceEnd := summarize.SliceLines(w.data, w.in.StartLine, w.in.EndLine)
	out := w.out
	out.StartLine = sliceStart
	out.EndLine = sliceEnd
	if len(slice) > maxSummarizeBytes {
		slice = slice[:maxSummarizeBytes]
		out.Truncated = true
	}
	out.Bytes = len(slice)

	system := summarize.BuildSystem(w.in.Focus)
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
		s.readCacheMark(w.sessionID, w.relTarget, w.etag, w.in.Mode)
		return nil, out, nil
	}

	out.Status = "ok"
	out.Etag = w.etag
	out.Content = resp.Content
	out.FinishReason = resp.FinishReason
	if resp.Model != "" {
		out.Model = resp.Model
	}
	s.readCacheMark(w.sessionID, w.relTarget, w.etag, w.in.Mode)
	s.activityRecord(w.p.Root, 1)
	return nil, out, nil
}

// graphRelatedHint, inlineTaskSymbol, symbolQueryScore, and tokenizeWords
// moved to internal/summarize (#472 step 3).
