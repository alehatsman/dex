package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/review"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── verb: review ─────────────────────────────────────────────────────────
//
// review (#639 / GitHub #65 Tier S2) is per-hunk PR intelligence. Code review is
// delta-shaped, but every other dex verb is state-shaped — ask/find/trace/read
// give a current snapshot. To review a diff an agent otherwise stitches together
// five tools per file: diff, then callers per touched symbol, then sibling tests,
// then git history. review collapses that into one call.
//
// It is pure composition over shipped infrastructure: the review.ParseUnified
// hunk parser (the only new piece), ChunkAt for hunk→symbol mapping, traceVerb
// for callers + risk, the Enricher legs for tests / nearest doc / blame, and small
// git helpers for churn + author history. No new store schema, no new backend,
// no model call.
//
// Scope (v1): hunk→symbol mapping resolves a changed line to its *enclosing
// declaration* via ChunkAt (decl byte-spans from #591). It is NOT call-site
// precise — the graph stores call edges with start_line only, no reference
// byte-spans — so a hunk is attributed to the function it lives in, which is
// what a reviewer wants anyway.

const (
	reviewMaxFiles       = 100 // cap files scanned (huge diffs)
	reviewMaxHunks       = 200 // cap total hunks emitted; truncate past this
	reviewMaxHunksNoCode = 3   // per-file cap when the file has no indexed symbols
	reviewMaxSymHunk     = 6   // cap symbols resolved per hunk
	reviewMaxProbes      = 32  // cap ChunkAt lookups per hunk (strided over big hunks)
	reviewCallerMed      = 10  // >= this many callers → at least medium risk
	reviewCallerHigh     = 30  // >= this many callers → high risk
	reviewGitTimeout     = 5 * time.Second
	reviewChurnWindow    = "30 days ago"
)

