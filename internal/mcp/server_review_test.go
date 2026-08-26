package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/retrieve"
)

func TestHunkRisk(t *testing.T) {
	cases := []struct {
		maxCallers int
		exported   bool
		hadGraph   bool
		wantTier   string
	}{
		{0, false, true, "low"},
		{5, false, true, "low"},
		{10, false, true, "medium"},
		{29, false, true, "medium"},
		{30, false, true, "high"},
		{0, true, true, "medium"},  // exported bumps low→medium
		{10, true, true, "high"},   // exported bumps medium→high
		{30, true, true, "high"},   // already high, stays high
		{0, false, false, "low"},   // no graph, not exported
		{0, true, false, "medium"}, // no graph, exported → bump
	}
	for _, c := range cases {
		tier, reason := hunkRisk(c.maxCallers, c.exported, c.hadGraph)
		if tier != c.wantTier {
			t.Errorf("hunkRisk(%d,%v,%v) tier = %q, want %q", c.maxCallers, c.exported, c.hadGraph, tier, c.wantTier)
		}
		if reason == "" {
			t.Errorf("hunkRisk(%d,%v,%v) gave empty reason", c.maxCallers, c.exported, c.hadGraph)
		}
	}
}

// TestCollectCallersBySymbol locks the #136 dedup invariant: a symbol touched by
// many hunks contributes its caller list to the top-level map exactly ONCE, and
// only symbols present in emitted hunks (post-compact/truncation) — with a
// non-empty cached caller lane — are included.
func TestCollectCallersBySymbol(t *testing.T) {
	// Greet is touched by two hunks (in two files); Helper by one; Orphan is in
	// the cache but no hunk touches it; Bare is touched but has no callers.
	files := []ReviewFile{
		{Hunks: []ReviewHunk{
			{SymbolsTouched: []ReviewSymbol{{Name: "Greet"}, {Name: "Bare"}}},
			{SymbolsTouched: []ReviewSymbol{{Name: "Greet"}}},
		}},
		{Hunks: []ReviewHunk{
			{SymbolsTouched: []ReviewSymbol{{Name: "Helper"}}},
		}},
	}
	cache := map[string]traceResult{
		"Greet":  {callers: []CallSite{{Path: "a.go", StartLine: 1}, {Path: "b.go", StartLine: 2}}, count: 2},
		"Helper": {callers: []CallSite{{Path: "c.go", StartLine: 3}}, count: 1},
		"Bare":   {callers: nil, count: 0},              // touched, no callers → omitted
		"Orphan": {callers: []CallSite{{Path: "x.go"}}}, // has callers but untouched → omitted
	}

	got := collectCallersBySymbol(files, cache)

	if len(got) != 2 {
		t.Fatalf("map size = %d, want 2 (Greet, Helper); got keys %v", len(got), keysOf(got))
	}
	if g := got["Greet"]; len(g) != 2 {
		t.Errorf("Greet callers = %d, want 2 (deduped to one entry despite two hunks)", len(g))
	}
	if _, ok := got["Bare"]; ok {
		t.Errorf("Bare has no callers; must be omitted, not keyed empty")
	}
	if _, ok := got["Orphan"]; ok {
		t.Errorf("Orphan is untouched by any hunk; must not leak into the map")
	}

	// Empty result → nil so the JSON field is omitted, not `{}`.
	if collectCallersBySymbol(nil, cache) != nil {
		t.Errorf("no emitted hunks → want nil map (omitempty), got non-nil")
	}
}

func keysOf(m map[string][]CallSite) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestRangeEndsAtHEAD(t *testing.T) {
	cases := []struct {
		rng  string
		want bool
	}{
		{"HEAD~3..HEAD", true},
		{"HEAD~1..HEAD", true},
		{"abc123..", true},        // empty head → HEAD in git
		{"main...feat/x", false},  // branch tip, not HEAD
		{"HEAD~5..HEAD~1", false}, // older revision
		{"v1.0.0..v2.0.0", false},
		{"dev...HEAD", true},
	}
	for _, c := range cases {
		if got := rangeEndsAtHEAD(c.rng); got != c.want {
			t.Errorf("rangeEndsAtHEAD(%q) = %v, want %v", c.rng, got, c.want)
		}
	}
}

