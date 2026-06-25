package eval

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestValidateGolden(t *testing.T) {
	base := GoldenSet{
		Queries: []GoldenQuery{
			{ID: "q1", Query: "open store connection", RelevantFiles: []string{"store.go"}},
			{ID: "q2", Query: "parse config file", RelevantFiles: []string{"config.go", "config_test.go"}},
		},
	}

	t.Run("all valid returns unchanged", func(t *testing.T) {
		got, issues := ValidateGolden(base)
		if len(issues) != 0 {
			t.Fatalf("want no issues, got %v", issues)
		}
		if len(got.Queries) != 2 {
			t.Fatalf("want 2 queries, got %d", len(got.Queries))
		}
	})

	t.Run("empty ID skipped", func(t *testing.T) {
		gs := GoldenSet{Queries: []GoldenQuery{
			{ID: "", Query: "something useful", RelevantFiles: []string{"a.go"}},
			base.Queries[0],
		}}
		got, issues := ValidateGolden(gs)
		if len(got.Queries) != 1 || got.Queries[0].ID != "q1" {
			t.Fatalf("expected q1 only, got %+v", got.Queries)
		}
		if len(issues) != 1 {
			t.Fatalf("want 1 issue, got %v", issues)
		}
	})

	t.Run("duplicate ID drops second occurrence", func(t *testing.T) {
		gs := GoldenSet{Queries: []GoldenQuery{
			base.Queries[0], // q1
			{ID: "q1", Query: "different query text", RelevantFiles: []string{"other.go"}},
			base.Queries[1], // q2
		}}
		got, issues := ValidateGolden(gs)
		if len(got.Queries) != 2 {
			t.Fatalf("want 2 queries, got %d", len(got.Queries))
		}
		if got.Queries[0].Query != base.Queries[0].Query {
			t.Fatalf("first q1 not retained")
		}
		if len(issues) != 1 {
			t.Fatalf("want 1 issue, got %v", issues)
		}
	})

	t.Run("empty query text skipped", func(t *testing.T) {
		gs := GoldenSet{Queries: []GoldenQuery{
			{ID: "q3", Query: "", RelevantFiles: []string{"a.go"}},
			base.Queries[0],
		}}
		got, issues := ValidateGolden(gs)
		if len(got.Queries) != 1 {
			t.Fatalf("want 1 query, got %d", len(got.Queries))
		}
		if len(issues) != 1 {
			t.Fatalf("want 1 issue, got %v", issues)
		}
	})

	t.Run("empty relevant_files skipped", func(t *testing.T) {
		gs := GoldenSet{Queries: []GoldenQuery{
			{ID: "q4", Query: "do something important", RelevantFiles: nil},
			base.Queries[0],
		}}
		got, issues := ValidateGolden(gs)
		if len(got.Queries) != 1 {
			t.Fatalf("want 1 query, got %d", len(got.Queries))
		}
		if len(issues) != 1 {
			t.Fatalf("want 1 issue, got %v", issues)
		}
	})

	t.Run("anchor in relevant_files removed", func(t *testing.T) {
		gs := GoldenSet{Queries: []GoldenQuery{
			{ID: "q5", Query: "blast radius from anchor.go", Anchor: "anchor.go", RelevantFiles: []string{"anchor.go", "dep.go", "other.go"}},
		}}
		got, issues := ValidateGolden(gs)
		if len(got.Queries) != 1 {
			t.Fatalf("want 1 query, got %d", len(got.Queries))
		}
		for _, f := range got.Queries[0].RelevantFiles {
			if f == "anchor.go" {
				t.Fatalf("anchor.go should have been removed from relevant_files")
			}
		}
		if len(issues) != 1 {
			t.Fatalf("want 1 issue (anchor removed), got %v", issues)
		}
	})

	t.Run("anchor is only relevant_file skips query", func(t *testing.T) {
		gs := GoldenSet{Queries: []GoldenQuery{
			{ID: "q6", Query: "blast radius single file", Anchor: "only.go", RelevantFiles: []string{"only.go"}},
		}}
		got, issues := ValidateGolden(gs)
		if len(got.Queries) != 0 {
			t.Fatalf("want 0 queries (only relevant file was anchor), got %d", len(got.Queries))
		}
		if len(issues) != 2 { // one for removal, one for skip
			t.Fatalf("want 2 issues (anchor removed + no relevant_files), got %v", issues)
		}
	})
}

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