// ReviewInput selects the diff four ways. Precedence is Ref, then Branch, then
// PR, then Worktree; with none given, review defaults to Worktree — "review my
// uncommitted changes" is the common unanchored case (#137).
type ReviewInput struct {
	Ref         string `json:"ref,omitempty" jsonschema:"a git range ('HEAD~3..HEAD') or a single ref ('HEAD~3', diffed against HEAD)"`
	Branch      string `json:"branch,omitempty" jsonschema:"a branch name; reviews what it adds since diverging from the default branch (main...branch)"`
	PR          int    `json:"pr,omitempty" jsonschema:"a GitHub PR number; resolves its head branch via the 'gh' CLI (best-effort, needs gh + a remote)"`
	Worktree    bool   `json:"worktree,omitempty" jsonschema:"review uncommitted working-tree changes (git diff HEAD — staged + unstaged); the default when no ref/branch/pr is given"`
	Base        string `json:"base,omitempty" jsonschema:"base branch for branch/PR comparison (default 'main')"`
	Compact     bool   `json:"compact,omitempty" jsonschema:"drop low-risk hunks, returning only medium/high-risk ones"`
	K           int    `json:"k,omitempty" jsonschema:"max callers and notes per symbol (default 8, max 30)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
}

// ReviewSymbol is a declaration a hunk touches, resolved to its enclosing chunk.
// CallerCount is how many callers the symbol has (its caller bodies live once in
// ReviewOutput.CallersBySymbol, keyed by Name — #136).
type ReviewSymbol struct {
	Name        string `json:"name"`
	Kind        string `json:"kind,omitempty"`
	Exported    bool   `json:"exported"`
	StartLine   int    `json:"start_line,omitempty"`
	EndLine     int    `json:"end_line,omitempty"`
	CallerCount int    `json:"caller_count,omitempty"`
}

// ReviewHunk is one @@ block plus its composed intelligence. Symbol-level lanes
// (symbols, notes, risk) live here; file-level lanes (tests, doc, churn, author)
// live on ReviewFile to avoid repeating them per hunk. Caller bodies are NOT
// here — a symbol touched by many hunks would duplicate them; they are hoisted
// to ReviewOutput.CallersBySymbol and joined via SymbolsTouched[i].Name (#136).
type ReviewHunk struct {
	OldStart       int            `json:"old_start"`
	OldLines       int            `json:"old_lines"`
	NewStart       int            `json:"new_start"`
	NewLines       int            `json:"new_lines"`
	Heading        string         `json:"heading,omitempty"`
	SymbolsTouched []ReviewSymbol `json:"symbols_touched,omitempty"`
	RiskTier       string         `json:"risk_tier"`             // "low" | "medium" | "high"
	RiskReason     string         `json:"risk_reason,omitempty"` // dominant signal
}

// ReviewFile groups one file's hunks with its file-level history signals.
type ReviewFile struct {
	Path          string   `json:"file"`
	OldPath       string   `json:"old_path,omitempty"`
	Status        string   `json:"status"` // added | modified | deleted | renamed
	Tests         []string `json:"tests_covering,omitempty"`
	NearestDoc    string   `json:"nearest_doc,omitempty"`
	Churn30d      int      `json:"churn_30d,omitempty"`
	LastCommit    string   `json:"last_commit,omitempty"`
	LastAuthor    string   `json:"last_author,omitempty"`
	AuthorHistory []string `json:"author_history,omitempty"`
	// GateFindings are machine-readable quality-gate findings (#155) whose path
	// is this file, read from .gate/findings.jsonl (the goq/findings artifact).
	// Best-effort: empty when the artifact is absent or the file has none. Lets
	// "review my changes" show the gate's verdict on the touched files inline —
	// the ingest half of the gate-speaks-agent flywheel.
	GateFindings []GateFinding `json:"gate_findings,omitempty"`
	Hunks        []ReviewHunk  `json:"hunks"`
}

// GateFinding is one machine-readable quality-gate finding (#155) — the shared
// schema emitted by goq/findings' `--format jsonl` steps and by dex's own
// `smells`/`clones --format jsonl` (P3 emit). Two projections of one shape:
// attached under a ReviewFile (ingest) Path/Fingerprint are omitted — path is
// the enclosing file; emitted standalone they are set. Field order matches the
// go-quality emitters so the JSONL keys line up across producers.
type GateFinding struct {
	Tool        string `json:"tool"`
	Rule        string `json:"rule"`
	Level       string `json:"level"` // error | warning | note
	Path        string `json:"path,omitempty"`
	Line        int    `json:"line,omitempty"`
	Col         int    `json:"col,omitempty"`
	Message     string `json:"message,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// ReviewOutput is the per-hunk bundle. Every lane is best-effort: an empty list
// means that lane found nothing (or degraded — callers is empty with no graph),
// never an error.
type ReviewOutput struct {
	Status  string       `json:"status"` // ok | no-index | no-changes | not-found | error
	Hint    string       `json:"hint,omitempty"`
	Project string       `json:"project,omitempty"`
	Range   string       `json:"range,omitempty"` // resolved git range actually diffed
	Files   []ReviewFile `json:"files,omitempty"`
	// CallersBySymbol holds each touched symbol's callers ONCE, keyed by symbol
	// name (#136). Hunks reference it via SymbolsTouched[i].Name instead of
	// embedding the caller bodies per hunk — the same symbol is often touched by
	// many hunks, and the caller source was the largest duplicated payload.
	CallersBySymbol map[string][]CallSite `json:"callers_by_symbol,omitempty"`
	TotalHunks      int                   `json:"total_hunks"`
	Truncated       bool                  `json:"truncated,omitempty"`
}

// Review runs the review verb for callers without an SDK request — the REST
// `/review` route. Composes over the local *Server exactly like the stdio tool.
func (s *Server) Review(ctx context.Context, in ReviewInput) (ReviewOutput, error) {
	_, out, err := s.review(ctx, nil, in)
	return out, err
}

// reviewResponse routes an intent=review ask ("review my changes") to the
// per-hunk review composition and wraps its delta-shaped result in the
// discriminated-union ContextOutput.Review field, so the four-verb front door
// reaches the everyday review loop without cramming a delta into the
// state-shaped lanes. Mirrors orientResponse's short-circuit contract (returns
// a nil CallToolResult; the ask wrapper serializes the ContextOutput). The auto
// path reviews the working tree via Review's #137 no-selector default; targeted
// PR/branch/ref review stays on the review_diff tool / `dex review` CLI.
func (s *Server) reviewResponse(ctx context.Context, in ContextInput) (*sdk.CallToolResult, ContextOutput, error) {
	ro, err := s.Review(ctx, ReviewInput{ProjectRoot: in.ProjectRoot, K: in.K})
	if err != nil {
		return nil, ContextOutput{Status: "error", Intent: retrieve.IntentReview, Hint: err.Error()}, nil
	}
	out := ContextOutput{
		Status:  ro.Status,
		Project: ro.Project,
		Intent:  retrieve.IntentReview,
		Hint:    ro.Hint,
		Review:  &ro,
	}
	return nil, out, nil
}

func (s *Server) review(ctx context.Context, _ *sdk.CallToolRequest, in ReviewInput) (*sdk.CallToolResult, ReviewOutput, error) {
	// No selector → review the uncommitted working tree ("review my changes"),
	// the common unanchored case (#137). Explicit ref/branch/pr still win.
	if strings.TrimSpace(in.Ref) == "" && strings.TrimSpace(in.Branch) == "" && in.PR == 0 {
		in.Worktree = true
	}
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, ReviewOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, ReviewOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}

	k := in.K
	if k <= 0 {
		k = 30 // default matches cap so reviewCallerMed=10 and reviewCallerHigh=30 thresholds are reachable
	}
	if k > 30 {
		k = 30
	}

	rng, rstatus, rhint := resolveReviewRange(ctx, p.Root, in)
	if rstatus != "ok" {
		return nil, ReviewOutput{Status: rstatus, Project: p.Root, Hint: rhint}, nil
	}

	diffText, err := gitDiffUnified(ctx, p.Root, rng)
	if err != nil {
		return nil, ReviewOutput{Status: "error", Project: p.Root, Range: rng,
			Hint: fmt.Sprintf("could not diff %q — check it is a valid git ref/range (try `git rev-parse %s`)", rng, rng)}, nil
	}
	files := review.ParseUnified(diffText)
	if len(files) == 0 {
		hint := fmt.Sprintf("no changes in %s", rng)
		if in.Worktree {
			hint = "working tree is clean (no uncommitted changes) — pass ref/branch/pr to review committed work"
			if n := gitUntrackedCount(ctx, p.Root); n > 0 {
				hint += fmt.Sprintf("; %d untracked file(s) are not shown (`git add -N` to include them)", n)
			}
		}
		return nil, ReviewOutput{Status: "no-changes", Project: p.Root, Range: rng, Hint: hint}, nil
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, ReviewOutput{Status: "error", Project: p.Root, Range: rng,
			Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	out := ReviewOutput{Status: "ok", Project: p.Root, Range: rng}
	// When the range doesn't end at HEAD, resolve hunk→symbol against the
	// diff's own new-side ref via git show (time-travel, #644). Callers/risk
	// still come from the live graph — note that in the hint.
	newRef := ""
	if !rangeEndsAtHEAD(rng) {
		newRef = extractNewRef(rng)
		if newRef == "" {
			// Couldn't extract a concrete ref (shouldn't happen after resolveReviewRange).
			out.Hint = appendHint(out.Hint, "range does not end at HEAD; symbols map against the current index and may be incomplete")
		} else {
			out.Hint = appendHint(out.Hint, "callers and risk tiers reflect the current index, not the diff's historical revision")
		}
	}
	e := &retrieve.Enricher{ProjectRoot: p.Root}
	// Cache caller/risk per symbol — the same function often recurs across hunks
	// and files, and traceVerb is the costly leg.
	callerCache := map[string]traceResult{}

	hunkBudget := reviewMaxHunks
	if len(files) > reviewMaxFiles {
		files = files[:reviewMaxFiles]
		out.Truncated = true
	}

	for _, fd := range files {
		rf := s.reviewFile(ctx, st, e, p.Root, fd, k, &hunkBudget, callerCache, newRef)
		if in.Compact {
			rf.Hunks = dropLowRiskHunks(rf.Hunks)
			if len(rf.Hunks) == 0 {
				continue // file had only low-risk hunks
			}
		}
		out.TotalHunks += len(rf.Hunks)
		out.Files = append(out.Files, rf)
		if hunkBudget <= 0 {
			out.Truncated = true
			break
		}
	}
	// Hoist caller bodies to a single top-level map keyed by symbol (#136). Only
	// symbols that survive in emitted hunks (post-compact, post-truncation) are
	// included; each hunk joins via SymbolsTouched[i].Name + CallerCount.
	out.CallersBySymbol = collectCallersBySymbol(out.Files, callerCache)
	// Fold in machine-readable gate findings (#155): attach each finding whose
	// path is a reviewed file, so "review my changes" shows the quality gate's
	// verdict on the touched files. Best-effort snapshot of .gate/findings.jsonl.
	if gate := loadGateFindings(p.Root); len(gate) > 0 {
		nGate := 0
		for i := range out.Files {
			if fs := gate[cleanRelPath(out.Files[i].Path)]; len(fs) > 0 {
				out.Files[i].GateFindings = fs
				nGate += len(fs)
			}
		}
		if nGate > 0 {
			out.Hint = appendHint(out.Hint, fmt.Sprintf("%d gate finding(s) on touched files from .gate/findings.jsonl (run `mooncake task findings` to refresh)", nGate))
		}
	}
	if out.Truncated {
		out.Hint = appendHint(out.Hint, fmt.Sprintf("output capped at %d hunks / %d files — narrow the range for full coverage", reviewMaxHunks, reviewMaxFiles))
	}
	if len(out.Files) > 0 {
		// Close the review→edit loop (#87): findings you confirm here should be
		// persisted where the next editor will hit them, not left in chat.
		out.Hint = appendHint(out.Hint, "persist a finding the next editor needs via notes(action=add, archetype=ReviewFinding, scope=<file>, body=\"[kind] …\") — it then surfaces in read/locate/review scoped_notes on touch")
	}
	return nil, out, nil
}

// loadGateFindings reads .gate/findings.jsonl under root (the goq/findings
// artifact) and groups findings by cleaned path. Best-effort: a missing file or
// an unparseable line yields no findings for that path, never an error — the
// gate view is optional context, not a review dependency (#155).
func loadGateFindings(root string) map[string][]GateFinding {
	f, err := os.Open(filepath.Join(root, ".gate", "findings.jsonl"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	out := map[string][]GateFinding{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			GateFinding
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Path == "" {
			continue // tolerate a malformed line rather than sink the whole read
		}
		key := cleanRelPath(rec.Path)
		out[key] = append(out[key], rec.GateFinding)
	}
	return out
}

// cleanRelPath normalizes a finding/diff path for matching: forward slashes, no
// leading "./". Emitters vary (god-file paths carry "./"; gocyclo/dupl/ai-lint
// don't; git diff paths are bare repo-relative), so both sides are cleaned.
func cleanRelPath(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(p), "./")
}