func TestDropLowRiskHunks(t *testing.T) {
	in := []ReviewHunk{
		{NewStart: 1, RiskTier: "low"},
		{NewStart: 2, RiskTier: "medium"},
		{NewStart: 3, RiskTier: "high"},
		{NewStart: 4, RiskTier: "low"},
	}
	out := dropLowRiskHunks(in)
	if len(out) != 2 || out[0].RiskTier != "medium" || out[1].RiskTier != "high" {
		t.Errorf("dropLowRiskHunks = %+v, want [medium, high]", out)
	}
}

func TestResolveReviewRange(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		in         ReviewInput
		wantRange  string
		wantStatus string
	}{
		{ReviewInput{Ref: "HEAD~3..HEAD"}, "HEAD~3..HEAD", "ok"},
		{ReviewInput{Ref: "HEAD~3"}, "HEAD~3..HEAD", "ok"}, // single ref → ref..HEAD
		{ReviewInput{Ref: "ref; rm -rf /"}, "", "error"},   // injection rejected
		{ReviewInput{Branch: "feat/x"}, "main...feat/x", "ok"},
		{ReviewInput{Branch: "feat/x", Base: "dev"}, "dev...feat/x", "ok"},
		{ReviewInput{Branch: "bad branch"}, "", "error"},
		{ReviewInput{Worktree: true}, "HEAD", "ok"},                              // #137: working tree vs HEAD
		{ReviewInput{Ref: "HEAD~3..HEAD", Worktree: true}, "HEAD~3..HEAD", "ok"}, // ref wins over worktree
		{ReviewInput{}, "", "error"},                                             // nothing selected still errors at this layer
	}
	for _, c := range cases {
		rng, status, _ := resolveReviewRange(ctx, t.TempDir(), c.in)
		if status != c.wantStatus || (c.wantStatus == "ok" && rng != c.wantRange) {
			t.Errorf("resolveReviewRange(%+v) = (%q,%q), want (%q,%q)", c.in, rng, status, c.wantRange, c.wantStatus)
		}
	}
}

// TestReviewNoSelectorDefaultsToWorktree locks the #137 default: an empty
// selector no longer errors — review() defaults to the uncommitted working tree.
// With no index built it stops at no-index (proving it passed the selector guard
// rather than rejecting the call for a missing ref/branch/pr).
func TestReviewNoSelectorDefaultsToWorktree(t *testing.T) {
	s := &Server{IndexDir: t.TempDir()}
	_, out, _ := s.review(context.Background(), nil, ReviewInput{ProjectRoot: t.TempDir()})
	if out.Status != "no-index" {
		t.Errorf("review(empty) = (%q,%q), want no-index (worktree default, not a selector error)", out.Status, out.Hint)
	}
}

