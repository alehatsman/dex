// Package shadow maintains a per-project SHADOW git repository — a private git
// history of the working tree kept entirely OUTSIDE the user's own .git, so an
// agent can checkpoint work-in-progress and review what it changed across a
// session without ever touching the user's repository (#608).
//
// Isolation is by construction. Every git invocation runs with:
//   - an environment scrubbed of inherited GIT_* discovery vars (the #341
//     bare-repo-wipe / test-hijack hazard), then
//   - explicit GIT_DIR (the shadow dir) + GIT_WORK_TREE (the project root).
//
// GIT_DIR set in the environment takes precedence over git's repo discovery, so
// no command can ever fall through to the user's .git. The shadow is the only
// repo written; the user's tree is only ever READ (staged into the shadow).
// There is deliberately no restore/write-back: dex stays read-only on the user's
// working tree (#551) — the agent applies any rollback itself from a diff.
package shadow

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// gitTimeout bounds a single shadow git invocation. `add -A` over a large tree
// is the slow case; 60s is generous without hanging an agent turn forever.
const gitTimeout = 60 * time.Second

// reValidRef restricts user-supplied diff endpoints to commit-ish tokens
// (SHALabels, HEAD, HEAD~1, ^, branch-ish). It forbids whitespace and shell
// metacharacters so a ref can never smuggle extra arguments.
var reValidRef = regexp.MustCompile(`^[A-Za-z0-9_./~^@-]+$`)

// Repo is a project's shadow git repository.
type Repo struct {
	gitDir   string // <cacheDir>/shadow — the isolated git directory
	workTree string // the user's project root (read-only to us)
}

// Open returns the shadow repo handle for a project. cacheDir is the project's
// dex cache dir (proj.Project.CacheDir); workTree is its root. Nothing is
// created until the first Snapshot.
func Open(cacheDir, workTree string) *Repo {
	return &Repo{gitDir: filepath.Join(cacheDir, "shadow"), workTree: workTree}
}

// Commit is one shadow checkpoint.
type Commit struct {
	SHA     string    `json:"sha"`
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

// SnapshotResult reports the outcome of a Snapshot.
type SnapshotResult struct {
	SHA          string `json:"sha"`
	FilesChanged int    `json:"files_changed"`
	Created      bool   `json:"created"` // false when the tree was unchanged (no new commit)
}

// hermeticGitEnv returns the ambient environment with git's repo-discovery
// variables stripped, then pins GIT_DIR + GIT_WORK_TREE to this shadow. Copy of
// the mcp/corpus helper (issue #341) — kept local so the shadow package owns its
// isolation guarantee.
func (r *Repo) env() []string {
	leaky := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
		"GIT_COMMON_DIR": true, "GIT_OBJECT_DIRECTORY": true,
		"GIT_NAMESPACE": true, "GIT_PREFIX": true,
	}
	src := os.Environ()
	out := make([]string, 0, len(src)+2)
	for _, kv := range src {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if leaky[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_DIR="+r.gitDir, "GIT_WORK_TREE="+r.workTree)
}

// git runs a git subcommand against the shadow and returns trimmed stdout.
func (r *Repo) git(ctx context.Context, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...) // #nosec G204 — args are literals or reValidRef-validated refs
	cmd.Env = r.env()
	cmd.Dir = r.workTree // for .gitignore resolution; GIT_DIR env still pins the repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// initialized reports whether the shadow repo exists.
func (r *Repo) initialized() bool {
	_, err := os.Stat(filepath.Join(r.gitDir, "HEAD"))
	return err == nil
}

// ensure creates and configures the shadow repo on first use. Idempotent.
func (r *Repo) ensure(ctx context.Context) error {
	if r.initialized() {
		return nil
	}
	if err := os.MkdirAll(r.gitDir, 0o750); err != nil {
		return err
	}
	if _, err := r.git(ctx, "init", "--quiet"); err != nil {
		return err
	}
	// Self-contained config so commits never need the user's global git identity
	// or a GPG key, and no background GC mutates the shadow under us.
	cfg := [][2]string{
		{"user.email", "checkpoint@dex.local"},
		{"user.name", "dex checkpoint"},
		{"commit.gpgsign", "false"},
		{"gc.auto", "0"},
		{"core.autocrlf", "false"},
		{"core.safecrlf", "false"},
	}
	for _, kv := range cfg {
		if _, err := r.git(ctx, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// Snapshot stages the whole working tree into the shadow and commits it. When
// the tree is unchanged since the last snapshot it returns the existing HEAD
// with Created=false rather than an empty commit (idempotent).
func (r *Repo) Snapshot(ctx context.Context, message string) (SnapshotResult, error) {
	if err := r.ensure(ctx); err != nil {
		return SnapshotResult{}, err
	}
	if _, err := r.git(ctx, "add", "-A"); err != nil {
		return SnapshotResult{}, err
	}
	staged, err := r.git(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		return SnapshotResult{}, err
	}
	if strings.TrimSpace(staged) == "" {
		// Nothing changed — return the current HEAD (if any) without committing.
		head, _ := r.git(ctx, "rev-parse", "HEAD")
		return SnapshotResult{SHA: head, FilesChanged: 0, Created: false}, nil
	}
	files := len(strings.Split(staged, "\n"))
	if strings.TrimSpace(message) == "" {
		message = "checkpoint " + time.Now().UTC().Format("2006-01-02 15:04:05")
	}
	if _, err := r.git(ctx, "commit", "--quiet", "--no-gpg-sign", "-m", message); err != nil {
		return SnapshotResult{}, err
	}
	head, err := r.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return SnapshotResult{}, err
	}
	return SnapshotResult{SHA: head, FilesChanged: files, Created: true}, nil
}

// Log returns up to limit checkpoints, newest first. An uninitialised shadow
// (no snapshot yet) returns an empty slice, not an error.
func (r *Repo) Log(ctx context.Context, limit int) ([]Commit, error) {
	if !r.initialized() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	// NUL-delimited fields, newline-delimited records — robust against newlines
	// or NULs never appearing in a SHA/unix-time, and messages are single-line
	// (%s = subject).
	out, err := r.git(ctx, "log", "--format=%H%x00%ct%x00%s", "-n", strconv.Itoa(limit))
	if err != nil {
		// A fresh repo with zero commits exits non-zero on `log`; treat as empty.
		if strings.Contains(err.Error(), "does not have any commits") ||
			strings.Contains(err.Error(), "bad default revision") {
			return nil, nil
		}
		return nil, err
	}
	var commits []Commit
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\x00", 3)
		if len(f) != 3 {
			continue
		}
		sec, _ := strconv.ParseInt(f[1], 10, 64)
		commits = append(commits, Commit{SHA: f[0], Time: time.Unix(sec, 0), Message: f[2]})
	}
	return commits, nil
}

// Diff returns the unified diff between two checkpoints. Empty from/to default
// to HEAD~1..HEAD. Refs are validated to commit-ish tokens. An uninitialised
// shadow returns an empty diff.
func (r *Repo) Diff(ctx context.Context, from, to string) (string, error) {
	if !r.initialized() {
		return "", nil
	}
	if from == "" {
		from = "HEAD~1"
	}
	if to == "" {
		to = "HEAD"
	}
	for _, ref := range []string{from, to} {
		if !reValidRef.MatchString(ref) {
			return "", fmt.Errorf("invalid ref %q", ref)
		}
	}
	return r.git(ctx, "diff", "--no-color", from+".."+to)
}
