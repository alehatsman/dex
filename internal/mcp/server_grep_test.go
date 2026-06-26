package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSearchGrepNotFound asserts that an explicit but non-existent subdir
// path fails loud (status "not-found") instead of silently walking the whole
// project root and returning a misleading "no-matches" (issue #73).
func TestSearchGrepNotFound(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())

	projDir := t.TempDir()
	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Pattern:     "foo",
		Path:        "does/not/exist",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "not-found" {
		t.Errorf("status = %q, want not-found", out.Status)
	}
}

// TestSearchGrepFilePath asserts that an explicit *file* path is accepted and
// scoped to exactly that file, rather than rejected with a lying "does not
// exist" message — grep used to require a directory (issue #534).
func TestSearchGrepFilePath(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())

	projDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projDir, "target.go"), []byte("package p\n// needle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "other.go"), []byte("package p\n// needle here too\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Pattern:     "needle",
		Path:        "target.go",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok", out.Status)
	}
	if out.Total != 1 {
		t.Fatalf("total = %d, want 1 (scan scoped to the single file)", out.Total)
	}
	if len(out.Matches) != 1 || out.Matches[0].Path != "target.go" {
		t.Errorf("matches = %+v, want one in target.go", out.Matches)
	}
}

// TestSearchGrepFilePathExtExcluded asserts that a single-file scope with an
// ext filter that excludes the file yields no-matches, never a whole-repo walk.
func TestSearchGrepFilePathExtExcluded(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())

	projDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projDir, "target.go"), []byte("// needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Pattern:     "needle",
		Path:        "target.go",
		Ext:         "py",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no-matches" {
		t.Errorf("status = %q, want no-matches", out.Status)
	}
}

// TestSearchGrepContext covers #662: context lines before/after each match,
// clamped at file start/EOF, with the trailing-newline artifact dropped.
func TestSearchGrepContext(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	projDir := t.TempDir()
	body := "L1 alpha\nL2 beta\nL3 NEEDLE mid\nL4 delta\nL5 NEEDLE last\n"
	if err := os.WriteFile(filepath.Join(projDir, "f.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	grep := func(ctxN int) []GrepMatch {
		_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
			Pattern: "NEEDLE", Path: "f.go", Context: ctxN, ProjectRoot: projDir,
		})
		if err != nil || out.Status != "ok" {
			t.Fatalf("status=%q err=%v", out.Status, err)
		}
		return out.Matches
	}

	// context=0 → no before/after (default behaviour unchanged).
	for _, m := range grep(0) {
		if len(m.Before) != 0 || len(m.After) != 0 {
			t.Errorf("context=0 should carry no context, got %+v", m)
		}
	}

	ms := grep(2)
	if len(ms) != 2 {
		t.Fatalf("want 2 matches, got %d", len(ms))
	}
	// Match 1 at line 3: Before = L1,L2; After = L4,L5.
	mid := ms[0]
	if got := strings.Join(mid.Before, "|"); got != "L1 alpha|L2 beta" {
		t.Errorf("mid Before = %q, want L1|L2", got)
	}
	if got := strings.Join(mid.After, "|"); got != "L4 delta|L5 NEEDLE last" {
		t.Errorf("mid After = %q, want L4|L5", got)
	}
	// Match 2 at line 5 (last content line): After clamps to empty (the trailing
	// "" from the final newline is dropped, not shown as a phantom blank line).
	last := ms[1]
	if got := strings.Join(last.Before, "|"); got != "L3 NEEDLE mid|L4 delta" {
		t.Errorf("last Before = %q, want L3|L4", got)
	}
	if len(last.After) != 0 {
		t.Errorf("last After should be empty at EOF, got %+v", last.After)
	}
}

// TestSearchGrepFixed covers #663: fixed=true matches the pattern literally,
// so regex metacharacters are treated as plain text.
func TestSearchGrepFixed(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	projDir := t.TempDir()
	// "a.b" appears literally; "axb" would match the regex a.b but not the literal.
	body := "match a.b here\ndecoy axb here\n"
	if err := os.WriteFile(filepath.Join(projDir, "f.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	grep := func(fixed bool) SearchGrepOutput {
		_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
			Pattern: "a.b", Path: "f.txt", Fixed: fixed, ProjectRoot: projDir,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Regex mode: "a.b" matches both "a.b" and "axb".
	if reOut := grep(false); reOut.Total != 2 {
		t.Errorf("regex a.b should match both lines, got %d", reOut.Total)
	}
	// Fixed mode: only the literal "a.b" line.
	fx := grep(true)
	if fx.Total != 1 {
		t.Fatalf("fixed a.b should match only the literal line, got %d: %+v", fx.Total, fx.Matches)
	}
	if !strings.Contains(fx.Matches[0].Content, "a.b") {
		t.Errorf("fixed match should be the a.b line, got %q", fx.Matches[0].Content)
	}

	// A pattern that is invalid regex but a fine literal must work in fixed mode.
	_, badRe, _ := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Pattern: "f(x", Path: "f.txt", Fixed: false, ProjectRoot: projDir,
	})
	if badRe.Status != "error" {
		t.Errorf("unbalanced paren as regex should error, got %q", badRe.Status)
	}
	_, okFixed, _ := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Pattern: "f(x", Path: "f.txt", Fixed: true, ProjectRoot: projDir,
	})
	if okFixed.Status == "error" {
		t.Errorf("unbalanced paren as a literal should be valid, got error: %s", okFixed.Hint)
	}
}
