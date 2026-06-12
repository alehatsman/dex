package corpus

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Ensure makes a checkout of spec at its pinned commit available under
// cacheRoot and returns the checkout directory. It is idempotent: a cache hit
// (the dir exists and its HEAD already equals the pin) does no network I/O.
//
// The clone is blob-filtered (--filter=blob:none): the full commit graph is
// fetched so eval.Generate/GenerateBlastRadius can mine auto-labels from the
// repo's own history, but historical file blobs are fetched lazily on checkout
// rather than all up front. The pinned commit must be reachable from the
// default branch or a tag (release-tag pins satisfy this).
func Ensure(ctx context.Context, spec RepoSpec, cacheRoot string) (string, error) {
	dir := filepath.Join(cacheRoot, spec.Name+"@"+shortSHA(spec.Commit))

	// Cache hit: dir exists and is already checked out at the pin.
	if head, err := gitHead(ctx, dir); err == nil {
		if head == spec.Commit {
			return dir, nil
		}
		// Wrong revision in an existing checkout: try to move it to the pin
		// in place before falling back to a fresh clone.
		if err := checkoutPin(ctx, dir, spec.Commit); err == nil {
			return dir, nil
		}
		// In-place move failed — discard and reclone.
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("corpus: clear stale checkout %q: %w", dir, err)
		}
	}

	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return "", fmt.Errorf("corpus: create cache root %q: %w", cacheRoot, err)
	}
	if err := runGit(ctx, "", "clone", "--filter=blob:none", spec.URL, dir); err != nil {
		return "", fmt.Errorf("corpus: clone %s: %w", spec.URL, err)
	}
	if err := checkoutPin(ctx, dir, spec.Commit); err != nil {
		return "", err
	}
	return dir, nil
}

// checkoutPin detaches the working tree at commit and verifies HEAD landed on
// it. If the commit is not yet present (e.g. unreferenced by any fetched ref)
// it attempts a targeted fetch first.
func checkoutPin(ctx context.Context, dir, commit string) error {
	if err := runGit(ctx, dir, "checkout", "--quiet", "--detach", commit); err != nil {
		// The commit may not be referenced by a fetched ref; try fetching it
		// directly (servers with uploadpack.allowReachableSHA1InWant honor this).
		if ferr := runGit(ctx, dir, "fetch", "--quiet", "origin", commit); ferr != nil {
			return fmt.Errorf("corpus: checkout %s: %w (fetch fallback: %v)", shortSHA(commit), err, ferr)
		}
		if err := runGit(ctx, dir, "checkout", "--quiet", "--detach", commit); err != nil {
			return fmt.Errorf("corpus: checkout %s: %w", shortSHA(commit), err)
		}
	}
	head, err := gitHead(ctx, dir)
	if err != nil {
		return err
	}
	if head != commit {
		return fmt.Errorf("corpus: HEAD %s != pinned %s after checkout", shortSHA(head), shortSHA(commit))
	}
	return nil
}

// gitHead returns the resolved HEAD commit of a checkout, or an error if dir is
// not a usable git working tree.
func gitHead(ctx context.Context, dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return "", err
	}
	out, err := gitOutput(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return out, nil
}

// hermeticGitEnv returns the ambient environment with git's repo-discovery
// variables stripped. Corpus git commands always target their own cache repo
// via cmd.Dir; an inherited GIT_DIR (e.g. injected into pre-commit/pre-push
// hook children when the test suite runs from a linked worktree) would
// otherwise override cmd.Dir and make `git init` reinitialize the real repo as
// bare — flipping the shared core.bare=true and wiping every worktree. See
// issue #341.
func hermeticGitEnv() []string {
	const prefix = "GIT_"
	leaky := map[string]bool{
		"GIT_DIR":              true,
		"GIT_WORK_TREE":        true,
		"GIT_INDEX_FILE":       true,
		"GIT_COMMON_DIR":       true,
		"GIT_OBJECT_DIRECTORY": true,
		"GIT_NAMESPACE":        true,
		"GIT_PREFIX":           true,
	}
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if strings.HasPrefix(k, prefix) && leaky[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = hermeticGitEnv()
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = hermeticGitEnv()
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
