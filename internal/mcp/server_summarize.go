package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
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
	Mode         string   `json:"mode,omitempty" jsonschema:"read mode (default 'full'): 'full' (raw file content, no LLM), 'signatures' (indexed symbols + source lines, no LLM), 'skeleton' (exported type decls in full + function/method signatures with @B<n> body handles, no LLM), 'map' (imports + exported symbols from index, no LLM), 'lines:N-M' (raw line slice, no LLM), 'analyze' (token-cost comparison of every mode + a recommended mode, NO file content — pick the cheapest sufficient view before paying to read it), 'summary' (LLM-generated digest — the only mode needing a chat model; returns status='needs-chat' when none is wired)"`
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
	// Dedup controls Go import block deduplication in multi-file (paths[]) reads.
	// Default (omitted / true): the union of all import blocks is emitted once as a
	// shared header and each file's block is replaced with a back-reference comment.
	// Pass false to receive raw per-file output without any deduplication.
	Dedup *bool `json:"dedup,omitempty" jsonschema:"set to false to disable Go import deduplication in batch reads (default: true, dedup is on for full/signatures modes with ≥2 Go files)"`
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
	// Analysis is populated only by mode=analyze (#623): a per-mode token-cost
	// comparison + recommended mode, with no file content.
	Analysis *ReadAnalysis `json:"analysis,omitempty"`
	// StablePrefixTokens is the estimated token count of the stable-prefix
	// section when cache_layout=stable_first reordering was applied to a batch
	// call. Zero for single-file calls or when no stable files were found.
	// Place the Anthropic cache_control breakpoint after this many tokens from
	// the start of the response to maximise prompt-cache hits.
	StablePrefixTokens int `json:"stable_prefix_tokens,omitempty"`
	// ImportsDedupSavedLines is the number of import lines removed from
	// per-file output by Go import deduplication. Zero when dedup was not
	// applied (single-file, fewer than 2 files had import blocks, or dedup
	// was explicitly disabled).
	ImportsDedupSavedLines int `json:"imports_dedup_saved_lines,omitempty"`
}

// maxSummarizeBytes caps the slice we send to the chat endpoint. Above
// this the local model's quality drops sharply and latency spikes;
// callers wanting a whole-repo overview should use ask_codebase with
// RAG instead. Tuned to fit comfortably in a 32B-coder context window
// alongside the system prompt and the summary itself.
const maxSummarizeBytes = 64 * 1024

// summarizeWork holds the resolved parameters for a single-file summarize call,
// shared across mode-specific helpers.
type summarizeWork struct {
	ctx        context.Context
	req        *sdk.CallToolRequest
	in         SummarizeInput
	p          *proj.Project
	data       []byte
	realTarget string
	relTarget  string
	sessionID  string
	etag       string
	bt         *bounceTracker
	out        SummarizeOutput
}

func (s *Server) summarize(ctx context.Context, req *sdk.CallToolRequest, in SummarizeInput) (result *sdk.CallToolResult, out SummarizeOutput, err error) {

	// Expansion handle (#344): decode to concrete path + line range, superseding
	// any path/paths/lines the caller also passed.
	in, bad := applyExpansionHandle(in)
	if bad != nil {
		return nil, *bad, nil
	}

	mode, isLLM := s.summarizeResolveMode(in)

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
		return nil, SummarizeOutput{
			Status: "error",
			Hint:   fmt.Sprintf("unrecognized read mode %q; valid: %s (use lines:N-M for a line range)", mode, strings.Join(ReadModes(), ", ")),
		}, nil
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

	if isLLM {
		out.Endpoint = s.ChatClient.Endpoint()
		out.Model = s.ChatClient.ModelName()
	}

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
			s.readCacheMark(sessionID, relTarget, etag)
			return nil, out, nil
		}
	}

	w := summarizeWork{
		ctx: ctx, req: req, in: in, p: p, data: data,
		realTarget: realTarget, relTarget: relTarget,
		sessionID: sessionID, etag: etag, bt: bt, out: out,
	}
	switch {
	case mode == ReadModeSummary:
		result, out, err = s.summarizeModeSummary(w)
	case mode.IsLines():
		result, out, err = s.summarizeModeLines(w, mode)
	case mode == ReadModeSignatures:
		result, out, err = s.summarizeModeSignatures(w)
	case mode == ReadModeMap:
		result, out, err = s.summarizeModeMap(w)
	case mode == ReadModeAggressive:
		result, out, err = s.summarizeModeAggressive(w)
	case mode == ReadModeSkeleton:
		result, out, err = s.summarizeModeSkeleton(w)
	case mode == ReadModeAnalyze:
		result, out, err = s.summarizeModeAnalyze(w)
	case mode == ReadModeHandle:
		// Cheapest terminal of the budget downgrade chain (#487): compact
		// body-handle stub, never a fall-through to the full raw file.
		result, out, err = s.summarizeModeHandle(w)
	default: // full — raw file content, no LLM, no compression.
		result, out, err = s.summarizeModeRaw(w)
	}
	return
}

