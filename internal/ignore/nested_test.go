package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReanchorPattern(t *testing.T) {
	cases := []struct{ dir, in, want string }{
		{"p/q", "x", "p/q/**/x"},                            // unanchored -> any depth below dir
		{"p/q", "/x", "p/q/x"},                              // leading slash -> anchored to dir
		{"p/q", "a/b", "p/q/a/b"},                           // middle slash -> anchored
		{"p/q", "build/", "p/q/**/build/"},                  // unanchored dir-only
		{"p/q", "/build/", "p/q/build/"},                    // anchored dir-only
		{"p/q", "!keep.txt", "!p/q/**/keep.txt"},            // negation preserved
		{"p/q", "!/keep.txt", "!p/q/keep.txt"},              // anchored negation
		{"p/q", "   ", ""},                                  // blank
		{"p/q", "# comment", ""},                            // comment
		{"p/q", "\tspaced  ", "p/q/**/spaced"},              // trims whitespace
		{"a/cm", "cm/labextension", "a/cm/cm/labextension"}, // real bright case
	}
	for _, c := range cases {
		if got := reanchorPattern(c.dir, c.in); got != c.want {
			t.Errorf("reanchorPattern(%q, %q) = %q, want %q", c.dir, c.in, got, c.want)
		}
	}
}

// writeCfg writes a .dex/config.yml enabling nested gitignore + an include so the
// matcher constructs; returns nothing.
func writeCfg(t *testing.T, root string, respectNested bool) {
	t.Helper()
	body := "index:\n  include:\n    - \"**\"\n"
	if respectNested {
		body += "  respect_nested_gitignore: true\n"
	}
	mustWrite(t, filepath.Join(root, ".dex", "config.yml"), body)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// nestedTree lays out a monorepo where apps/pkg carries its own .gitignore. Only
// the nested rules can exclude apps/pkg/labextension (the root-anchored /build/
// default does not reach it) — so it isolates the #74 behavior.
func nestedTree(t *testing.T) string {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "apps", "pkg", ".gitignore"), "/labextension/\n*.gen.ts\n")
	return root
}

func TestNestedGitignoreOptIn(t *testing.T) {
	root := nestedTree(t)
	writeCfg(t, root, true)
	m, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	// Excluded by the nested rules, re-anchored to apps/pkg.
	assertExcluded(t, m, "apps/pkg/labextension", true, true)
	assertExcluded(t, m, "apps/pkg/foo.gen.ts", false, true)      // unanchored, direct child
	assertExcluded(t, m, "apps/pkg/deep/bar.gen.ts", false, true) // unanchored, any depth
	// The re-anchoring must NOT leak to a sibling directory of the same name.
	assertExcluded(t, m, "other/labextension", true, false)
	// A normal source file survives.
	assertExcluded(t, m, "apps/pkg/src/main.ts", false, false)
}

func TestNestedGitignoreDefaultOff(t *testing.T) {
	root := nestedTree(t)
	writeCfg(t, root, false)
	m, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	// With the flag off, the nested .gitignore is ignored — apps/pkg/labextension
	// escapes the root-anchored /build/ default and is NOT excluded. Behavior is
	// byte-for-byte the prior root-file + config model.
	assertExcluded(t, m, "apps/pkg/labextension", true, false)
	assertExcluded(t, m, "apps/pkg/foo.gen.ts", false, false)
}

func assertExcluded(t *testing.T, m *Matcher, rel string, isDir, want bool) {
	t.Helper()
	if got := m.MatchExclude(rel, isDir); got != want {
		t.Errorf("MatchExclude(%q, isDir=%v) = %v, want %v", rel, isDir, got, want)
	}
}
