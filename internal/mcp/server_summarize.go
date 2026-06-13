package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/heatmap"
	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: view_summarize ─────────────────────────────────────────────────

type SummarizeInput struct {
	Path         string   `json:"path,omitempty" jsonschema:"file path to summarize; relative paths are resolved against project_root; required when paths is not set"`
	Paths        []string `json:"paths,omitempty" jsonschema:"batch mode: list of files (max 10); all use the same mode; path is ignored when paths is non-empty"`
	ProjectRoot  string   `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Mode         string   `json:"mode,omitempty" jsonschema:"read fidelity: 'full' (default, summarize via LLM), 'skeleton' (exported signatures + @B<n> body handles, no LLM), 'signatures' (indexed symbols + source lines, no LLM), 'map' (imports + exported symbols from index, no LLM), 'lines:N-M' (raw line slice, no LLM)"`
	StartLine    int      `json:"start_line,omitempty" jsonschema:"first line to summarize (1-indexed, inclusive); 0 = beginning of file"`
	EndLine      int      `json:"end_line,omitempty" jsonschema:"last line to summarize (1-indexed, inclusive); 0 = end of file"`
	Focus        string   `json:"focus,omitempty" jsonschema:"optional steering — e.g. 'public API surface', 'side effects', 'error handling'"`
	Temperature  float32  `json:"temperature,omitempty" jsonschema:"sampling temperature (0 = server default)"`
	MaxTokens    int      `json:"max_tokens,omitempty" jsonschema:"maximum tokens to generate (0 = server default)"`
	Etag         string   `json:"etag,omitempty" jsonschema:"content hash from a prior read; if the file is unchanged the server returns status=unchanged — re-use the content already in context instead of re-reading"`
	BudgetTokens int      `json:"budget_tokens,omitempty" jsonschema:"optional remaining context budget in tokens; when set, dex auto-downgrades mode to fit (full→skeleton→signatures→map→handle) — omit for no budget constraint"`
	Task         string   `json:"task,omitempty" jsonschema:"optional current task description (e.g. from the session tool); when set, dex selects the compression level automatically — Generate/Test tasks use aggressive (no LLM), others use lightweight cleanup"`
	// CacheLayout overrides the profile's cache_layout knob for this call.
	// Values: "stable_first" (default), "recency", "off". Empty means use profile default.
	CacheLayout string `json:"cache_layout,omitempty" jsonschema:"batch ordering policy for prompt-cache hits: stable_first (session-seen files first), recency (caller order), off"`
	// Expand retrieves a suppressed function/method body from a previous skeleton-mode
	// read. Pass the handle key from the skeleton output (e.g. "@B3").
	Expand string `json:"expand,omitempty" jsonschema:"expand a body handle issued by a previous skeleton-mode read, e.g. '@B3'; returns the full source lines for that scope"`
	// Handle is an expansion handle (#344) minted by find/ask/lookup. When set it
	// decodes to a concrete path + line range and supersedes path/paths/start_line/
	// end_line — the agent echoes the opaque token instead of constructing a
	// reference, so it can never read a path it invented. Distinct from `expand`,
	// which addresses suppressed bodies within one skeleton read.
	Handle string `json:"handle,omitempty" jsonschema:"expansion handle from a find/ask/lookup result (the result's 'handle' field); reads that exact range — supersedes path/paths/start_line/end_line"`
}

type SummarizeOutput struct {
	Status       string   `json:"status"` // "ok" | "unchanged" | "delta" | "chat-service-unreachable" | "bad-handle" | "error"
	Hint         string   `json:"hint,omitempty"`
	Project      string   `json:"project,omitempty"`
	Path         string   `json:"path,omitempty"` // resolved path, relative to project root
	Paths        []string `json:"paths,omitempty"`
	StartLine    int      `json:"start_line,omitempty"`
	EndLine      int      `json:"end_line,omitempty"`
	Bytes        int      `json:"bytes,omitempty"`     // how many bytes were sent to the model
	Truncated    bool     `json:"truncated,omitempty"` // true if the slice was cut to fit the cap
	Model        string   `json:"model,omitempty"`
	Endpoint     string   `json:"endpoint,omitempty"`
	Content      string   `json:"content,omitempty"`
	FinishReason string   `json:"finish_reason,omitempty"`
	Etag         string   `json:"etag,omitempty"` // sha256[:16] of file content; pass back on re-reads
	// StablePrefixTokens is the estimated token count of the stable-prefix
	// section when cache_layout=stable_first reordering was applied to a batch
	// call. Zero for single-file calls or when no stable files were found.
	// Place the Anthropic cache_control breakpoint after this many tokens from
	// the start of the response to maximise prompt-cache hits.
	StablePrefixTokens int `json:"stable_prefix_tokens,omitempty"`
}

// maxSummarizeBytes caps the slice we send to the chat endpoint. Above
// this the local model's quality drops sharply and latency spikes;
// callers wanting a whole-repo overview should use ask_codebase with
// RAG instead. Tuned to fit comfortably in a 32B-coder context window
// alongside the system prompt and the summary itself.
const maxSummarizeBytes = 64 * 1024

func (s *Server) summarize(ctx context.Context, req *sdk.CallToolRequest, in SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error) { //nolint:cyclop
	out := SummarizeOutput{}

	// Expansion handle (#344): decode it to a concrete path + line range that
	// supersedes any path/paths/lines the caller also passed. A token that fails
	// to decode is rejected here (status="bad-handle") so a hallucinated handle
	// never reaches the filesystem. Existence of the decoded path is enforced
	// downstream by the normal path resolution + stat below.
	if h := strings.TrimSpace(in.Handle); h != "" {
		path, start, end, ok := DecodeHandle(h)
		if !ok {
			return nil, SummarizeOutput{Status: "bad-handle", Hint: "handle did not decode to a valid path:line range; re-run find/ask to get a fresh handle"}, nil
		}
		in.Path = path
		in.StartLine = start
		in.EndLine = end
		in.Paths = nil
		// #355 F2: a handle encodes an exact range to read. Pin a lines: mode so
		// the profile/task resolution below can't downgrade to a whole-file
		// compressed view (signatures/map/aggressive/skeleton) that silently
		// drops the range — only the `full` and `lines:` branches honor the
		// decoded StartLine/EndLine. A lines: pin is chat-independent (unlike
		// `full`, which the lean profile downgrades back to `map`, re-dropping
		// the range) and returns exactly the requested slice. An explicit caller
		// mode still wins: the handle supersedes path/lines, not a deliberate
		// mode choice.
		if strings.TrimSpace(in.Mode) == "" {
			in.Mode = fmt.Sprintf("lines:%d-%d", start, end)
		}
	}

	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		// Apply profile default_mode when no explicit mode was passed.
		if in.ProjectRoot != "" {
			if prof := profiles.Active(in.ProjectRoot); prof.Read.DefaultMode != "" {
				mode = prof.Read.DefaultMode
			}
		}
		if mode == "" {
			mode = "full"
		}
	}
	isFull := mode == "full"

	// Task-aware mode selection (#130): when the caller declares a task and
	// hasn't forced a specific mode, override to the most appropriate compression.
	// Generate/Test → aggressive (no LLM, comments stripped); others stay as-is.
	if in.Task != "" && mode == "full" {
		if override := compress.TaskToMode(in.Task); override != "" {
			// Adaptive policy (#109): if this (intent, mode) pair has been penalized
			// by prior output-ratio feedback, downgrade to a less lossy mode.
			if p2, h2 := s.resolveProject(in.ProjectRoot); h2 == "" {
				pt := compress.LoadPolicy(p2.CacheDir)
				override = pt.ChooseMode(compress.IntentFromTask(in.Task), override)
			}
			mode = override
			isFull = false
		}
	}

	if isFull && s.ChatClient == nil {
		mode = "map"
		isFull = false
	}
	if len(in.Paths) > 0 {
		return s.summarizeBatch(ctx, in)
	}
	if strings.TrimSpace(in.Path) == "" {
		return nil, SummarizeOutput{Status: "error", Hint: "path is empty"}, nil
	}
	root := in.ProjectRoot
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: "could not determine project root; pass project_root explicitly"}, nil
		}
		root = wd
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
		if tr.ConsumeThrottle() && mode == "full" {
			// Throttle: downgrade full→signatures to reduce token output.
			mode = "signatures"
			isFull = false
		}
		if blockMsg := sloBlock(tr.Check()); blockMsg != "" {
			return nil, SummarizeOutput{Status: "error", Hint: blockMsg}, nil
		}
	}

	if isFull {
		out.Endpoint = s.ChatClient.Endpoint()
		out.Model = s.ChatClient.ModelName()
	}

	// Resolve path under the project root. Reject anything that
	// escapes it (so an MCP caller can't read /etc/passwd by passing
	// "/etc/passwd" or "../../etc/passwd").
	target := in.Path
	if !filepath.IsAbs(target) {
		target = filepath.Join(p.Root, target)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("file does not exist: %s", target)}, nil
		}
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("resolve path: %v", err)}, nil
	}
	relTarget, err := filepath.Rel(p.Root, realTarget)
	if err != nil || strings.HasPrefix(relTarget, "..") || relTarget == ".." {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("path %s is outside project root %s", target, p.Root)}, nil
	}
	// Heatmap recording (#108): on every successful file_view, record the
	// access and compression savings. Fires after the function returns so
	// out.Bytes and out.Status are final. Best-effort — never blocks the read.
	cacheDir := p.CacheDir
	sloTracker := s.sloFor(p.Root)
	defer func() {
		if out.Status != "ok" {
			return
		}
		hm := heatmap.Load(cacheDir)
		origTok := out.Bytes / 4
		compTok := len(out.Content) / 4
		saved := origTok - compTok
		if saved < 0 {
			saved = 0
		}
		hm.RecordAccess(relTarget, origTok, saved)
		_ = hm.Save(cacheDir)

		// SLO: record output tokens and append any warn annotations.
		sloTracker.RecordTokens(len(out.Content) / 4)
		if ann := sloAnnotation(sloTracker.Check()); ann != "" {
			if out.Hint == "" {
				out.Hint = ann
			} else {
				out.Hint += " " + ann
			}
		}
	}()
	fi, err := os.Stat(realTarget)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("stat: %v", err)}, nil
	}
	if fi.IsDir() {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("%s is a directory — pass a file path", relTarget)}, nil
	}
	out.Path = relTarget

	data, err := os.ReadFile(realTarget)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("read: %v", err)}, nil
	}

	h := sha256.Sum256(data)
	etag := hex.EncodeToString(h[:])[:16]

	sessionID := "stdio" // fallback: stdio transport returns "" from ID()
	if req != nil && req.Session != nil {
		if id := req.Session.ID(); id != "" {
			sessionID = id
		}
	}
	if in.Etag != "" && in.Etag == etag && s.readCacheCheck(sessionID, relTarget, etag) {
		return nil, SummarizeOutput{Status: "unchanged", Project: out.Project, Path: relTarget, Etag: etag}, nil
	}

	// Delta re-read (#217): file changed since last delivery — try a compact unified diff.
	// Skip when the caller is expanding a body handle (handled below) or mode=full with
	// a live chat client (LLM summary; diff of raw bytes != diff of two summaries).
	if in.Expand == "" && !(isFull && s.ChatClient != nil) {
		if prevData, ok := s.readCacheGetContent(sessionID, relTarget); ok {
			if delta, worth := computeLineDelta(prevData, data); worth {
				out.Status = "delta"
				out.Etag = etag
				out.Bytes = len(data)
				out.Content = delta
				s.readCacheMark(sessionID, relTarget, etag)
				s.readCacheSetContent(sessionID, relTarget, data)
				return nil, out, nil
			}
		}
	}
	// Store raw bytes so future changed re-reads can produce a delta.
	defer func() {
		if out.Status == "ok" {
			s.readCacheSetContent(sessionID, relTarget, data)
		}
	}()

	// Body handle expansion (#206): @B<n> handle from a prior skeleton-mode read.
	if in.Expand != "" {
		h, ok := s.lookupBodyHandle(sessionID, in.Expand)
		if !ok {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("unknown handle %q — issue a skeleton-mode read first", in.Expand)}, nil
		}
		if h.etag != etag {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("file has changed since handle %q was issued — re-read with mode=skeleton", in.Expand)}, nil
		}
		slice, sliceStart, sliceEnd := sliceLines(data, h.startLine, h.endLine)
		out.Status = "ok"
		out.Etag = etag
		out.StartLine = sliceStart
		out.EndLine = sliceEnd
		out.Bytes = len(slice)
		out.Content = string(slice)
		out.Hint = fmt.Sprintf("body expansion of %s (lines %d-%d)", in.Expand, sliceStart, sliceEnd)
		s.readCacheMark(sessionID, relTarget, etag)
		return nil, out, nil
	}

	// Bounce detection (#98): if this file was recently delivered compressed
	// and the agent is re-requesting it, escalate to full mode.
	bt := s.bt()
	bt.recordRead(sessionID, relTarget)
	if bt.shouldForceFull(sessionID, relTarget) && mode != "full" {
		mode = "full"
		isFull = mode == "full" && s.ChatClient != nil
	}

	// Budget-aware downgrade (#106): auto-select the richest mode that fits
	// within the caller's remaining context budget. No-op when BudgetTokens=0.
	if in.BudgetTokens > 0 && !isFull {
		fileTokens := len(data) / 4 // ~4 bytes per token (rough approximation)
		mode = selectAffordableMode(mode, fileTokens, in.BudgetTokens)
	}

	// Dependency manifest shortcut (#125): for package.json, go.mod, Cargo.toml,
	// etc. return a compact summary directly — 10-50× token reduction.
	if compress.IsDepsFilename(filepath.Base(realTarget)) && mode != "full" {
		if summary, ok := compress.CompressDepsFile(relTarget, data); ok {
			out.Status = "ok"
			out.Etag = etag
			out.Bytes = len(data)
			out.Content = summary
			s.readCacheMark(sessionID, relTarget, etag)
			return nil, out, nil
		}
	}

	switch {
	case strings.HasPrefix(mode, "lines:"):
		rest := strings.TrimPrefix(mode, "lines:")
		start, end, ok := parseLinesRange(rest)
		if !ok {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("invalid lines mode %q — expected lines:N-M (e.g. lines:10-40)", in.Mode)}, nil
		}
		slice, sliceStart, sliceEnd := sliceLines(data, start, end)
		if sliceStart > sliceEnd {
			fileLines := chunk.LineCount(data)
			return nil, SummarizeOutput{
				Status: "error",
				Hint:   fmt.Sprintf("line range %d-%d is past EOF (file has %d lines)", start, end, fileLines),
			}, nil
		}
		out.StartLine = sliceStart
		out.EndLine = sliceEnd
		out.Bytes = len(slice)
		out.Status = "ok"
		out.Etag = etag
		out.Content = string(slice)
		s.readCacheMark(sessionID, relTarget, etag)
		s.sessionAutoFile(p.DBPath, relTarget)
		return nil, out, nil

	case mode == "signatures":
		st, err := s.openStore(p.DBPath)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
		}
		syms, err := st.SymbolsByFile(ctx, relTarget)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("symbol query: %v", err)}, nil
		}
		if len(syms) == 0 {
			out.Status = "ok"
			out.Hint = "no indexed symbols for this file — run `dex index` first or use mode=full"
			return nil, out, nil
		}
		content := formatSignatures(data, syms, relTarget, nil)
		if related := graphRelatedHint(ctx, st, relTarget); related != "" {
			content += related
		}
		// N16: inline best task-relevant symbol body when a session task is declared.
		content = inlineTaskSymbol(ctx, st, data, syms, content)
		out.Status = "ok"
		out.Etag = etag
		out.Content = content
		out.Bytes = len(content)
		s.readCacheMark(sessionID, relTarget, etag)
		bt.recordCompressed(sessionID, relTarget)
		s.sessionAutoFile(p.DBPath, relTarget)
		return nil, out, nil

	case mode == "map":
		// N14: non-code files get a pure-Go structural outline; no index needed.
		if content, ok := compress.NonCodeMap(relTarget, data); ok {
			out.Status = "ok"
			out.Etag = etag
			out.Content = content
			out.Bytes = len(content)
			s.readCacheMark(sessionID, relTarget, etag)
			return nil, out, nil
		}
		st, err := s.openStore(p.DBPath)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
		}
		syms, err := st.SymbolsByFile(ctx, relTarget)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("symbol query: %v", err)}, nil
		}
		imports, err := st.ImportsForFile(ctx, relTarget)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("import query: %v", err)}, nil
		}
		if len(syms) == 0 && len(imports) == 0 {
			out.Status = "ok"
			out.Hint = "no indexed data for this file — run `dex index` first or use mode=full"
			return nil, out, nil
		}
		content := formatMap(relTarget, syms, imports)
		if related := graphRelatedHint(ctx, st, relTarget); related != "" {
			content += related
		}
		// N16: inline best task-relevant symbol body when a session task is declared.
		if len(syms) > 0 {
			content = inlineTaskSymbol(ctx, st, data, syms, content)
		}
		out.Status = "ok"
		out.Etag = etag
		out.Content = content
		out.Bytes = len(content)
		s.readCacheMark(sessionID, relTarget, etag)
		bt.recordCompressed(sessionID, relTarget)
		s.sessionAutoFile(p.DBPath, relTarget)
		return nil, out, nil

	case mode == "aggressive":
		ext := filepath.Ext(realTarget)
		// Weak target_model profiles get the anchor-verbatim floor (#291).
		strict := profiles.Active(p.Root).StrictAnchors()
		content := compress.CompressCode(string(data), ext, strict)
		// Semantic chunk reordering (#105): when a task is provided, reorder
		// compressed content so the most task-relevant blocks appear first.
		if in.Task != "" {
			content = applySemanticChunkOrder(content, in.Task)
		}
		out.Status = "ok"
		out.Etag = etag
		out.Content = content
		out.Bytes = len(content)
		origLines := bytes.Count(data, []byte("\n")) + 1
		compLines := strings.Count(content, "\n") + 1
		if origLines > compLines {
			out.Hint = fmt.Sprintf("aggressive: %d → %d lines (%.0f%% reduction)",
				origLines, compLines, float64(origLines-compLines)*100/float64(origLines))
		}
		s.readCacheMark(sessionID, relTarget, etag)
		bt.recordCompressed(sessionID, relTarget)
		s.sessionAutoFile(p.DBPath, relTarget)
		return nil, out, nil

	case mode == "skeleton":
		// Skeleton mode (#206): exported type declarations in full; exported
		// function/method bodies replaced with @B<n> handles; unexported omitted.
		// Falls back to signatures when the index has no symbols.
		st, err := s.openStore(p.DBPath)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
		}
		syms, err := st.SymbolsByFile(ctx, relTarget)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("symbol query: %v", err)}, nil
		}
		if len(syms) == 0 {
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
		res := compress.SkeletonPass(data, relTarget, scopes)
		s.registerBodyHandles(sessionID, relTarget, etag, res.Bodies)
		out.Status = "ok"
		out.Etag = etag
		out.Content = res.Text
		out.Bytes = len(res.Text)
		s.readCacheMark(sessionID, relTarget, etag)
		bt.recordCompressed(sessionID, relTarget)
		s.sessionAutoFile(p.DBPath, relTarget)
		return nil, out, nil

	default: // full
		slice, sliceStart, sliceEnd := sliceLines(data, in.StartLine, in.EndLine)
		out.StartLine = sliceStart
		out.EndLine = sliceEnd
		if len(slice) > maxSummarizeBytes {
			slice = slice[:maxSummarizeBytes]
			out.Truncated = true
		}
		out.Bytes = len(slice)

		if lineCount := bytes.Count(data, []byte("\n")) + 1; lineCount > 250 {
			out.Hint = fmt.Sprintf("⚠ Large file (%d lines): pass mode=skeleton, mode=signatures, or mode=map to reduce tokens.", lineCount)
		}

		system := buildSummarizeSystem(in.Focus)
		cleaned := compress.LightweightCleanup(string(slice))
		userContent := fmt.Sprintf("FILE: %s (lines %d-%d)\n\n```\n%s\n```",
			relTarget, sliceStart, sliceEnd, cleaned)

		chatMsgs := []chat.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: userContent},
		}
		chatOpts := chat.Options{Temperature: in.Temperature, MaxTokens: in.MaxTokens}
		var resp chat.Response
		if req != nil && req.Session != nil {
			sess := req.Session
			resp, err = s.ChatClient.GenerateStream(ctx, chatMsgs, chatOpts, func(tok string) {
				_ = sess.Log(ctx, &sdk.LoggingMessageParams{
					Level:  "debug",
					Logger: "dex/file_view",
					Data:   tok,
				})
			})
		} else {
			resp, err = s.ChatClient.Generate(ctx, chatMsgs, chatOpts)
		}
		if err != nil {
			hint := fmt.Sprintf("chat error (%v) — showing raw content", err)
			if errors.Is(err, chat.ErrUnreachable) {
				hint = "chat service offline — showing raw content"
			}
			out.Status = "ok"
			out.Etag = etag
			out.Content = string(slice)
			out.Hint = hint
			s.readCacheMark(sessionID, relTarget, etag)
			return nil, out, nil
		}

		out.Status = "ok"
		out.Etag = etag
		out.Content = resp.Content
		out.FinishReason = resp.FinishReason
		if resp.Model != "" {
			out.Model = resp.Model
		}
		s.readCacheMark(sessionID, relTarget, etag)
		s.activityRecord(p.Root, 1)
		return nil, out, nil
	}
}

// summarizeBatch handles file_view when paths[] is provided.
// All files are processed with the same mode in a single call.
// When 3+ files are successfully read, a TF-IDF codebook is applied to
// replace repeated lines (imports, boilerplate) with short §N refs.
func (s *Server) summarizeBatch(ctx context.Context, in SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error) {
	const maxBatch = 10
	if len(in.Paths) > maxBatch {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("batch too large: max %d files per call, got %d", maxBatch, len(in.Paths))}, nil
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "signatures"
	}

	// Stable-first layout: load session-stable file set before the per-file
	// loop so we can annotate results as they come in.
	stableSet := batchStableSet(ctx, in.ProjectRoot, s.IndexDir)

	type fileResult struct {
		path    string // resolved path (or "" for errors)
		header  string // "## <path>" or "## <rawPath>\n⚠ <hint>"
		content string // file content (empty for errors)
		ok      bool
		stable  bool // session-seen before this turn
	}

	results := make([]fileResult, 0, len(in.Paths))
	var project string

	for _, rawPath := range in.Paths {
		single := in
		single.Path = rawPath
		single.Paths = nil
		single.Mode = mode
		_, out, err := s.summarize(ctx, nil, single)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("%s: %v", rawPath, err)}, nil
		}
		if project == "" {
			project = out.Project
		}
		if out.Status != "ok" {
			results = append(results, fileResult{
				header: fmt.Sprintf("## %s\n⚠ %s", rawPath, out.Hint),
			})
			continue
		}
		results = append(results, fileResult{
			path:    out.Path,
			header:  fmt.Sprintf("## %s", out.Path),
			content: out.Content,
			ok:      true,
			stable:  stableSet[out.Path],
		})
	}

	// cache_layout=stable_first (default): move session-stable files to the
	// front so the Anthropic prompt cache can build a consistent prefix across
	// turns. Preserve relative order within each tier. No-op when no session
	// exists, only one file, or the profile opts out.
	// Per-call override wins; fall back to profile; fall back to stable_first.
	layout := in.CacheLayout
	if layout == "" {
		layout = profiles.Active(in.ProjectRoot).Read.CacheLayout
	}
	if layout == "" {
		layout = "stable_first" // default
	}
	if layout == "stable_first" && len(results) > 1 {
		stable := results[:0:0]
		fresh := results[:0:0]
		for _, r := range results {
			if r.ok && r.stable {
				stable = append(stable, r)
			} else {
				fresh = append(fresh, r)
			}
		}
		results = append(stable, fresh...)
	}

	// Build codebook from successfully-read file contents.
	var fileContents []string
	for _, r := range results {
		if r.ok {
			fileContents = append(fileContents, r.content)
		}
	}
	cb := compress.BuildCodebook(fileContents)

	var sb strings.Builder
	if !cb.Empty() {
		sb.WriteString(cb.Legend())
		sb.WriteByte('\n')
	}

	var resolvedPaths []string
	var stablePrefixTokens int
	countingStable := layout == "stable_first"
	for _, r := range results {
		if r.ok {
			content := cb.Apply(r.content)
			chunk := fmt.Sprintf("%s\n%s\n\n", r.header, content)
			sb.WriteString(chunk)
			resolvedPaths = append(resolvedPaths, r.path)
			if countingStable && r.stable {
				stablePrefixTokens += compress.EstimateTokens(chunk)
			} else {
				countingStable = false // stop counting once we hit a fresh file
			}
		} else {
			fmt.Fprintf(&sb, "%s\n\n", r.header)
			if countingStable {
				countingStable = false
			}
		}
	}

	return nil, SummarizeOutput{
		Status:             "ok",
		Project:            project,
		Content:            strings.TrimRight(sb.String(), "\n"),
		Paths:              resolvedPaths,
		StablePrefixTokens: stablePrefixTokens,
	}, nil
}

// batchStableSet returns the set of project-relative file paths that appear
// in the current session — these are "session-stable" for cache layout
// purposes. Returns an empty (non-nil) map when no session exists or on any
// error (failures are silently swallowed; this is a best-effort optimisation).
func batchStableSet(ctx context.Context, projectRoot, indexDir string) map[string]bool {
	root := projectRoot
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return map[string]bool{}
		}
	}
	p, err := proj.Resolve(root, indexDir)
	if err != nil {
		return map[string]bool{}
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		return map[string]bool{}
	}
	ss, ok, err := st.SessionGet(ctx)
	if err != nil || !ok {
		return map[string]bool{}
	}
	set := make(map[string]bool, len(ss.Files))
	for _, f := range ss.Files {
		set[f.Path] = true
	}
	return set
}

// parseLinesRange parses "N-M" from a lines:N-M mode string.
func parseLinesRange(s string) (start, end int, ok bool) {
	i := strings.IndexByte(s, '-')
	if i <= 0 {
		return 0, 0, false
	}
	n, err1 := strconv.Atoi(s[:i])
	m, err2 := strconv.Atoi(s[i+1:])
	if err1 != nil || err2 != nil || n < 1 || m < n {
		return 0, 0, false
	}
	return n, m, true
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

// formatSignatures produces a compact symbol index for a file.
// Each exported symbol gets its declaration line; unexported symbols are
// listed without source. Output is ~10× smaller than mode=full.
func formatSignatures(src []byte, syms []store.GraphSymbol, relPath string, _ []string) string {
	srcLines := bytes.Split(bytes.TrimRight(src, "\n"), []byte("\n"))
	totalLines := bytes.Count(src, []byte("\n")) + 1
	var b strings.Builder
	fmt.Fprintf(&b, "%s %dL (%d symbols)\n\n", relPath, totalLines, len(syms))

	isTypeKind := func(kind string) bool {
		return kind == "struct" || kind == "interface" || kind == "type"
	}
	// Only top-level named symbols (func/type/var/const) count as exported,
	// not struct fields, imports, or file-level nodes.
	exported := func(sym store.GraphSymbol) bool {
		if sym.Kind == "field" || sym.Kind == "import" || sym.Kind == "file" {
			return false
		}
		return len(sym.Name) > 0 && sym.Name[0] >= 'A' && sym.Name[0] <= 'Z'
	}
	writeSym := func(sym store.GraphSymbol) {
		si := sym.StartLine - 1
		exp := exported(sym)
		if exp {
			marker := "⊛"
			fmt.Fprintf(&b, "%s %s (lines %d-%d)\n", marker, sym.QualifiedName, sym.StartLine, sym.EndLine)
			if si >= 0 && si < len(srcLines) {
				b.Write(srcLines[si])
				b.WriteByte('\n')
			}
		} else {
			fmt.Fprintf(&b, "  %s %s (lines %d-%d)\n", sym.Kind, sym.QualifiedName, sym.StartLine, sym.EndLine)
		}
	}
	for _, sym := range syms {
		if isTypeKind(sym.Kind) {
			writeSym(sym)
		}
	}
	for _, sym := range syms {
		if !isTypeKind(sym.Kind) {
			writeSym(sym)
		}
	}
	return b.String()
}

// formatMap produces a compact dependency map for a file: its package-level
// imports and exported declarations, sourced from the index (no LLM, no file
// read). Unexported symbols are omitted so the output mirrors the public API.
func formatMap(relPath string, syms []store.GraphSymbol, imports []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FILE: %s\n\n", relPath)
	if len(imports) > 0 {
		b.WriteString("IMPORTS:\n")
		for _, imp := range imports {
			fmt.Fprintf(&b, "  %s\n", imp)
		}
		b.WriteByte('\n')
	}
	var exportedLines strings.Builder
	count := 0
	for _, sym := range syms {
		if len(sym.Name) == 0 || sym.Name[0] < 'A' || sym.Name[0] > 'Z' {
			continue
		}
		fmt.Fprintf(&exportedLines, "  %s %s (lines %d-%d)\n", sym.Kind, sym.QualifiedName, sym.StartLine, sym.EndLine)
		count++
	}
	if count > 0 {
		fmt.Fprintf(&b, "EXPORTS (%d):\n", count)
		b.WriteString(exportedLines.String())
	}
	return b.String()
}

// sliceLines returns the byte slice of `data` between lines start and
// end (both 1-indexed, inclusive). Zero values mean "from start of
// file" / "to end of file". Returned start/end are clamped to the
// actual file extents so the caller can echo back what was used.
func sliceLines(data []byte, start, end int) ([]byte, int, int) {
	if start <= 0 && end <= 0 {
		return data, 1, chunk.LineCount(data)
	}
	if start <= 0 {
		start = 1
	}
	// Walk newlines once. Cheap and avoids splitting the whole file.
	var (
		startByte = -1
		endByte   = len(data)
		line      = 1
	)
	if start == 1 {
		startByte = 0
	}
	for i := range data {
		if data[i] != '\n' {
			continue
		}
		line++
		if startByte < 0 && line == start {
			startByte = i + 1
		}
		if end > 0 && line > end {
			endByte = i + 1
			break
		}
	}
	if startByte < 0 {
		// `start` is past EOF — return empty slice but record extents.
		return nil, start, start - 1
	}
	if end <= 0 || end > line {
		end = line
	}
	return data[startByte:endByte], start, end
}

func buildSummarizeSystem(focus string) string {
	base := "You are a file summarizer. Given a single file (or slice), produce a tight, factual summary the reader can use as a substitute for opening the file. " +
		"Lead with one sentence on what the file is for. Then a short bulleted list of the central items the file defines or exposes — picking the framing that fits the file kind: " +
		"exported types/functions for source code, targets and variables for Makefiles, top-level keys for config (YAML/TOML/JSON), section headings for docs, etc. " +
		"Also note key invariants, side effects, or constraints, and any non-obvious dependencies or cross-references. " +
		"Quote identifiers and names verbatim. No prose padding, no apologies, no restating the prompt. " +
		"Keep under 200 words. For trivial files (license, .gitignore, simple stubs) a single sentence is fine."
	if strings.TrimSpace(focus) != "" {
		base += " Focus specifically on: " + strings.TrimSpace(focus) + "."
	}
	return base
}
