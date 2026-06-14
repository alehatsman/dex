package mcp

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/proj"
)

func TestEscapesRoot(t *testing.T) {
	root := filepath.Clean("/home/u/proj")
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(root, "a/b.go"), false},
		{root, false},
		{filepath.Join(root, "..foo"), false},        // sibling-name false positive guard
		{filepath.Join(root, "deep/../x.go"), false}, // normalises back inside
		{filepath.Clean(root + "/.."), true},
		{filepath.Clean(root + "/../etc/shadow"), true},
		{"/etc/passwd", true},
	}
	for _, c := range cases {
		if got := escapesRoot(root, c.path); got != c.want {
			t.Errorf("escapesRoot(%q, %q) = %v, want %v", root, c.path, got, c.want)
		}
	}
}

// TestSummarizeReadFilePathEscapeOrder locks issue #508: a relative path that
// escapes the project root must report "outside project root" regardless of
// whether the resolved path exists. The bug was that EvalSymlinks (existence)
// ran before the containment check, so a non-existent escaping path leaked the
// resolved path through a misleading "file does not exist" message.
func TestSummarizeReadFilePathEscapeOrder(t *testing.T) {
	root := t.TempDir()
	p := &proj.Project{Root: root}
	s := &Server{}

	t.Run("non-existent escaping path is outside-root, not missing", func(t *testing.T) {
		_, _, _, _, out, done := s.summarizeReadFile(p, "../../../etc/definitely-not-here-508")
		if !done {
			t.Fatal("expected done=true for an escaping path")
		}
		if !strings.Contains(out.Hint, "outside project root") {
			t.Errorf("hint = %q, want it to mention 'outside project root'", out.Hint)
		}
		if strings.Contains(out.Hint, "file does not exist") {
			t.Errorf("hint = %q leaked a 'file does not exist' message for an escaping path", out.Hint)
		}
	})

	t.Run("absolute escaping path is outside-root", func(t *testing.T) {
		_, _, _, _, out, done := s.summarizeReadFile(p, "/etc/definitely-not-here-508")
		if !done || !strings.Contains(out.Hint, "outside project root") {
			t.Errorf("done=%v hint=%q, want outside-project-root", done, out.Hint)
		}
	})

	t.Run("missing in-root file still reports file does not exist", func(t *testing.T) {
		_, _, _, _, out, done := s.summarizeReadFile(p, "subdir/missing.go")
		if !done {
			t.Fatal("expected done=true for a missing in-root file")
		}
		if !strings.Contains(out.Hint, "file does not exist") {
			t.Errorf("hint = %q, want 'file does not exist' for a missing in-root file", out.Hint)
		}
	})
}
