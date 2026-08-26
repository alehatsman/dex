package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/proj"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: view_summarize ─────────────────────────────────────────────────

func (s *Server) summarize(ctx context.Context, req *sdk.CallToolRequest, in SummarizeInput) (result *sdk.CallToolResult, out SummarizeOutput, err error) {

	// Handle decode (#344): a plain handle decodes to a concrete path+range,
	// returning a *SummarizeOutput on early-exit.
	in, bad := applyExpansionHandle(in)
	if bad != nil {
		return nil, *bad, nil
	}

	mode, isLLM := s.summarizeResolveMode(ctx, in)

	// Did the caller explicitly name a mode? An explicit request (incl. a
	// lines:N-M range) must win over the dependency-manifest shortcut below
	// (#511); only auto-selected modes (profile default / task hint / budget
	// downgrade, all of which leave in.Mode empty) may compact a manifest.
	explicitMode := strings.TrimSpace(in.Mode) != ""

	// An explicitly-requested mode the dispatch doesn't recognize must error
	// loudly: the switch's default arm serves the full raw file, so a typo'd or
	// CLI-only mode (e.g. "entropy") would silently blow the token budget (#528).
	// Auto-selected modes come from trusted sources and are always valid.
	if explicitMode && !ValidReadMode(mode) {
		// Give specific guidance for CLI-only modes (#768/#776).
		if mode == "entropy" {
			return nil, SummarizeOutput{
				Status: "error",
				Hint:   "mode \"entropy\" is CLI-only (drops low-information lines); use aggressive for a similar lossy pass via the MCP tool",
			}, nil
		}
		if mode == "auto" {
			return nil, SummarizeOutput{
				Status: "error",
				Hint:   "mode \"auto\" is CLI-only; omit the mode field to let dex auto-select based on file type and budget",
			}, nil
		}
		modes := ReadModes()
		for i, m := range modes {
			if m == "lines" {
				modes[i] = "lines:N-M"
			}
		}
		return nil, SummarizeOutput{
			Status: "error",
			Hint:   fmt.Sprintf("unrecognized read mode %q; valid: %s", mode, strings.Join(modes, ", ")),
		}, nil
	}

	if len(in.Paths) > 0 {
		return s.summarizeBatch(ctx, in)
	}
	if strings.TrimSpace(in.Path) == "" {
		return nil, SummarizeOutput{Status: "error", Hint: "path is empty"}, nil
	}
	root, rootErr := s.resolveProjectRoot(ctx, in.ProjectRoot)
	if rootErr != nil {
		return nil, SummarizeOutput{Status: "error", Hint: rootErr.Error()}, nil
	}
	p, err := proj.Resolve(root, s.IndexDir)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("resolve project: %v", err)}, nil
	}
	s.markForeground(p)
	out.Project = p.Root

	// SLO monitoring: record the tool call, check for throttle/block.
	{
		tr := s.sloFor(p.Root)
		tr.RecordToolCall()
		if tr.ConsumeThrottle() && mode == ReadModeSummary {
			mode = ReadModeSignatures
			isLLM = false
		}
		if blockMsg := sloBlock(tr.Check()); blockMsg != "" {
			return nil, SummarizeOutput{Status: "error", Hint: blockMsg}, nil
		}
	}

	// mode=summary is the only LLM path; the structural modes (full/raw,
	// signatures, skeleton, map, lines) need no chat model. Degrade with a
	// clear status when summary is requested but no chat model is wired.
	if isLLM && s.ChatClient == nil {
		out.Status = "needs-chat"
		out.Hint = "mode=summary needs a chat model (set DEX_CHAT_URL); use mode=full (raw) or signatures/skeleton/map for no-LLM reads"
		return nil, out, nil
	}

	s.setSummarizeModel(&out, isLLM)

	var realTarget, relTarget string
	var data []byte
	var etag string
	var earlyOut SummarizeOutput
	var done bool
	realTarget, relTarget, data, etag, earlyOut, done = s.summarizeReadFile(p, in.Path)
	if done {
		out = earlyOut
		out.Project = p.Root
		return
	}
	out.Path = relTarget

	// Two early exits fire right after the content is read, before the
	// cache/SLO/bounce machinery: a --ref time-travel read (#657) and a
	// binary-file refusal (#674). Both leave working-tree text reads
	// byte-identical — contained, opt-in branches.
	if early, done := s.summarizePostRead(ctx, in, realTarget, relTarget, data, mode); done {
		early.Project = p.Root
		return nil, early, nil
	}

	cacheDir := p.CacheDir
	sloTracker := s.sloFor(p.Root)
	defer s.recordSummarizeMetrics(cacheDir, sloTracker, relTarget, &out)

	var sessionID string
	sessionID, earlyOut, done = s.summarizeCheckCached(req, in, relTarget, etag, isLLM, data, out)
	if done {
		out = earlyOut
		return
	}
	defer func() {
		if out.Status == "ok" {
			s.readCacheSetContent(sessionID, relTarget, data)
		}
	}()

	if earlyOut, done = s.summarizeExpandHandle(in, data, etag, sessionID, relTarget, out); done {
		out = earlyOut
		return
	}

	// Surgical slice (#630): apply spec to file content and return immediately,
	// bypassing mode dispatch. This composes with handle-resolved ranges: a
	// handle narrows to a line range first, then slice extracts within it.
	if earlySlice, done := s.summarizeModeSlice(in, data, etag, sessionID, relTarget, out); done {
		out = earlySlice
		return
	}

	// Bounce detection (#98): re-escalate on repeated compressed reads.
	bt := s.bt()
	bt.recordRead(sessionID, relTarget)
	mode, isLLM = s.escalateOnBounce(bt, sessionID, relTarget, mode, isLLM)

	// Budget-aware downgrade (#106): auto-select richest mode within budget.
	// Skips the LLM summary (its output is already small); raw full is the
	// largest payload, so it is downgraded toward signatures/map as needed.
	if in.BudgetTokens > 0 && !isLLM {
		mode = selectAffordableMode(mode, len(data)/4, in.BudgetTokens)
	}

	// Dependency manifest shortcut (#125): compact summary for package.json etc.
	// Honor an explicit raw-full or LLM-summary request, and honor any other
	// explicitly-requested mode (e.g. lines:N-M, signatures) — the shortcut
	// only applies when the structural mode was auto-selected (#511).
	if !explicitMode && compress.IsDepsFilename(filepath.Base(realTarget)) && mode != ReadModeFull && mode != ReadModeSummary {
		if summary, ok := compress.CompressDepsFile(relTarget, data); ok {
			out.Status = "ok"
			out.Etag = etag
			out.Bytes = len(data)
			out.Content = summary
			s.readCacheMark(sessionID, relTarget, etag, string(mode))
			return nil, out, nil
		}
	}

	w := summarizeWork{
		ctx: ctx, req: req, in: in, p: p, data: data,
		realTarget: realTarget, relTarget: relTarget,
		sessionID: sessionID, etag: etag, bt: bt, out: out,
	}
	return s.summarizeModeDispatch(w, mode)
}

// summarizeModeDispatch routes a read to the concrete mode handler.
// The switch here is the single canonical mapping from ReadMode to handler.
func (s *Server) summarizeModeDispatch(w summarizeWork, mode ReadMode) (*sdk.CallToolResult, SummarizeOutput, error) {
	switch {
	case mode == ReadModeSummary:
		return s.summarizeModeSummary(w)
	case mode.IsLines():
		return s.summarizeModeLines(w, mode)
	case mode == ReadModeSignatures:
		return s.summarizeModeSignatures(w)
	case mode == ReadModeMap:
		return s.summarizeModeMap(w)
	case mode == ReadModeAggressive:
		return s.summarizeModeAggressive(w)
	case mode == ReadModeSkeleton:
		return s.summarizeModeSkeleton(w)
	case mode == ReadModeAnalyze:
		return s.summarizeModeAnalyze(w)
	case mode == ReadModeHandle:
		// Cheapest terminal of the budget downgrade chain (#487): compact
		// body-handle stub, never a fall-through to the full raw file.
		return s.summarizeModeHandle(w)
	default: // full — raw file content, no LLM, no compression.
		return s.summarizeModeRaw(w)
	}
}
