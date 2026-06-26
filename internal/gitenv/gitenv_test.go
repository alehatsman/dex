package gitenv

import (
	"slices"
	"strings"
	"testing"
)

// TestHermeticStripsRepoRedirectingVars is the regression guard for #341/#680:
// every variable that can redirect git's repo discovery away from cmd.Dir must
// be dropped, while unrelated vars (including non-discovery GIT_* like
// GIT_AUTHOR_NAME) survive.
func TestHermeticStripsRepoRedirectingVars(t *testing.T) {
	in := []string{
		// the dangerous inherited shape a linked-worktree hook exports
		"GIT_DIR=/repo/.git/worktrees/wt",
		"GIT_WORK_TREE=/repo",
		"GIT_INDEX_FILE=/repo/.git/worktrees/wt/index",
		"GIT_COMMON_DIR=/repo/.git",
		"GIT_OBJECT_DIRECTORY=/repo/.git/objects",
		"GIT_NAMESPACE=ns",
		"GIT_PREFIX=sub/",
		// must be preserved
		"PATH=/usr/bin",
		"HOME=/home/u",
		"GIT_AUTHOR_NAME=Aleh", // GIT_* but not a discovery var
	}
	got := Hermetic(in)

	for _, banned := range []string{
		"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=",
		"GIT_COMMON_DIR=", "GIT_OBJECT_DIRECTORY=", "GIT_NAMESPACE=", "GIT_PREFIX=",
	} {
		if slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, banned) }) {
			t.Errorf("Hermetic leaked %q — an inherited GIT_DIR can flip a shared repo's core.bare (#341/#680)", banned)
		}
	}
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/u", "GIT_AUTHOR_NAME=Aleh"} {
		if !slices.Contains(got, want) {
			t.Errorf("Hermetic dropped %q — only repo-discovery vars should be stripped", want)
		}
	}
}

// TestHermeticIsPure confirms the input slice is not mutated (callers commonly
// pass os.Environ()).
func TestHermeticIsPure(t *testing.T) {
	in := []string{"GIT_DIR=/x", "PATH=/usr/bin"}
	orig := slices.Clone(in)
	_ = Hermetic(in)
	if !slices.Equal(in, orig) {
		t.Errorf("Hermetic mutated its input: %v != %v", in, orig)
	}
}

// TestHermeticHandlesMalformedEntries: an entry without '=' is treated as a
// bare name and kept unless it exactly names a leaky var.
func TestHermeticHandlesMalformedEntries(t *testing.T) {
	got := Hermetic([]string{"NOEQUALS", "GIT_DIR", "PATH=/bin"})
	if slices.Contains(got, "GIT_DIR") {
		t.Error("Hermetic kept a bare GIT_DIR entry")
	}
	if !slices.Contains(got, "NOEQUALS") {
		t.Error("Hermetic dropped an unrelated malformed entry")
	}
	if !slices.Contains(got, "PATH=/bin") {
		t.Error("Hermetic dropped PATH")
	}
}
