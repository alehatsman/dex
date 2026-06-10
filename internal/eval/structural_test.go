package eval

import (
	"context"
	"sort"
	"strings"
	"testing"
)

func TestIsProseSubject(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"add graph-proximity lane for structural coupling", true},
		{"fix index drainer idle re-arm on no progress", true},
		{"remove unused helper", true},            // 3 words
		{"short", false},                          // 1 word
		{"two words", false},                      // 2 words
		{"call foo() to initialize store", false}, // contains ()
		{"set config {key: val}", false},          // contains {}
		{"parse []string args correctly", false},  // contains []
	}
	for _, tc := range cases {
		got := isProseSubject(tc.s)
		if got != tc.want {
			t.Errorf("isProseSubject(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestDistinctDirs(t *testing.T) {
	cases := []struct {
		files []string
		want  int
	}{
		{[]string{"a/b.go", "a/c.go"}, 1},
		{[]string{"a/b.go", "b/c.go"}, 2},
		{[]string{"a/b.go", "b/c.go", "b/d.go"}, 2},
		{[]string{"a/b.go", "b/c.go", "c/d.go"}, 3},
	}
	for _, tc := range cases {
		got := distinctDirs(tc.files)
		if got != tc.want {
			t.Errorf("distinctDirs(%v) = %d, want %d", tc.files, got, tc.want)
		}
	}
}

func TestHasStructuralTarget(t *testing.T) {
	// "service" names service/server.go; "metrics" does not appear → structural target exists.
	if !hasStructuralTarget("add prometheus instrumentation to service layer", []string{"service/server.go", "metrics/counter.go"}) {
		t.Error("expected structural target (metrics not in subject)")
	}
	// Both tokens appear in subject → no structural target.
	if hasStructuralTarget("wire store and index together for fast lookup", []string{"store/store.go", "index/index.go"}) {
		t.Error("expected no structural target (both named)")
	}
	// Short tokens (≤3 chars) skipped: "mcp" in subject won't name mcp/server.go.
	// "store" len=5 not in subject → both files are structural targets → true.
	if !hasStructuralTarget("improve mcp context routing performance", []string{"mcp/server.go", "store/store.go"}) {
		t.Error("expected structural target (mcp skipped as short, store not in subject)")
	}
}

func TestPathTokens(t *testing.T) {
	got := pathTokens("internal/store/store.go")
	want := []string{"internal", "store", "store"}
	if len(got) != len(want) {
		t.Fatalf("pathTokens = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pathTokens[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGenerateStructural(t *testing.T) {
	unsetGitEnv(t)
	dir := gitInitRepo(t)

	// Commit 1: cross-package, subject names only one package (service) not
	// the other (metrics) → structural target present → 1 query expected.
	write(t, dir, "service/server.go", "package service\nfunc Serve() {}\n")
	write(t, dir, "metrics/counter.go", "package metrics\nfunc Count() {}\n")
	gitCommitAll(t, dir, "add prometheus instrumentation to service layer")

	// Commit 2: cross-package with import coupling between files (gamma imports
	// delta). Under the new design this is INCLUDED because import edges are
	// the graph structure we want to measure — but subject names both packages
	// ("gamma" and "delta") → excluded by mixed-lexicality filter.
	write(t, dir, "gamma/gamma.go", "package gamma\nfunc Gamma() {}\n")
	write(t, dir, "delta/delta.go", "package delta\nimport \"github.com/org/repo/gamma\"\nfunc Delta() { gamma.Gamma() }\n")
	gitCommitAll(t, dir, "wire gamma into delta service for processing")
	// "gamma" (5) and "delta" (5) both appear → no structural target → excluded.

	// Commit 3: same directory → excluded (not cross-package).
	write(t, dir, "epsilon/e1.go", "package epsilon\nfunc E1() {}\n")
	write(t, dir, "epsilon/e2.go", "package epsilon\nfunc E2() {}\n")
	gitCommitAll(t, dir, "add epsilon helpers for encoding pipeline layer")

	// Commit 4: single file → excluded (< 2 files).
	write(t, dir, "zeta/zeta.go", "package zeta\nfunc Zeta() {}\n")
	gitCommitAll(t, dir, "add standalone zeta utility function helper")

	// Commit 5: subject too short → excluded.
	write(t, dir, "eta/eta.go", "package eta\nfunc Eta() {}\n")
	write(t, dir, "theta/theta.go", "package theta\nfunc Theta() {}\n")
	gitCommitAll(t, dir, "fix: typo")

	// Commit 6: cross-package, subject names neither file (short tokens only:
	// "eta"=3, "theta"=5). "theta" appears in subject → theta is named,
	// eta is a structural target → included.
	write(t, dir, "eta/eta.go", "package eta\nfunc Eta2() {}\n")
	write(t, dir, "theta/theta.go", "package theta\nfunc Theta2() {}\n")
	gitCommitAll(t, dir, "add theta listener for distributed coordination")
	// "theta" (5) in subject → theta/theta.go named; "eta" (3) skipped → eta/eta.go is structural target.

	gs, err := GenerateStructural(context.Background(), dir, GenOpts{MaxCommits: 20, MaxFiles: 5})
	if err != nil {
		t.Fatal(err)
	}

	// Commits 1 and 6 should produce queries; 2, 3, 4, 5 should not.
	if len(gs.Queries) != 2 {
		for _, q := range gs.Queries {
			t.Logf("  %s: %q → %v", q.ID, q.Query, q.RelevantFiles)
		}
		t.Fatalf("got %d queries, want 2", len(gs.Queries))
	}

	for _, q := range gs.Queries {
		if q.Query == "" {
			t.Errorf("empty query text in %s", q.ID)
		}
		if len(q.RelevantFiles) < 2 {
			t.Errorf("query %s: got %d relevant files, want ≥2", q.ID, len(q.RelevantFiles))
		}
		if !sort.StringsAreSorted(q.RelevantFiles) {
			t.Errorf("relevant files not sorted: %v", q.RelevantFiles)
		}
		if q.Anchor != "" {
			t.Errorf("structural query should have no anchor, got %q", q.Anchor)
		}
		// No same-dir commits should leak through.
		for _, f := range q.RelevantFiles {
			if strings.Contains(f, "epsilon/") {
				t.Errorf("same-dir commit leaked into structural set: %v", q.RelevantFiles)
			}
		}
	}
}
