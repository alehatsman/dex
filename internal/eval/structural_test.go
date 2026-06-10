package eval

import (
	"context"
	"sort"
	"testing"
)

func TestIsProseSubject(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"add graph-proximity lane for structural coupling", true},
		{"fix index drainer idle re-arm on no progress", true},
		{"remove unused helper", true},             // 3 words
		{"short", false},                           // 1 word
		{"two words", false},                       // 2 words
		{"call foo() to initialize store", false},  // contains ()
		{"set config {key: val}", false},           // contains {}
		{"parse []string args correctly", false},   // contains []
		{"feat: add handler for graph lane", true}, // conventional prefix already stripped by cleanSubject
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

func TestImportRefPresent(t *testing.T) {
	goContent := `package foo

import (
	"github.com/org/repo/internal/bar"
	"fmt"
)
`
	tsContent := `import { Foo } from "../bar/foo"
import React from "react"
`
	if !importRefPresent(goContent, "internal/bar") {
		t.Error("should detect Go import of internal/bar")
	}
	if importRefPresent(goContent, "internal/baz") {
		t.Error("should not detect import of internal/baz")
	}
	if !importRefPresent(tsContent, "bar/foo") {
		t.Error("should detect TS import of bar/foo")
	}
	// Content that mentions a dir only in a comment — no import keywords / quotes.
	commentOnly := "// see internal/bar for docs\nfunc main() {}\n"
	if importRefPresent(commentOnly, "internal/bar") {
		t.Error("comment-only reference should not count as import")
	}
}

func TestAnyImportsCross(t *testing.T) {
	dir := t.TempDir()

	// Two files that DO import each other.
	write(t, dir, "pkg/a/a.go", `package a
import "github.com/org/repo/pkg/b"
func A() { b.B() }
`)
	write(t, dir, "pkg/b/b.go", `package b
func B() {}
`)

	if !anyImportsCross(dir, []string{"pkg/a/a.go", "pkg/b/b.go"}) {
		t.Error("expected cross-import detected between a and b")
	}

	// Two files that do NOT import each other.
	write(t, dir, "pkg/c/c.go", "package c\nfunc C() {}\n")
	write(t, dir, "pkg/d/d.go", "package d\nfunc D() {}\n")

	if anyImportsCross(dir, []string{"pkg/c/c.go", "pkg/d/d.go"}) {
		t.Error("unexpected cross-import between c and d")
	}
}

func TestGenerateStructural(t *testing.T) {
	unsetGitEnv(t)
	dir := gitInitRepo(t)

	// Commit 1: two files in different packages, no cross-imports, prose message.
	// → should produce 1 structural query.
	write(t, dir, "pkg/alpha/alpha.go", "package alpha\nfunc Alpha() {}\n")
	write(t, dir, "pkg/beta/beta.go", "package beta\nfunc Beta() {}\n")
	gitCommitAll(t, dir, "feat: add alpha and beta handlers for new pipeline")

	// Commit 2: two files in different packages but one imports the other.
	// → should be excluded (cross-import).
	write(t, dir, "pkg/gamma/gamma.go", "package gamma\nfunc Gamma() {}\n")
	write(t, dir, "pkg/delta/delta.go", `package delta
import "github.com/org/repo/pkg/gamma"
func Delta() { gamma.Gamma() }
`)
	gitCommitAll(t, dir, "refactor: wire gamma into delta service layer")

	// Commit 3: two files in the SAME package (same dir).
	// → should be excluded (not multi-package).
	write(t, dir, "pkg/epsilon/e1.go", "package epsilon\nfunc E1() {}\n")
	write(t, dir, "pkg/epsilon/e2.go", "package epsilon\nfunc E2() {}\n")
	gitCommitAll(t, dir, "feat: add epsilon helpers for encoding layer")

	// Commit 4: single file.
	// → should be excluded (< 2 files).
	write(t, dir, "pkg/zeta/zeta.go", "package zeta\nfunc Zeta() {}\n")
	gitCommitAll(t, dir, "feat: add standalone zeta utility function")

	// Commit 5: prose subject too short.
	// → should be excluded.
	write(t, dir, "pkg/eta/eta.go", "package eta\nfunc Eta() {}\n")
	write(t, dir, "pkg/theta/theta.go", "package theta\nfunc Theta() {}\n")
	gitCommitAll(t, dir, "fix: typo")

	gs, err := GenerateStructural(context.Background(), dir, GenOpts{MaxCommits: 20, MaxFiles: 5})
	if err != nil {
		t.Fatal(err)
	}

	if len(gs.Queries) != 1 {
		t.Fatalf("got %d queries, want 1; queries: %+v", len(gs.Queries), gs.Queries)
	}

	q := gs.Queries[0]
	if q.Query == "" {
		t.Error("query text is empty")
	}
	if len(q.RelevantFiles) != 2 {
		t.Errorf("got %d relevant files, want 2: %v", len(q.RelevantFiles), q.RelevantFiles)
	}
	if !sort.StringsAreSorted(q.RelevantFiles) {
		t.Errorf("relevant files not sorted: %v", q.RelevantFiles)
	}
	if q.Anchor != "" {
		t.Errorf("structural query should have no anchor, got %q", q.Anchor)
	}
	// Verify the two expected files are present.
	wantFiles := []string{"pkg/alpha/alpha.go", "pkg/beta/beta.go"}
	sort.Strings(wantFiles)
	for i, f := range wantFiles {
		if q.RelevantFiles[i] != f {
			t.Errorf("relevant[%d] = %q, want %q", i, q.RelevantFiles[i], f)
		}
	}
}