// TestReviewResponseWrapsUnion asserts the #144 ask-merge slice-5a wiring: an
// intent=review ask short-circuits to reviewResponse, which carries the
// delta-shaped ReviewOutput in the discriminated-union ContextOutput.Review
// field and leaves the state-shaped lanes empty. The no-index temp dir just
// exercises the wrapping seam (the review composition's own paths are covered
// by TestReviewIntegration); what matters is that the union is populated, the
// intent is stamped, and no state lane leaks into a delta result.
func TestReviewResponseWrapsUnion(t *testing.T) {
	if got, _ := retrieve.ResolveIntent("review my changes", ""); got != retrieve.IntentReview {
		t.Fatalf("ResolveIntent(review my changes) = %q, want %q", got, retrieve.IntentReview)
	}
	s := &Server{IndexDir: t.TempDir()}
	_, out, _ := s.reviewResponse(context.Background(), ContextInput{ProjectRoot: t.TempDir()})
	if out.Intent != retrieve.IntentReview {
		t.Errorf("intent = %q, want %q", out.Intent, retrieve.IntentReview)
	}
	if out.Review == nil {
		t.Fatal("Review union field is nil; want the delta-shaped result carried here")
	}
	if out.Review.Status != "no-index" {
		t.Errorf("Review.Status = %q, want no-index (worktree default seam)", out.Review.Status)
	}
	if out.Status != out.Review.Status {
		t.Errorf("top-level Status %q should mirror Review.Status %q", out.Status, out.Review.Status)
	}
	if len(out.SemanticHits) != 0 || len(out.Symbols) != 0 || len(out.SuggestedReads) != 0 {
		t.Errorf("state lanes must stay empty for a delta result: hits=%d symbols=%d reads=%d",
			len(out.SemanticHits), len(out.Symbols), len(out.SuggestedReads))
	}
}

func TestReviewNoIndex(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	_, out, _ := s.review(context.Background(), nil, ReviewInput{
		Ref: "HEAD~1..HEAD", ProjectRoot: t.TempDir(),
	})
	if out.Status != "no-index" {
		t.Errorf("status = %q, want no-index", out.Status)
	}
}

