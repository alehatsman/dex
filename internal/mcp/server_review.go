package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/gitenv"
	"github.com/alehatsman/dex/internal/review"
	"github.com/alehatsman/dex/internal/store"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── verb: review ─────────────────────────────────────────────────────────
//
// review (#639 / GitHub #65 Tier S2) is per-hunk PR intelligence. Code review is
// delta-shaped, but every other dex verb is state-shaped — ask/find/trace/read
// give a current snapshot. To review a diff an agent otherwise stitches together
// six tools per file: diff, then callers per touched symbol, then sibling tests,
// then git history, then linked notes. review collapses that into one call.
//
// It is pure composition over shipped infrastructure: the review.ParseUnified
// hunk parser (the only new piece), ChunkAt for hunk→symbol mapping, traceVerb
// for callers + risk, the Enricher legs for tests / nearest doc / blame, small
// git helpers for churn + author history, and recallFacts for notes. No new
// store schema, no new backend, no model call.
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

// ReviewInput selects the diff three ways. Exactly one of Ref / Branch / PR is
// expected; precedence is Ref, then Branch, then PR.
type ReviewInput struct {
	Ref         string `json:"ref,omitempty" jsonschema:"a git range ('HEAD~3..HEAD') or a single ref ('HEAD~3', diffed against HEAD)"`
	Branch      string `json:"branch,omitempty" jsonschema:"a branch name; reviews what it adds since diverging from the default branch (main...branch)"`
	PR          int    `json:"pr,omitempty" jsonschema:"a GitHub PR number; resolves its head branch via the 'gh' CLI (best-effort, needs gh + a remote)"`
	Base        string `json:"base,omitempty" jsonschema:"base branch for branch/PR comparison (default 'main')"`
	Compact     bool   `json:"compact,omitempty" jsonschema:"drop low-risk hunks, returning only medium/high-risk ones"`
	K           int    `json:"k,omitempty" jsonschema:"max callers and notes per symbol (default 8, max 30)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// ReviewSymbol is a declaration a hunk touches, resolved to its enclosing chunk.
type ReviewSymbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	Exported  bool   `json:"exported"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

// ReviewHunk is one @@ block plus its composed intelligence. Symbol-level lanes
// (symbols, callers, notes, risk) live here; file-level lanes (tests, doc,
// churn, author) live on ReviewFile to avoid repeating them per hunk.
type ReviewHunk struct {
	OldStart       int            `json:"old_start"`
	OldLines       int            `json:"old_lines"`
	NewStart       int            `json:"new_start"`
	NewLines       int            `json:"new_lines"`
	Heading        string         `json:"heading,omitempty"`
	SymbolsTouched []ReviewSymbol `json:"symbols_touched,omitempty"`
	Callers        []CallSite     `json:"callers_of_touched,omitempty"`
	Notes          []LocatedFact  `json:"notes,omitempty"`
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
	// ScopedNotes are notes BOUND to this file's path via their scope
	// (gotcha-on-touch, #645/#649) — surfaced because the PR touches the file,
	// each tagged with the matching glob/path. Distinct from per-hunk Notes,
	// which are recalled by the touched symbol.
	ScopedNotes []LocatedFact `json:"scoped_notes,omitempty"`
	Hunks       []ReviewHunk  `json:"hunks"`
}

// ReviewOutput is the per-hunk bundle. Every lane is best-effort: an empty list
// means that lane found nothing (or degraded — callers is empty with no graph),
// never an error.
type ReviewOutput struct {
	Status     string       `json:"status"` // ok | no-index | no-changes | not-found | error
	Hint       string       `json:"hint,omitempty"`
	Project    string       `json:"project,omitempty"`
	Range      string       `json:"range,omitempty"` // resolved git range actually diffed
	Files      []ReviewFile `json:"files,omitempty"`
	TotalHunks int          `json:"total_hunks"`
	Truncated  bool         `json:"truncated,omitempty"`
}

// Review runs the review verb for callers without an SDK request — the REST
// `/review` route. Composes over the local *Server exactly like the stdio tool.
func (s *Server) Review(ctx context.Context, in ReviewInput) (ReviewOutput, error) {
	_, out, err := s.review(ctx, nil, in)
	return out, err
}

func (s *Server) review(ctx context.Context, _ *sdk.CallToolRequest, in ReviewInput) (*sdk.CallToolResult, ReviewOutput, error) {
	if strings.TrimSpace(in.Ref) == "" && strings.TrimSpace(in.Branch) == "" && in.PR == 0 {
		return nil, ReviewOutput{Status: "error", Hint: "review needs one of: ref, branch, pr"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
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
		return nil, ReviewOutput{Status: "no-changes", Project: p.Root, Range: rng,
			Hint: fmt.Sprintf("no changes in %s", rng)}, nil
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, ReviewOutput{Status: "error", Project: p.Root, Range: rng,
			Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	out := ReviewOutput{Status: "ok", Project: p.Root, Range: rng}
	// Hunk→symbol mapping resolves new-side line numbers against the CURRENT
	// index (which reflects HEAD). When the range doesn't end at HEAD those line
	// numbers don't line up, so symbols/callers/risk degrade silently — say so.
	if !rangeEndsAtHEAD(rng) {
		out.Hint = appendHint(out.Hint, "range does not end at HEAD; symbols/callers map against the current index and may be incomplete for older revisions")
	}
	e := &Enricher{projectRoot: p.Root}
	// Cache caller/risk + notes per symbol — the same function often recurs
	// across hunks and files, and traceVerb/recallFacts are the costly legs.
	callerCache := map[string]traceResult{}
	noteCache := map[string][]LocatedFact{}

	hunkBudget := reviewMaxHunks
	if len(files) > reviewMaxFiles {
		files = files[:reviewMaxFiles]
		out.Truncated = true
	}

	for _, fd := range files {
		rf := s.reviewFile(ctx, st, e, p.Root, fd, k, &hunkBudget, callerCache, noteCache)
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
	if out.Truncated {
		out.Hint = appendHint(out.Hint, fmt.Sprintf("output capped at %d hunks / %d files — narrow the range for full coverage", reviewMaxHunks, reviewMaxFiles))
	}
	return nil, out, nil
}

// traceResult is the cached caller lane for one symbol.
type traceResult struct {
	callers []CallSite
	count   int
	noGraph bool
}

// reviewFile composes one file's hunks. File-level history legs (tests, doc,
// blame, churn, author) run once; symbol legs run per hunk against the caches.
func (s *Server) reviewFile(ctx context.Context, st *store.Store, e *Enricher, root string,
	fd review.FileDiff, k int, hunkBudget *int,
	callerCache map[string]traceResult, noteCache map[string][]LocatedFact) ReviewFile {

	rf := ReviewFile{Path: fd.Path, OldPath: fd.OldPath, Status: fd.Status}

	// File-level history — pure filesystem / git, best-effort.
	rf.Tests = e.pairSiblingTests(fd.Path)
	rf.NearestDoc = e.findNearestDoc(fd.Path)
	meta := map[string]*PathMeta{}
	e.enrichBlame(ctx, []string{fd.Path}, meta)
	if m := meta[fd.Path]; m != nil {
		rf.LastCommit = m.LastCommit
		rf.LastAuthor = m.LastAuthor
	}
	rf.Churn30d = gitChurnCount(ctx, root, fd.Path)
	rf.AuthorHistory = gitAuthorHistory(ctx, root, fd.Path)

	// Proactive gotcha-on-touch (#645/#649): notes whose scope binds this file's
	// path — surfaced because the PR touches it, even if no hunk symbol recalls
	// them. Best-effort.
	if scoped, err := st.KnowledgeByScope(ctx, fd.Path, k); err == nil {
		for _, f := range scoped {
			rf.ScopedNotes = append(rf.ScopedNotes, LocatedFact{
				ID: f.ID, Archetype: f.Archetype, Body: f.Body, Salience: f.Salience, Scope: f.Scope,
			})
		}
	}

	// A deleted file has no current symbols to resolve; emit its hunks with
	// diff + history only (still useful, per the cold-start contract).
	resolvable := fd.Status != "deleted"

	// Track whether this file has any indexed symbols. After reviewMaxHunksNoCode
	// hunks with zero symbols we treat it as a data/non-code file and apply a
	// tighter per-file cap so a single large JSON or generated file can't consume
	// the entire hunkBudget and starve code files in the same diff.
	var fileHunksEmitted, fileSymbolsSeen int

	for _, h := range fd.Hunks {
		if *hunkBudget <= 0 {
			break
		}
		// Non-code file guard: after the probe window, if still no symbols, cap early.
		if fileSymbolsSeen == 0 && fileHunksEmitted >= reviewMaxHunksNoCode {
			break
		}
		*hunkBudget--
		fileHunksEmitted++

		rh := ReviewHunk{
			OldStart: h.OldStart, OldLines: h.OldLines,
			NewStart: h.NewStart, NewLines: h.NewLines, Heading: h.Heading,
		}
		var maxCallers int
		var exported, hadGraph bool
		if resolvable {
			syms := resolveHunkSymbols(ctx, st, fd.Path, h)
			rh.SymbolsTouched = syms
			fileSymbolsSeen += len(syms)
			// No symbols at this hunk (comment/whitespace/data). Don't emit
			// "graph not indexed" — there's simply nothing to look up.
			if len(syms) == 0 {
				hadGraph = true
			}
			seenCaller := map[string]bool{}
			seenNote := map[int64]bool{}
			for _, sym := range syms {
				if sym.Exported {
					exported = true
				}
				tr := cachedCallers(ctx, s, root, sym.Name, k, callerCache)
				if !tr.noGraph {
					hadGraph = true
				}
				if tr.count > maxCallers {
					maxCallers = tr.count
				}
				for _, c := range tr.callers {
					key := c.Path + ":" + strconv.Itoa(c.StartLine)
					if !seenCaller[key] {
						seenCaller[key] = true
						rh.Callers = append(rh.Callers, c)
					}
				}
				// Dedup notes by ID across symbols: two symbols in the same hunk
				// often share the same note (e.g. a gotcha scoped to the file),
				// which would produce duplicates per hunk with many probes (#701).
				for _, n := range cachedNotes(ctx, s, st, sym.Name, k, noteCache) {
					if !seenNote[n.ID] {
						seenNote[n.ID] = true
						rh.Notes = append(rh.Notes, n)
					}
				}
			}
		}
		rh.RiskTier, rh.RiskReason = hunkRisk(maxCallers, exported, hadGraph)
		rf.Hunks = append(rf.Hunks, rh)
	}
	return rf
}

// resolveHunkSymbols maps a hunk's new-side line range to the enclosing
// declarations via ChunkAt, deduped by name+line and capped.
func resolveHunkSymbols(ctx context.Context, st *store.Store, path string, h review.Hunk) []ReviewSymbol {
	seen := map[string]bool{}
	var out []ReviewSymbol
	// Probe ChunkAt at most reviewMaxProbes times, strided evenly across the
	// hunk so a large added file (hundreds of new lines) costs a bounded number
	// of lookups instead of one per line.
	lines := h.TouchedLines()
	stride := 1
	if len(lines) > reviewMaxProbes {
		stride = (len(lines) + reviewMaxProbes - 1) / reviewMaxProbes
	}
	for i := 0; i < len(lines); i += stride {
		if len(out) >= reviewMaxSymHunk {
			break
		}
		line := lines[i]
		hit, err := st.ChunkAt(ctx, path, line)
		if err != nil || hit.Name == "" {
			continue
		}
		// Dedup by name only: a long function spans multiple indexed chunks
		// with different start_line values, but they're the same symbol.
		// Method names are qualified ((*Foo).Method), so different-receiver
		// methods with the same bare name won't collide (#700).
		key := hit.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ReviewSymbol{
			Name: hit.Name, Kind: hit.Kind, Exported: isExportedName(bareSymbolName(hit.Name)),
			StartLine: hit.StartLine, EndLine: hit.EndLine,
		})
	}
	return out
}

// cachedCallers returns the caller lane for a symbol, memoised across the review.
func cachedCallers(ctx context.Context, s *Server, root, symbol string, k int, cache map[string]traceResult) traceResult {
	if tr, ok := cache[symbol]; ok {
		return tr
	}
	_, tr, _ := traceVerb(ctx, s, nil, TraceInput{
		Symbol: symbol, Direction: "callers", K: k, ProjectRoot: root,
	})
	res := traceResult{}
	switch tr.Status {
	case "ok":
		res.callers = tr.Hits
		res.count = len(tr.Hits)
	case "no-graph":
		res.noGraph = true
	}
	cache[symbol] = res
	return res
}

// cachedNotes returns related notes for a symbol, memoised across the review.
func cachedNotes(ctx context.Context, s *Server, st *store.Store, symbol string, k int, cache map[string][]LocatedFact) []LocatedFact {
	if n, ok := cache[symbol]; ok {
		return n
	}
	var notes []LocatedFact
	if facts, err := s.recallFacts(ctx, st, symbol, k, false, true); err == nil {
		for _, f := range facts {
			notes = append(notes, LocatedFact{ID: f.ID, Archetype: f.Archetype, Body: f.Body, Salience: f.Salience})
		}
	}
	cache[symbol] = notes
	return notes
}

// hunkRisk tiers a hunk from its symbols' caller blast radius and export status.
// Thresholds (from the S2 proposal): >=10 callers → medium, >=30 → high, an
// exported symbol bumps one tier. When the graph isn't indexed callers are
// unknown, so risk falls back to export status only and the reason says so.
func hunkRisk(maxCallers int, exported, hadGraph bool) (tier, reason string) {
	tier = "low"
	switch {
	case maxCallers >= reviewCallerHigh:
		tier = "high"
	case maxCallers >= reviewCallerMed:
		tier = "medium"
	}
	if exported {
		tier = bumpTier(tier)
	}

	switch {
	case !hadGraph && exported:
		reason = "exported symbol; callers unknown (graph not indexed)"
	case !hadGraph:
		reason = "callers unknown (graph not indexed)"
	case exported && maxCallers > 0:
		reason = fmt.Sprintf("exported symbol with %d callers", maxCallers)
	case maxCallers > 0:
		reason = fmt.Sprintf("%d callers", maxCallers)
	case exported:
		reason = "exported symbol, no indexed callers"
	default:
		reason = "no exported symbols or callers touched"
	}
	return tier, reason
}

func bumpTier(t string) string {
	switch t {
	case "low":
		return "medium"
	case "medium":
		return "high"
	default:
		return "high"
	}
}

// dropLowRiskHunks keeps only medium/high-risk hunks (the `compact` flag).
func dropLowRiskHunks(hunks []ReviewHunk) []ReviewHunk {
	var out []ReviewHunk
	for _, h := range hunks {
		if h.RiskTier != "low" {
			out = append(out, h)
		}
	}
	return out
}

// ─── range resolution + git helpers ──────────────────────────────────────

// resolveReviewRange turns the input selector into a git range token for
// `git diff`. Ref wins, then Branch, then PR (which resolves to a branch).
func resolveReviewRange(ctx context.Context, root string, in ReviewInput) (rng, status, hint string) {
	base := strings.TrimSpace(in.Base)
	if base == "" {
		base = "main"
	}
	if !reValidRef.MatchString(base) {
		return "", "error", fmt.Sprintf("invalid base ref %q", base)
	}

	if ref := strings.TrimSpace(in.Ref); ref != "" {
		if !reValidRef.MatchString(ref) {
			return "", "error", fmt.Sprintf("invalid ref %q — only alphanumeric, ~^:./_@{}- characters allowed", ref)
		}
		if !strings.Contains(ref, "..") {
			ref += "..HEAD" // single ref → ref..HEAD
		}
		return ref, "ok", ""
	}

	if br := strings.TrimSpace(in.Branch); br != "" {
		if !reValidRef.MatchString(br) {
			return "", "error", fmt.Sprintf("invalid branch %q", br)
		}
		return base + "..." + br, "ok", "" // symmetric: what the branch adds since divergence
	}

	// PR: resolve the head branch via gh, then review it like a branch.
	head := ghPRHeadBranch(ctx, root, in.PR)
	if head == "" {
		return "", "not-found", fmt.Sprintf("could not resolve PR #%d head branch — needs the `gh` CLI, a GitHub remote, and a fetched head", in.PR)
	}
	if !reValidRef.MatchString(head) {
		return "", "error", fmt.Sprintf("PR #%d head branch %q has unexpected characters", in.PR, head)
	}
	return base + "..." + head, "ok", ""
}

// rangeEndsAtHEAD reports whether a git range's right-hand side is HEAD (the
// state the index reflects). resolveReviewRange always emits a range containing
// ".." / "...", so the side after the last ".." is the head; an empty head
// (e.g. "ref..") defaults to HEAD in git.
func rangeEndsAtHEAD(rng string) bool {
	head := rng
	if i := strings.LastIndex(rng, ".."); i >= 0 {
		head = rng[i+2:]
	}
	head = strings.TrimSpace(head)
	return head == "" || head == "HEAD" || head == "@"
}

// gitDiffUnified runs `git diff --unified=0 <range>` in root and returns the raw
// unified diff. Zero context keeps hunks tight around the actual change.
// Output is capped at maxDiffBytes to prevent OOM on auto-generated or vendored
// diffs; the display caps in the review verb fire on the parsed hunks afterwards.
func gitDiffUnified(ctx context.Context, root, rng string) (string, error) {
	const maxDiffBytes = 4 * 1024 * 1024 // 4 MB hard cap
	cctx, cancel := context.WithTimeout(ctx, reviewGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", root,
		"diff", "--unified=0", "--no-color", "--end-of-options", rng) // #nosec G204 — rng validated by reValidRef
	cmd.Env = gitenv.Current()
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(pipe, maxDiffBytes))
	// Kill ensures the process exits promptly if we stopped reading early;
	// it is a no-op when the process already exited naturally.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if readErr != nil {
		return "", readErr
	}
	return string(data), nil
}

// gitChurnCount returns the number of commits touching path in the churn window
// (best-effort; 0 on any error or missing git).
func gitChurnCount(ctx context.Context, root, path string) int {
	if path == "" {
		return 0
	}
	cctx, cancel := context.WithTimeout(ctx, reviewGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", root,
		"rev-list", "--count", "--since="+reviewChurnWindow, "HEAD", "--", path) // #nosec G204
	cmd.Env = gitenv.Current()
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return n
}

// gitAuthorHistory returns the authors of the last 3 commits touching path,
// most-recent first (best-effort).
func gitAuthorHistory(ctx context.Context, root, path string) []string {
	if path == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, reviewGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", root,
		"log", "-3", "--format=%an", "--", path) // #nosec G204
	cmd.Env = gitenv.Current()
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var authors []string
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			authors = append(authors, ln)
		}
	}
	return authors
}

// ghPRHeadBranch resolves a PR number to its head branch via the `gh` CLI.
// Best-effort: a missing binary, no repo, or any error yields "". Capped at the
// git timeout so a slow network never stalls review.
func ghPRHeadBranch(ctx context.Context, root string, pr int) string {
	if pr <= 0 {
		return ""
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, reviewGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "gh", "pr", "view", strconv.Itoa(pr),
		"--json", "headRefName", "--jq", ".headRefName")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
