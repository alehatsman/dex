package mcp

import (
	"context"

	"github.com/alehatsman/dex/internal/proj"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type SummarizeInput struct {
	Path         string   `json:"path,omitempty" jsonschema:"file path to summarize; relative paths are resolved against project_root; required when paths is not set"`
	Paths        []string `json:"paths,omitempty" jsonschema:"batch mode: list of files (max 10); all use the same mode; path is ignored when paths is non-empty"`
	ProjectRoot  string   `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
	Mode         string   `json:"mode,omitempty" jsonschema:"read mode (default 'full'): 'full' (raw file content, no LLM), 'signatures' (indexed symbols + source lines, no LLM), 'skeleton' (exported type decls in full + function/method signatures with @B<n> body handles, no LLM), 'map' (imports + exported symbols from index, no LLM), 'lines:N-M' (raw line slice, no LLM; also lines:N single line, lines:N- to EOF, lines:-M first M), 'analyze' (token-cost comparison of every mode + a recommended mode, NO file content — pick the cheapest sufficient view before paying to read it), 'summary' (LLM-generated digest — the only mode needing a chat model; returns status='needs-chat' when none is wired)"`
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
	// Ref time-travels the read to a git revision (#657).
	Ref string `json:"ref,omitempty" jsonschema:"read the file as of a git ref (e.g. HEAD~5, v1.0, a sha) instead of the working tree; supports mode=full and mode=signatures (the historical API). The file must still exist now."`
	// Dedup controls Go import block deduplication in multi-file (paths[]) reads.
	// Default (omitted / true): the union of all import blocks is emitted once as a
	// shared header and each file's block is replaced with a back-reference comment.
	// Pass false to receive raw per-file output without any deduplication.
	Dedup *bool `json:"dedup,omitempty" jsonschema:"set to false to disable Go import deduplication in batch reads (default: true, dedup is on for full/signatures modes with ≥2 Go files)"`
	// Slice applies a surgical extraction to the file content before returning it,
	// superseding mode when present. Supported specs (#630):
	//   head:N          first N lines
	//   tail:N          last N lines
	//   range:L1-L2     lines L1–L2 (1-indexed, inclusive)
	//   search:PATTERN  RE2 grep ± 3 context lines; groups separated by ---
	//   json_path:EXPR  dot-path JSON extraction ($.a.b, $.a[0].b)
	Slice string `json:"slice,omitempty" jsonschema:"surgical extraction: head:N (first N lines), tail:N (last N lines), range:L1-L2 (line slice), search:PATTERN (RE2 grep ±3 context lines), json_path:EXPR (JSON dot-path e.g. $.a.b[0])"`
	// CCRHash retrieves a content-addressed blob written by the proxy's CCR tee
	// store (dex:lc_expand:<hash> recovery markers). When set, path/paths/handle
	// are ignored; Slice applies to the retrieved blob content (#630).
	CCRHash string `json:"ccr_hash,omitempty" jsonschema:"content-addressed recovery hash (from a dex:lc_expand:<hash> marker in pruned proxy history); returns the archived tool result, with optional slice applied"`
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
	// SeenTurn is set when look's read lane suppressed this range's content
	// because the same bytes were surfaced on an earlier turn of this session
	// (#110 step 3): the turn they were first sent. Content is cleared; reuse
	// what you already have.
	SeenTurn int `json:"seen_turn,omitempty"`
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
