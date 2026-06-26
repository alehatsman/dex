package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	}
	for _, c := range cases {
		rng, status, _ := resolveReviewRange(ctx, t.TempDir(), c.in)
		if status != c.wantStatus || (c.wantStatus == "ok" && rng != c.wantRange) {
			t.Errorf("resolveReviewRange(%+v) = (%q,%q), want (%q,%q)", c.in, rng, status, c.wantRange, c.wantStatus)
		}
	}
}

func TestReviewNoSelector(t *testing.T) {
	s := &Server{IndexDir: t.TempDir()}
	_, out, _ := s.review(context.Background(), nil, ReviewInput{ProjectRoot: t.TempDir()})
	if out.Status != "error" || !strings.Contains(out.Hint, "one of") {
		t.Errorf("review(empty) = (%q,%q), want error mentioning 'one of'", out.Status, out.Hint)
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
	// File-level history legs are best-effort but should be populated here.
	if f.LastCommit == "" {
		t.Errorf("expected last_commit to be set")
	}
	if len(f.Tests) == 0 || f.Tests[0] != "greet_test.go" {
		t.Errorf("tests = %v, want [greet_test.go]", f.Tests)
	}
}

// TestReviewInDefaultSurface guards that review ships in the everyday tool
// surface (not behind DEX_EXPERT) — it's a headline review verb.
func TestReviewInDefaultSurface(t *testing.T) {
	t.Setenv("DEX_EXPERT", "")
	names := listToolNames(t, stubServer(t))
	if !names["review"] {
		t.Error("default surface omitted verb \"review\"; want it advertised")
	}
}
