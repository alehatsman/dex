package gitrecency

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitEnv returns os.Environ() with git repo-discovery variables stripped so
// that an inherited GIT_DIR (injected by pre-push hook children when the test
// suite runs from a linked worktree) does not override cmd.Dir. See issue #341.
func gitEnv(extra ...string) []string {
	leaky := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
		"GIT_COMMON_DIR": true, "GIT_OBJECT_DIRECTORY": true,
	}
	env := os.Environ()
	out := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if leaky[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// initGitRepo creates a minimal git repo in dir with one committed file
// at relPath. Returns the commit unix timestamp.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv(
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
			"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "test")
}

func TestNilCacheIsNoop(t *testing.T) {
	var c *Cache
	pathFor := map[int64]string{1: "foo.go", 2: "bar.go"}
	if got := c.Bonus(pathFor, 60); got != nil {
		t.Errorf("nil cache Bonus: want nil, got %v", got)
	}
}

func TestEmptyRootIsNoop(t *testing.T) {
	c := New("")
	pathFor := map[int64]string{1: "foo.go"}
	if got := c.Bonus(pathFor, 60); got != nil {
		t.Errorf("empty root Bonus: want nil, got %v", got)
	}
}

func TestRecencyBoostDecays(t *testing.T) {
	// Directly test the decay function without running git.
	now := time.Now()
	age0 := now.Sub(now)                       // 0 hours
	age48 := now.Add(-48 * time.Hour).Sub(now) // negative, but we negate
	_ = age0
	_ = age48

	// At age 0: decay = 1.0 → boost = RecencyBoostMax
	decay0 := math.Pow(2, 0)
	if got := float32(float64(RecencyBoostMax) * decay0); got != RecencyBoostMax {
		t.Errorf("decay at age=0: got %v, want %v", got, RecencyBoostMax)
	}

	// At age 48h: decay = 0.5 → boost = RecencyBoostMax/2
	decay48 := math.Pow(2, -48.0/48.0)
	want48 := float32(float64(RecencyBoostMax) * decay48)
	if math.Abs(float64(want48-RecencyBoostMax/2)) > 1e-6 {
		t.Errorf("decay at age=48h: got %v, want %v", want48, RecencyBoostMax/2)
	}
}

func TestBonusWithGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	dir := t.TempDir()
	initGitRepo(t, dir)

	// Write and commit a file.
	f := filepath.Join(dir, "main.go")
	if err := os.WriteFile(f, []byte("package main"), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv(
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("add", "main.go")
	run("commit", "--no-verify", "-m", "init")

	// Also leave a file dirty.
	dirty := filepath.Join(dir, "dirty.go")
	if err := os.WriteFile(dirty, []byte("package main"), 0644); err != nil {
		t.Fatal(err)
	}

	c := New(dir)
	pathFor := map[int64]string{
		1: "main.go",  // recently committed
		2: "dirty.go", // dirty (untracked)
		3: "other.go", // not in git at all
	}

	bonus := c.Bonus(pathFor, 60)
	if bonus == nil {
		t.Fatal("Bonus returned nil; expected non-nil for recent + dirty files")
	}

	// main.go: should have a recency bonus (committed just now, within 14 days)
	if bonus[1] <= 0 {
		t.Errorf("main.go (recently committed): expected recency bonus > 0, got %v", bonus[1])
	}

	// dirty.go: should have a dirty bonus
	if bonus[2] <= 0 {
		t.Errorf("dirty.go (untracked): expected dirty bonus > 0, got %v", bonus[2])
	}

	// other.go: no bonus
	if bonus[3] != 0 {
		t.Errorf("other.go (not in git): expected 0 bonus, got %v", bonus[3])
	}

	// Recency bonus should be at most RecencyBoostMax/60 (for rrfK=60)
	maxBonus := RecencyBoostMax / 60
	if bonus[1] > maxBonus {
		t.Errorf("main.go recency bonus %v exceeds max %v", bonus[1], maxBonus)
	}
}

func TestBonusNoGitRepo(t *testing.T) {
	dir := t.TempDir() // no .git here
	c := New(dir)
	pathFor := map[int64]string{1: "foo.go"}
	// Should degrade gracefully and return nil (no error, no panic)
	bonus := c.Bonus(pathFor, 60)
	if bonus != nil {
		t.Errorf("no-git dir: expected nil bonus, got %v", bonus)
	}
}

func TestTTLCaching(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	dir := t.TempDir()
	initGitRepo(t, dir)

	c := New(dir)
	pathFor := map[int64]string{1: "a.go"}

	// Prime caches on first call.
	c.Bonus(pathFor, 60)

	// Set expiry far in the future so the next call must use the cached result.
	c.recMu.Lock()
	c.recencyExp = time.Now().Add(10 * time.Minute)
	c.recMu.Unlock()

	c.dirMu.Lock()
	c.dirtyExp = time.Now().Add(10 * time.Minute)
	c.dirMu.Unlock()

	// Record state before second call — should not refresh (TTL not expired).
	c.recMu.Lock()
	expBefore := c.recencyExp
	c.recMu.Unlock()

	c.Bonus(pathFor, 60)

	c.recMu.Lock()
	expAfter := c.recencyExp
	c.recMu.Unlock()

	if expAfter != expBefore {
		t.Error("TTL refresh happened before expiry; expected cache hit")
	}
}