// gitRun runs a git command in dir with a hermetic environment and a
// deterministic identity. The hermeticity is load-bearing: when these tests run
// under the repo's own git hooks (e.g. the pre-push CI gate), git exports
// GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE into the environment. Inheriting
// those via os.Environ() would make `git init`/`add`/`commit` operate on the
// REAL repository instead of the throwaway temp dir — silently committing test
// fixtures onto the live branch. So we strip every inherited GIT_* var and set
// only the identity we need; -c core.hooksPath=/dev/null also blocks any
// ambient hook from firing inside the fixture repo.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	env := make([]string, 0, len(os.Environ())+6)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue // drop GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE/etc. — they'd hijack the real repo
		}
		env = append(env, kv)
	}
	cmd.Env = append(env,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestReviewIntegration indexes a two-commit git repo and reviews the last
// commit, asserting the diff → hunk → symbol composition produces a touched
// symbol with the file-level history attached.
func TestReviewIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	srv := fakeEmbed(t, 16)
	t.Cleanup(srv.Close)
	cacheDir := t.TempDir()
	projDir := t.TempDir()

	gitRun(t, projDir, "init", "-q")
	// v1: Greet + a caller.
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n\nfunc Caller() string { return Greet(\"x\") }\n")
	writeFile(t, filepath.Join(projDir, "greet_test.go"),
		"package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) { _ = Greet(\"y\") }\n")
	gitRun(t, projDir, "add", ".")
	gitRun(t, projDir, "commit", "-q", "-m", "v1")
	// v2: modify Greet's body.
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(name string) string { return \"hello \" + name }\n\nfunc Caller() string { return Greet(\"x\") }\n")
	gitRun(t, projDir, "add", ".")
	gitRun(t, projDir, "commit", "-q", "-m", "v2")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.review(context.Background(), nil, ReviewInput{
		Ref: "HEAD~1..HEAD", ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if len(out.Files) != 1 || out.Files[0].Path != "greet.go" {
		t.Fatalf("files = %+v, want one greet.go", out.Files)
	}
	f := out.Files[0]
	if f.Status != "modified" {
		t.Errorf("status = %q, want modified", f.Status)
	}
	if out.TotalHunks == 0 || len(f.Hunks) == 0 {
		t.Fatalf("no hunks emitted: %+v", f)
	}
	// The modified line is inside Greet — the symbol lane must resolve it.
	var sawGreet bool
	for _, h := range f.Hunks {
		for _, sym := range h.SymbolsTouched {
			if sym.Name == "Greet" {
				sawGreet = true
				if !sym.Exported {
					t.Errorf("Greet should be marked exported")
				}
			}
		}
	}
	if !sawGreet {
		t.Errorf("expected Greet among touched symbols, hunks=%+v", f.Hunks)
	}
	// Guard #700: no hunk should list the same symbol name twice.
	for _, h := range f.Hunks {
		seen := map[string]int{}
		for _, sym := range h.SymbolsTouched {
			seen[sym.Name]++
		}
		for name, count := range seen {
			if count > 1 {
				t.Errorf("hunk @%d has %d duplicate entries for symbol %q (want 1)", h.NewStart, count, name)
			}
		}
	}
	// #136: caller bodies are hoisted to the top-level map, keyed by symbol.
	// Its keys must always be a subset of the symbols the emitted hunks touch —
	// no orphan entries (holds even when the graph lane is empty, as here).
	touched := map[string]bool{}
	for _, h := range f.Hunks {
		for _, sym := range h.SymbolsTouched {
			touched[sym.Name] = true
		}
	}
	for name := range out.CallersBySymbol {
		if !touched[name] {
			t.Errorf("CallersBySymbol has orphan key %q not in any emitted hunk", name)
		}
	}
	// File-level history legs are best-effort but should be populated here.
	if f.LastCommit == "" {
		t.Errorf("expected last_commit to be set")
	}
	if len(f.Tests) == 0 || f.Tests[0] != "greet_test.go" {
		t.Errorf("tests = %v, want [greet_test.go]", f.Tests)
	}
}

// TestReviewWorktree (#137) commits v1, then edits the working tree WITHOUT
// committing, and reviews with Worktree:true — the uncommitted change to Greet
// must surface via `git diff HEAD`, proving the working-tree lane composes like
// the committed-range lane.
func TestReviewWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	srv := fakeEmbed(t, 16)
	t.Cleanup(srv.Close)
	cacheDir := t.TempDir()
	projDir := t.TempDir()

	gitRun(t, projDir, "init", "-q")
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n")
	writeFile(t, filepath.Join(projDir, "greet_test.go"),
		"package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) { _ = Greet(\"y\") }\n")
	gitRun(t, projDir, "add", ".")
	gitRun(t, projDir, "commit", "-q", "-m", "v1")
	// Uncommitted edit to Greet's body — this is what "review my changes" means.
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(name string) string { return \"hello \" + name }\n")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.review(context.Background(), nil, ReviewInput{
		Worktree: true, ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if out.Range != "HEAD" {
		t.Errorf("range = %q, want HEAD (working tree vs HEAD)", out.Range)
	}
	if len(out.Files) != 1 || out.Files[0].Path != "greet.go" {
		t.Fatalf("files = %+v, want one greet.go", out.Files)
	}
	var sawGreet bool
	for _, h := range out.Files[0].Hunks {
		for _, sym := range h.SymbolsTouched {
			if sym.Name == "Greet" {
				sawGreet = true
			}
		}
	}
	if !sawGreet {
		t.Errorf("expected Greet among touched symbols, hunks=%+v", out.Files[0].Hunks)
	}

	// #155 P3: a .gate/findings.jsonl artifact should fold into the review —
	// findings for a touched file attach to it (path-cleaned), findings for an
	// untouched file are excluded, and a malformed line is tolerated.
	gateDir := filepath.Join(projDir, ".gate")
	if err := os.MkdirAll(gateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(gateDir, "findings.jsonl"),
		`{"tool":"budget","rule":"god-file","level":"warning","path":"./greet.go","line":1,"message":"x"}`+"\n"+
			`{"tool":"dupl","rule":"clone-pair","level":"warning","path":"other.go","line":9,"message":"y"}`+"\n"+
			`{ not valid json`+"\n")
	_, out2, err := s.review(context.Background(), nil, ReviewInput{Worktree: true, ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(out2.Files) != 1 {
		t.Fatalf("files = %+v, want one", out2.Files)
	}
	gf := out2.Files[0].GateFindings
	if len(gf) != 1 || gf[0].Rule != "god-file" {
		t.Fatalf("GateFindings = %+v, want exactly the god-file finding (./greet.go cleaned + matched; other.go excluded; bad line skipped)", gf)
	}
	if !strings.Contains(out2.Hint, "gate finding") {
		t.Errorf("hint = %q, want a gate-findings note", out2.Hint)
	}
}

// TestReviewIsExpertGated guards the 5c collapse (#145): review_diff is the
// targeted PR/branch/ref review escape hatch, gated behind DEX_EXPERT — the
// everyday worktree review is ask("review my changes") (#144). Absent from the
// default surface, present once expert is on.
func TestReviewIsExpertGated(t *testing.T) {
	t.Setenv("DEX_EXPERT", "")
	if listToolNames(t, stubServer(t))["review_diff"] {
		t.Error("default surface advertised review_diff; want it gated behind DEX_EXPERT")
	}
	t.Setenv("DEX_EXPERT", "1")
	if !listToolNames(t, stubServer(t))["review_diff"] {
		t.Error("DEX_EXPERT=1 but review_diff not advertised")
	}
}

// TestReviewNonCodeFileCap guards that a file with no indexed symbols is capped
// at reviewMaxHunksNoCode hunks so it can't starve code files in the same diff.
func TestReviewNonCodeFileCap(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	srv := fakeEmbed(t, 16)
	t.Cleanup(srv.Close)
	cacheDir := t.TempDir()
	projDir := t.TempDir()

	gitRun(t, projDir, "init", "-q")

	// v1: a code file + a JSON data file with many values spaced far enough
	// apart that each change in v2 becomes a separate git diff hunk (>= 7
	// unchanged lines between changes keeps hunks distinct with -U3 context).
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n")

	// 10 entries each separated by 10 filler keys → each value change is a distinct hunk.
	var jsonV1, jsonV2 strings.Builder
	jsonV1.WriteString("{\n")
	jsonV2.WriteString("{\n")
	for i := 0; i < 10; i++ {
		jsonV1.WriteString(strings.Repeat("  \"filler\": \"x\",\n", 10))
		jsonV1.WriteString("  \"key\": \"old\"")
		jsonV2.WriteString(strings.Repeat("  \"filler\": \"x\",\n", 10))
		jsonV2.WriteString("  \"key\": \"new\"")
		if i < 9 {
			jsonV1.WriteString(",\n")
			jsonV2.WriteString(",\n")
		} else {
			jsonV1.WriteString("\n")
			jsonV2.WriteString("\n")
		}
	}
	jsonV1.WriteString("}\n")
	jsonV2.WriteString("}\n")
	writeFile(t, filepath.Join(projDir, "data.json"), jsonV1.String())

	gitRun(t, projDir, "add", ".")
	gitRun(t, projDir, "commit", "-q", "-m", "v1")

	// v2: change the code file + update all 10 JSON values → 10 diff hunks.
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(name string) string { return \"hello \" + name }\n")
	writeFile(t, filepath.Join(projDir, "data.json"), jsonV2.String())
	gitRun(t, projDir, "add", ".")
	gitRun(t, projDir, "commit", "-q", "-m", "v2")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.review(context.Background(), nil, ReviewInput{
		Ref: "HEAD~1..HEAD", ProjectRoot: root,
	})
	if err != nil || out.Status != "ok" {
		t.Fatalf("review: status=%q hint=%q err=%v", out.Status, out.Hint, err)
	}

	// Find per-file hunk counts.
	jsonHunks, codeHunks := -1, -1
	for _, f := range out.Files {
		switch filepath.Ext(f.Path) {
		case ".json":
			jsonHunks = len(f.Hunks)
		case ".go":
			codeHunks = len(f.Hunks)
		}
	}
	if codeHunks <= 0 {
		t.Errorf("code file missing from review output: %+v", out.Files)
	}
	if jsonHunks > reviewMaxHunksNoCode {
		t.Errorf("data file emitted %d hunks, want <= %d (reviewMaxHunksNoCode)", jsonHunks, reviewMaxHunksNoCode)
	}
}

func TestExtractNewRef(t *testing.T) {
	cases := []struct{ rng, want string }{
		{"HEAD~5..HEAD~1", "HEAD~1"},
		{"main...feat/x", "feat/x"},
		{"HEAD~1..HEAD", ""}, // ends at HEAD → live index
		{"abc123..", ""},     // empty rhs → HEAD
		{"HEAD~1..@", ""},    // @ = HEAD alias
		{"v1.0..v2.0", "v2.0"},
	}
	for _, c := range cases {
		if got := extractNewRef(c.rng); got != c.want {
			t.Errorf("extractNewRef(%q) = %q, want %q", c.rng, got, c.want)
		}
	}
}

// TestReviewTimeTravel (#644): reviewing a range NOT ending at HEAD resolves
// symbols against the diff's own new-side ref, not the live index. The test
// fabricates a scenario where the live index has Greet at line 3, but in the
// historical diff (v1→v2) Greet was at line 53 (after a 50-line preamble).
// Without time-travel, ChunkAt(path, 53) fails (nothing at line 53 in HEAD).
// With time-travel, git show HEAD~1:greet.go is chunked and Greet is found.
func TestReviewTimeTravel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	srv := fakeEmbed(t, 16)
	t.Cleanup(srv.Close)
	cacheDir := t.TempDir()
	projDir := t.TempDir()

	gitRun(t, projDir, "init", "-q")

	// v1: Greet at line 3 (minimal file).
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(s string) string { return s }\n")
	gitRun(t, projDir, "add", ".")
	gitRun(t, projDir, "commit", "-q", "-m", "v1")

	// v2: 50 blank comment lines prepended so Greet moves to line 53,
	// and its body is also changed. The historical diff's new-side line
	// numbers are therefore 50+ higher than what the live index sees.
	preamble := strings.Repeat("// pad\n", 50)
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\n"+preamble+"func Greet(s string) string { return \"hello \"+s }\n")
	gitRun(t, projDir, "add", ".")
	gitRun(t, projDir, "commit", "-q", "-m", "v2")

	// v3 (HEAD): revert to compact file so Greet is back at line 3.
	// The live index (indexed at HEAD = v3) therefore has Greet at line 3.
	// Reviewing HEAD~2..HEAD~1 (v1→v2) produces a hunk with new-side lines
	// around 53 — which would find nothing in the live index.
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(s string) string { return \"hi \"+s }\n")
	gitRun(t, projDir, "add", ".")
	gitRun(t, projDir, "commit", "-q", "-m", "v3")

	root := indexProject(t, projDir, cacheDir, srv.URL) // indexes at HEAD (v3)
	s := newServer(srv.URL, cacheDir)

	// Review v1→v2 (historical, does not end at HEAD).
	_, out, err := s.review(context.Background(), nil, ReviewInput{
		Ref: "HEAD~2..HEAD~1", ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q hint = %q", out.Status, out.Hint)
	}
	if len(out.Files) == 0 {
		t.Fatal("no files in review output")
	}
	// The hint must say callers/risk reflect the current index (not the old degradation message).
	if !strings.Contains(out.Hint, "callers and risk tiers reflect the current index") {
		t.Errorf("unexpected hint %q", out.Hint)
	}
	// Time-travel must resolve Greet even though it was at line 53 in v2.
	var sawGreet bool
	for _, f := range out.Files {
		for _, h := range f.Hunks {
			for _, sym := range h.SymbolsTouched {
				if sym.Name == "Greet" {
					sawGreet = true
				}
			}
		}
	}
	if !sawGreet {
		t.Errorf("time-travel failed: Greet not in symbols_touched for historical diff (hunks: %+v)", out.Files)
	}
}
