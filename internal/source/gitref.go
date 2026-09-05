package source

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/gitenv"
)

// ReadAtRef returns the contents of path as of git ref (#644 time-travel). It
// locates the file's git repository, makes the path repo-relative, and runs
// `git show <ref>:<relpath>` so the read works from any subdirectory. The ref
// must name a revision, not a git option. Backs the MCP read tool's ref
// parameter (DEX_EXPERT — dropped from the CLI's `dex query` front door by
// #849, no query facet for a historical read).
func ReadAtRef(ctx context.Context, path, ref string) ([]byte, error) {
	if strings.HasPrefix(ref, "-") {
		// A ref starting with '-' would be parsed by git as an option, not a
		// revision — reject rather than risk option injection.
		return nil, fmt.Errorf("invalid ref %q (must not start with '-')", ref)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Resolve symlinks so filepath.Rel matches the canonical path that
	// git rev-parse --show-toplevel returns (e.g. /private/var on macOS).
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	dir := filepath.Dir(abs)
	rootOut, err := gitRead(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not a git repository at %s: %w", dir, err)
	}
	root := strings.TrimSpace(string(rootOut))
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return nil, err
	}
	out, err := gitRead(ctx, root, "show", ref+":"+filepath.ToSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s — check the ref and that the file existed then: %w", ref, rel, err)
	}
	return out, nil
}

// gitRead runs a read-only git command in dir with a hermetic environment.
// GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE are scrubbed so an injected value
// (e.g. from a hook) can't redirect git to the wrong repository (#341).
func gitRead(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitenv.Current()
	return cmd.Output()
}
