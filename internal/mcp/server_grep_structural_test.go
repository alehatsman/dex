package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSearchGrepStructuralMatchesByShape covers the issue's own success bar:
// a structural query returns a match a text/regex pattern would false-positive
// or false-negative on, for one representative language.
func TestSearchGrepStructuralMatchesByShape(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	projDir := t.TempDir()

	// A regex for "def foo(" would match the commented-out decoy line below,
	// which no call to foo() should. Structural query targets a real call node.
	body := "# def foo(): still shows up under a naive text search for 'foo('\n" +
		"def bar():\n" +
		"    foo(1, 2)\n" +
		"    baz(3)\n"
	if err := os.WriteFile(filepath.Join(projDir, "m.py"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Query:       `(call function: (identifier) @fn (#eq? @fn "foo"))`,
		Lang:        "python",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %s)", out.Status, out.Hint)
	}
	if out.Total != 1 {
		t.Fatalf("total = %d, want 1 (the comment line must not match)", out.Total)
	}
	if out.Matches[0].Line != 3 {
		t.Errorf("match line = %d, want 3 (the real foo(1, 2) call)", out.Matches[0].Line)
	}
}

func TestSearchGrepStructuralRequiresLang(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	projDir := t.TempDir()

	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Query:       `(call) @c`,
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "error" {
		t.Fatalf("status = %q, want error", out.Status)
	}
	if !strings.Contains(out.Hint, "lang is required") {
		t.Errorf("hint = %q, want it to explain lang is required", out.Hint)
	}
}

func TestSearchGrepStructuralUnknownLang(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	projDir := t.TempDir()

	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Query:       `(call) @c`,
		Lang:        "cobol",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "error" {
		t.Fatalf("status = %q, want error", out.Status)
	}
	if !strings.Contains(out.Hint, "unsupported lang") {
		t.Errorf("hint = %q, want it to name the lang as unsupported", out.Hint)
	}
}

func TestSearchGrepStructuralInvalidQuerySyntax(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	projDir := t.TempDir()

	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Query:       `(call not valid scm (((`,
		Lang:        "python",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "error" {
		t.Fatalf("status = %q, want error", out.Status)
	}
	if !strings.Contains(out.Hint, "invalid query") {
		t.Errorf("hint = %q, want it to say invalid query", out.Hint)
	}
}

// TestSearchGrepStructuralRejectsUnsupportedPredicate covers the spec's
// explicit choice: smacker/go-tree-sitter's FilterPredicates never enforces
// is?/is-not?/set! (they compile but silently no-op), so dex rejects a query
// using one of them at compile time instead of accepting a filter that won't
// actually apply.
func TestSearchGrepStructuralRejectsUnsupportedPredicate(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	projDir := t.TempDir()

	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Query:       `(call function: (identifier) @fn (#is? @fn "local"))`,
		Lang:        "python",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "error" {
		t.Fatalf("status = %q, want error", out.Status)
	}
	if !strings.Contains(out.Hint, "is?") {
		t.Errorf("hint = %q, want it to name the unsupported predicate", out.Hint)
	}
}

func TestSearchGrepStructuralAndPatternMutuallyExclusive(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	projDir := t.TempDir()

	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Pattern:     "foo",
		Query:       `(call) @c`,
		Lang:        "python",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "error" {
		t.Fatalf("status = %q, want error", out.Status)
	}
	if !strings.Contains(out.Hint, "mutually exclusive") {
		t.Errorf("hint = %q, want it to say mutually exclusive", out.Hint)
	}
}

// TestSearchGrepStructuralPerLanguage proves at least one shape-match per
// language the facet advertises, since #238's success bar was set per-language.
func TestSearchGrepStructuralPerLanguage(t *testing.T) {
	cases := []struct {
		lang, file, body, query string
		wantLine                int
	}{
		{
			lang: "javascript", file: "m.js",
			body:     "// foo(1) in a comment\nfunction bar() {\n  foo(1, 2);\n}\n",
			query:    `(call_expression function: (identifier) @fn (#eq? @fn "foo"))`,
			wantLine: 3,
		},
		{
			lang: "typescript", file: "m.ts",
			body:     "// foo(1) in a comment\nfunction bar(): void {\n  foo(1, 2);\n}\n",
			query:    `(call_expression function: (identifier) @fn (#eq? @fn "foo"))`,
			wantLine: 3,
		},
		{
			lang: "tsx", file: "m.tsx",
			body:     "// foo(1) in a comment\nfunction Bar() {\n  foo(1, 2);\n  return <div>ok</div>;\n}\n",
			query:    `(call_expression function: (identifier) @fn (#eq? @fn "foo"))`,
			wantLine: 3,
		},
		{
			lang: "rust", file: "m.rs",
			body:     "// foo(1) in a comment\nfn bar() {\n    foo(1, 2);\n}\n",
			query:    `(call_expression function: (identifier) @fn (#eq? @fn "foo"))`,
			wantLine: 3,
		},
		{
			lang: "java", file: "M.java",
			body:     "class M {\n  // foo(1) in a comment\n  void bar() {\n    foo(1, 2);\n  }\n}\n",
			query:    `(method_invocation name: (identifier) @fn (#eq? @fn "foo"))`,
			wantLine: 4,
		},
	}

	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			srv := fakeEmbed(t, 16)
			defer srv.Close()
			s := newServer(srv.URL, t.TempDir())
			projDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(projDir, tc.file), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}

			_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
				Query:       tc.query,
				Lang:        tc.lang,
				ProjectRoot: projDir,
			})
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "ok" {
				t.Fatalf("status = %q, want ok (hint: %s)", out.Status, out.Hint)
			}
			if out.Total != 1 || out.Matches[0].Line != tc.wantLine {
				t.Fatalf("matches = %+v, want exactly one at line %d", out.Matches, tc.wantLine)
			}
		})
	}
}
