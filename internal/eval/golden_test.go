package eval

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

// TestGenerateSubdirRelativePaths is the #285 regression guard: when the gen
// root is a subdirectory of a larger repo (the corpus index_subdir case), the
// generated relevant_files must be relative to that subdir (so they match how
// the index records paths), and files committed outside the subdir must be
// excluded entirely.
func TestGenerateSubdirRelativePaths(t *testing.T) {
	unsetGitEnv(t)
	dir := gitInitRepo(t)

	// A commit touching two files inside the target subdir and one outside it.
	write(t, dir, "pkg/sub/app.go", "package sub\n\nfunc App() int { return 1 }\n")
	write(t, dir, "pkg/sub/util.go", "package sub\n\nfunc Util() int { return App() }\n")
	write(t, dir, "other/outside.go", "package other\n\nfunc Outside() {}\n")
	gitCommitAll(t, dir, "add app and util helpers")

	// A commit touching ONLY a file outside the subdir — must yield no query
	// from the subdir's perspective.
	write(t, dir, "other/lonely.go", "package other\n\nfunc Lonely() {}\n")
	gitCommitAll(t, dir, "add lonely outside helper")

	sub := filepath.Join(dir, "pkg", "sub")
	gs, err := Generate(context.Background(), sub, GenOpts{MaxCommits: 10, MaxFiles: 5})
	if err != nil {
		t.Fatal(err)
	}

	if len(gs.Queries) != 1 {
		t.Fatalf("got %d queries, want 1 (only the commit touching the subdir); %+v", len(gs.Queries), gs.Queries)
	}
	got := gs.Queries[0].RelevantFiles
	want := []string{"app.go", "util.go"} // subdir-relative, sorted, outside.go excluded
	if !reflect.DeepEqual(got, want) {
		t.Errorf("relevant_files = %v, want %v (subdir-relative, outside files excluded)", got, want)
	}
}

// TestGenerateRepoRootUnchanged confirms the --relative flag is a no-op when the
// root IS the repo root: paths stay repo-root-relative.
func TestGenerateRepoRootUnchanged(t *testing.T) {
	unsetGitEnv(t)
	dir := gitInitRepo(t)
	write(t, dir, "pkg/sub/app.go", "package sub\n\nfunc App() int { return 1 }\n")
	gitCommitAll(t, dir, "add app helper in a nested package")

	gs, err := Generate(context.Background(), dir, GenOpts{MaxCommits: 10, MaxFiles: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(gs.Queries) != 1 {
		t.Fatalf("got %d queries, want 1; %+v", len(gs.Queries), gs.Queries)
	}
	got := gs.Queries[0].RelevantFiles
	want := []string{"pkg/sub/app.go"} // repo-root-relative, unchanged
	if !reflect.DeepEqual(got, want) {
		t.Errorf("relevant_files = %v, want %v (repo-root-relative)", got, want)
	}
}
