package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// unsetGitEnv scrubs GIT_* environment variables for the duration of the test.
// When the suite is invoked from inside a git hook (e.g. the pre-push gate),
// git exports GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE; an inherited GIT_DIR
// makes git operate on the REAL repo regardless of working directory — both the
// test's own commits (clobbering the live branch + index) and the in-process
// GenerateBlastRadius git calls (which then read the wrong repo). Clearing them
// process-wide keeps the whole test hermetic; they're restored on cleanup.
func unsetGitEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, "GIT_") {
			continue
		}
		old := os.Getenv(k)
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
		t.Cleanup(func() { _ = os.Setenv(k, old) })
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitInitRepo creates a temp git repo with deterministic identity.
func gitInitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "config", "user.email", "t@example.com")
	gitIn(t, dir, "config", "user.name", "t")
	gitIn(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

func gitCommitAll(t *testing.T, dir, msg string) {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "--no-verify", "-m", msg)
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateBlastRadius(t *testing.T) {
	unsetGitEnv(t)
	dir := gitInitRepo(t)
	// One multi-file commit (3 code files) → blast-radius examples.
	write(t, dir, "a.go", "package p\n\nfunc Alpha() int { return 1 }\n")
	write(t, dir, "b.go", "package p\n\nfunc Beta() int { return Alpha() + 1 }\n")
	write(t, dir, "c.go", "package p\n\nfunc Gamma() int { return Beta() * 2 }\n")
	gitCommitAll(t, dir, "feat: add alpha/beta/gamma")
	// One single-file commit → must NOT produce a blast-radius example.
	write(t, dir, "solo.go", "package p\n\nfunc Solo() {}\n")
	gitCommitAll(t, dir, "feat: solo")

	gs, err := GenerateBlastRadius(context.Background(), dir, GenOpts{MaxCommits: 10, MaxFiles: 5})
	if err != nil {
		t.Fatal(err)
	}

	// 3 anchors from the 3-file commit, none from the single-file commit.
	if len(gs.Queries) != 3 {
		t.Fatalf("got %d queries, want 3; %+v", len(gs.Queries), gs.Queries)
	}

	for _, q := range gs.Queries {
		if q.Anchor == "" {
			t.Errorf("query %s has no anchor", q.ID)
		}
		if q.Query == "" {
			t.Errorf("query %s has empty excerpt", q.ID)
		}
		// Relevant = the other two files, anchor excluded.
		if len(q.RelevantFiles) != 2 {
			t.Errorf("anchor %s: got %d relevant, want 2 (%v)", q.Anchor, len(q.RelevantFiles), q.RelevantFiles)
		}
		for _, r := range q.RelevantFiles {
			if r == q.Anchor {
				t.Errorf("anchor %s appears in its own relevant set", q.Anchor)
			}
		}
		if !sort.StringsAreSorted(q.RelevantFiles) {
			t.Errorf("relevant files not sorted: %v", q.RelevantFiles)
		}
	}

	// "solo.go" must never be an anchor (single-file commit).
	for _, q := range gs.Queries {
		if q.Anchor == "solo.go" {
			t.Errorf("solo.go (single-file commit) became a blast-radius anchor")
		}
	}
}

func TestCodeExcerptSkipsCommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("// header comment\n\n# hashish\npackage p\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := codeExcerpt(p)
	want := "package p\nfunc F() {}"
	if got != want {
		t.Errorf("codeExcerpt = %q, want %q", got, want)
	}
	if codeExcerpt(filepath.Join(dir, "missing.go")) != "" {
		t.Errorf("missing file should yield empty excerpt")
	}
}
