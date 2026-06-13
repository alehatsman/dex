package mcp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/chunk"
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
	Mode         string   `json:"mode,omitempty" jsonschema:"read mode (default 'full'): 'full' (raw file content, no LLM), 'signatures' (indexed symbols + source lines, no LLM), 'skeleton' (exported type decls in full + function/method signatures with @B<n> body handles, no LLM), 'map' (imports + exported symbols from index, no LLM), 'lines:N-M' (raw line slice, no LLM), 'summary' (LLM-generated digest — the only mode needing a chat model; returns status='needs-chat' when none is wired)"`
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
	if h := strings.TrimSpace(in.Handle); h != "" {
		path, start, end, ok := DecodeHandle(h)
		if !ok {
			return nil, SummarizeOutput{Status: "bad-handle", Hint: "handle did not decode to a valid path:line range; re-run find/ask to get a fresh handle"}, nil
		}
		in.Path = path
		in.StartLine = start
		in.EndLine = end
		in.Paths = nil
		if strings.TrimSpace(in.Mode) == "" {
			in.Mode = fmt.Sprintf("lines:%d-%d", start, end)
		}
	}

	mode, isLLM := s.summarizeResolveMode(in)

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
		if tr.ConsumeThrottle() && mode == "summary" {
			mode = "signatures"
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

	// Bounce detection (#98): re-escalate on repeated compressed reads — give
	// the agent the complete view (LLM summary when a chat model is wired,
	// else the raw full file).
	bt := s.bt()
	bt.recordRead(sessionID, relTarget)
	if bt.shouldForceFull(sessionID, relTarget) && mode != "summary" && mode != "full" {
		if s.ChatClient != nil {
			mode = "summary"
			isLLM = true
		} else {
			mode = "full"
			isLLM = false
		}
	}

	// Budget-aware downgrade (#106): auto-select richest mode within budget.
	// Skips the LLM summary (its output is already small); raw full is the
	// largest payload, so it is downgraded toward signatures/map as needed.
	if in.BudgetTokens > 0 && !isLLM {
		mode = selectAffordableMode(mode, len(data)/4, in.BudgetTokens)
	}

	// Dependency manifest shortcut (#125): compact summary for package.json etc.
	// Honor an explicit raw-full or LLM-summary request; compact only the
	// structural modes.
	if compress.IsDepsFilename(filepath.Base(realTarget)) && mode != "full" && mode != "summary" {
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
	case mode == "summary":
		result, out, err = s.summarizeModeSummary(w)
	case strings.HasPrefix(mode, "lines:"):
		result, out, err = s.summarizeModeLines(w, mode)
	case mode == "signatures":
		result, out, err = s.summarizeModeSignatures(w)
	case mode == "map":
		result, out, err = s.summarizeModeMap(w)
	case mode == "aggressive":
		result, out, err = s.summarizeModeAggressive(w)
	case mode == "skeleton":
		result, out, err = s.summarizeModeSkeleton(w)
	default: // "full" — raw file content, no LLM, no compression.
		result, out, err = s.summarizeModeRaw(w)
	}
	return
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
