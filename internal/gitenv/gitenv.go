// Package gitenv centralizes the one git-environment guard the whole codebase
// depends on: stripping git's repo-discovery variables from a process
// environment.
//
// Git consults GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE (and friends) for
// repository discovery, and they OVERRIDE cmd.Dir and the `-C <dir>` flag. When
// dex runs git as a subprocess and relies on cmd.Dir/`-C` to target a specific
// repo, an inherited GIT_DIR silently redirects the command to the wrong
// repository. The canonical failure: the test suite runs under a pre-commit /
// pre-push hook that exports a linked-worktree GIT_DIR; a child `git init` then
// reinitializes the SHARED repo as bare (core.bare=true), wiping every
// worktree's index. See issues #341 and #680.
//
// This scrub was previously copy-pasted across five packages with two different
// (and divergent) variable sets — some copies missed GIT_COMMON_DIR and the
// other extended vars. A single source of truth removes that drift risk: any
// new git-touching site calls Hermetic instead of hand-rolling a partial scrub.
package gitenv

import (
	"os"
	"strings"
)

// leaky lists the git environment variables that redirect repository discovery
// away from cmd.Dir / `-C <dir>`. Stripping all of them — not just the common
// trio — is what keeps a subprocess pinned to its intended directory.
var leaky = map[string]bool{
	"GIT_DIR":              true,
	"GIT_WORK_TREE":        true,
	"GIT_INDEX_FILE":       true,
	"GIT_COMMON_DIR":       true,
	"GIT_OBJECT_DIRECTORY": true,
	"GIT_NAMESPACE":        true,
	"GIT_PREFIX":           true,
}

// Hermetic returns environ with git's repo-discovery variables removed, so a
// child git command honors cmd.Dir / `-C <dir>` regardless of what the parent
// process inherited. It is a pure function of its input (no os access), which
// makes it trivially testable; pass os.Environ() — or use Current — at call
// sites.
func Hermetic(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if leaky[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Current is Hermetic(os.Environ()) — the common case for production callers
// that just want the live environment scrubbed before handing it to exec.
func Current() []string {
	return Hermetic(os.Environ())
}
