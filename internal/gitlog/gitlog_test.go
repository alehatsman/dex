package gitlog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

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

func TestCollectParsesCommitsAndFiles(t *testing.T) {
	unsetGitEnv(t)
	dir := gitInitRepo(t)
	write(t, dir, "a.go", "package p\n")
	write(t, dir, "b.go", "package p\n")
	gitCommitAll(t, dir, "add a and b")
	write(t, dir, "c.go", "package p\n")
	gitCommitAll(t, dir, "add c")

	commits, err := Collect(context.Background(), dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 {
		t.Fatalf("got %d commits, want 2: %+v", len(commits), commits)
	}
	// git log is newest-first.
	if commits[0].Subject != "add c" || !reflect.DeepEqual(commits[0].Files, []string{"c.go"}) {
		t.Errorf("commits[0] = %+v, want subject=%q files=[c.go]", commits[0], "add c")
	}
	if len(commits[0].ShortHash) != 8 {
		t.Errorf("ShortHash = %q, want length 8", commits[0].ShortHash)
	}
	gotFiles := append([]string(nil), commits[1].Files...)
	sort.Strings(gotFiles)
	if commits[1].Subject != "add a and b" || !reflect.DeepEqual(gotFiles, []string{"a.go", "b.go"}) {
		t.Errorf("commits[1] = %+v, want subject=%q files=[a.go b.go]", commits[1], "add a and b")
	}
}

func TestCollectMaxCommitsCap(t *testing.T) {
	unsetGitEnv(t)
	dir := gitInitRepo(t)
	for i := 0; i < 5; i++ {
		write(t, dir, "f.go", "package p\n\n// v"+string(rune('0'+i))+"\n")
		gitCommitAll(t, dir, "edit f")
	}
	commits, err := Collect(context.Background(), dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 {
		t.Fatalf("got %d commits, want 2 (max cap)", len(commits))
	}
}

func TestCollectEmptyRepoNoError(t *testing.T) {
	unsetGitEnv(t)
	dir := gitInitRepo(t) // no commits
	commits, err := Collect(context.Background(), dir, 10)
	if err != nil {
		t.Fatalf("want no error on empty history, got %v", err)
	}
	if len(commits) != 0 {
		t.Fatalf("got %d commits, want 0", len(commits))
	}
}

func TestCollectNonGitRootNoError(t *testing.T) {
	unsetGitEnv(t)
	dir := t.TempDir() // not a git repo
	commits, err := Collect(context.Background(), dir, 10)
	if err != nil {
		t.Fatalf("want no error on non-git root, got %v", err)
	}
	if len(commits) != 0 {
		t.Fatalf("got %d commits, want 0", len(commits))
	}
}

// TestCollectMissingGitBinaryIsError checks that a genuine failure to run
// git at all (not "no history to read") is surfaced as a real error, not
// collapsed into the same (nil, nil) "nothing to mine" result as an empty
// repo — callers (mineCoChanges) rely on this distinction to know whether to
// preserve previously-mined state instead of treating the run as "history is
// now empty".
func TestCollectMissingGitBinaryIsError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", "") // git is unrunnable
	_, err := Collect(context.Background(), dir, 10)
	if err == nil {
		t.Fatal("want error when git binary can't be found, got nil")
	}
}

// TestCollectCancelledContextIsError checks the same distinction for a
// context that's already cancelled before the git-log walk runs.
func TestCollectCancelledContextIsError(t *testing.T) {
	unsetGitEnv(t)
	dir := gitInitRepo(t)
	write(t, dir, "a.go", "package p\n")
	gitCommitAll(t, dir, "add a")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Collect(ctx, dir, 10)
	if err == nil {
		t.Fatal("want error on cancelled context, got nil")
	}
}

// TestCollectKeepsEmptySubjectCommit checks that a commit with an empty
// subject line is no longer dropped wholesale — internal/graph/cochange.go
// only reads .Files, never .Subject, so filtering here only served
// internal/eval's need for non-empty query text; that filtering now happens
// in internal/eval's own callers instead.
func TestCollectKeepsEmptySubjectCommit(t *testing.T) {
	unsetGitEnv(t)
	dir := gitInitRepo(t)
	write(t, dir, "a.go", "package p\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "--no-verify", "--allow-empty-message", "-m", "")

	commits, err := Collect(context.Background(), dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 {
		t.Fatalf("got %d commits, want 1 (empty-subject commit kept): %+v", len(commits), commits)
	}
	if commits[0].Subject != "" {
		t.Errorf("Subject = %q, want empty", commits[0].Subject)
	}
	if !reflect.DeepEqual(commits[0].Files, []string{"a.go"}) {
		t.Errorf("Files = %v, want [a.go]", commits[0].Files)
	}
}
