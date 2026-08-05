// Package gitworktree resolves a linked git worktree to its main working tree,
// purely from the filesystem — no `git` subprocess. dex keys its index and its
// opt-in `.dex/config.yml` off the project root, so a linked worktree (a
// distinct root with no config of its own) would otherwise index nothing. The
// config loaders call MainWorktree to inherit the parent checkout's config.
//
// A linked worktree's `.git` is a FILE (`gitdir: <main>/.git/worktrees/<name>`),
// unlike the main checkout (a `.git` directory) or a submodule (whose gitdir is
// `<super>/.git/modules/<name>`). MainWorktree fires only for the worktree case.
package gitworktree

import (
	"os"
	"path/filepath"
	"strings"
)

// MainWorktree reports the main working tree of a linked git worktree rooted at
// root. ok is false when root is not a linked worktree — its `.git` is a
// directory (the main checkout), missing, or a submodule gitdir — so callers
// fall through to their normal, non-inheriting behavior. Best-effort: any read
// or parse failure returns ("", false); inheritance is a convenience, never a
// hard dependency.
func MainWorktree(root string) (string, bool) {
	dotGit := filepath.Join(root, ".git")
	fi, err := os.Stat(dotGit)
	if err != nil || fi.IsDir() {
		return "", false // missing, or the main checkout's .git directory
	}

	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return "", false
	}
	gitDir := parseGitdir(string(raw))
	if gitDir == "" {
		return "", false
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Clean(filepath.Join(root, gitDir))
	}

	// A linked worktree's gitdir sits under `<common>/worktrees/<name>`. A
	// submodule's is `<super>/.git/modules/<name>` — no `worktrees` segment —
	// and must NOT inherit. This is what distinguishes the two.
	if !hasPathSegment(gitDir, "worktrees") {
		return "", false
	}

	// `commondir` (relative to gitDir, typically `../..`) points at the shared
	// git dir; the main working tree is its parent.
	commonRaw, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return "", false
	}
	common := strings.TrimSpace(string(commonRaw))
	if common == "" {
		return "", false
	}
	if !filepath.IsAbs(common) {
		common = filepath.Clean(filepath.Join(gitDir, common))
	}
	return filepath.Dir(common), true
}

// parseGitdir extracts the path from a `.git` file's `gitdir: <path>` line.
func parseGitdir(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "gitdir:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// hasPathSegment reports whether path contains seg as a full path element.
func hasPathSegment(path, seg string) bool {
	for _, p := range strings.Split(filepath.ToSlash(path), "/") {
		if p == seg {
			return true
		}
	}
	return false
}