// ReadModes returns the user-facing `read` modes the Summarize dispatch above
// handles, in documentation order. It is the canonical anchor for CLI↔MCP mode
// parity (cmd/dex/read_parity_test.go) — keep it in sync with the switch.
//
// `handle` is intentionally excluded: it is an internal terminal of the
// budget-downgrade chain (#487), never a mode an operator selects. `lines` is
// listed as a stand-in for the `lines:N-M` prefix family.
func ReadModes() []string {
	all := AllReadModes()
	out := make([]string, len(all))
	for i, m := range all {
		out[i] = string(m)
	}
	return out
}

// applyExpansionHandle decodes an in.Handle (#344) into a concrete path + line
// range, superseding any path/paths/lines the caller also passed. It returns
// the updated input, or a non-nil *SummarizeOutput when the handle is malformed
// (the caller should return it as-is). A blank handle is a no-op.
func applyExpansionHandle(in SummarizeInput) (SummarizeInput, *SummarizeOutput) {
	h := strings.TrimSpace(in.Handle)
	if h == "" {
		return in, nil
	}
	path, start, end, ok := DecodeHandle(h)
	if !ok {
		return in, &SummarizeOutput{Status: "bad-handle", Hint: "handle did not decode to a valid path:line range; re-run find/ask to get a fresh handle"}
	}
	in.Path = path
	in.StartLine = start
	in.EndLine = end
	in.Paths = nil
	if strings.TrimSpace(in.Mode) == "" {
		in.Mode = fmt.Sprintf("lines:%d-%d", start, end)
	}
	return in, nil
}

// summarizeBatch handles file_view when paths[] is provided.
// All files are processed with the same mode in a single call.
// Go import blocks are deduplicated across files (union emitted once as a shared
// header; per-file blocks replaced with a back-reference) unless dedup=false.
// When 3+ files are successfully read, a TF-IDF codebook is also applied to
// replace any remaining repeated lines with short §N refs.
func (s *Server) summarizeBatch(ctx context.Context, in SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error) {
	const maxBatch = 10
	if len(in.Paths) > maxBatch {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("batch too large: max %d files per call, got %d", maxBatch, len(in.Paths))}, nil
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = string(ReadModeSignatures)
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

	// Build file-content slice for dedup + codebook.
	var fileContents []string
	for _, r := range results {
		if r.ok {
			fileContents = append(fileContents, r.content)
		}
	}

	// Go import block deduplication: extract the parenthesized import block from
	// each file, emit the union once as a shared header, replace per-file blocks
	// with a single back-reference line. Applied when: mode is full or the content
	// happens to contain import blocks (dedup is a no-op when no blocks are found),
	// and the caller has not opted out with dedup=false.
	var dedupHeader string
	var importsDedupSavedLines int
	if in.Dedup == nil || *in.Dedup {
		var deduped []string
		dedupHeader, deduped, importsDedupSavedLines = compress.DeduplicateGoImports(fileContents)
		if dedupHeader != "" {
			// Splice deduped content back into results.
			j := 0
			for i := range results {
				if results[i].ok {
					results[i].content = deduped[j]
					j++
				}
			}
			fileContents = deduped
		}
	}

	cb := compress.BuildCodebook(fileContents)

	var sb strings.Builder
	if dedupHeader != "" {
		sb.WriteString(dedupHeader)
		sb.WriteByte('\n')
	}
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
		Status:                 "ok",
		Project:                project,
		Content:                strings.TrimRight(sb.String(), "\n"),
		Paths:                  resolvedPaths,
		StablePrefixTokens:     stablePrefixTokens,
		ImportsDedupSavedLines: importsDedupSavedLines,
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
	defer st.Close()
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

// Pure line-slicing and index-sourced formatting helpers (parseLinesRange,
// formatSignatures, formatMap, sliceLines, buildSummarizeSystem) moved to
// internal/summarize (#472 step 3).
