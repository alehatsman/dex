package main

import "testing"

// TestBuildDirTreeInvalidation is the #95e acceptance check: touching one file
// re-rolls only its ancestor directories, and no others. It exercises the pure
// tree/composite construction — no store, no chat.
func TestBuildDirTreeInvalidation(t *testing.T) {
	files := map[string]string{
		"a/b/c.go": "h_c",
		"a/b/d.go": "h_d",
		"a/e/f.go": "h_f",
		"g.go":     "h_g",
	}
	before := buildDirTree(files)

	// Every directory node exists with a non-empty composite (each has a file
	// descendant).
	for _, d := range []string{"", "a", "a/b", "a/e"} {
		if before.composite[d] == "" {
			t.Fatalf("dir %q missing composite", d)
		}
	}

	// Mutate exactly one file's content hash.
	mutated := map[string]string{}
	for k, v := range files {
		mutated[k] = v
	}
	mutated["a/b/c.go"] = "h_c_changed"
	after := buildDirTree(mutated)

	// Ancestors of a/b/c.go — and only these — must change.
	ancestor := map[string]bool{"": true, "a": true, "a/b": true}
	for _, d := range before.order {
		changed := before.composite[d] != after.composite[d]
		switch {
		case ancestor[d] && !changed:
			t.Errorf("ancestor %q did not re-roll", d)
		case !ancestor[d] && changed:
			t.Errorf("non-ancestor %q re-rolled (should be stable)", d)
		}
	}

	// The unchanged sibling subtree is byte-for-byte identical.
	if before.composite["a/e"] != after.composite["a/e"] {
		t.Errorf("sibling subtree a/e changed")
	}
}

func TestBuildDirTreeShape(t *testing.T) {
	files := map[string]string{"a/b/c.go": "1", "a/d.go": "2", "e.go": "3"}
	tr := buildDirTree(files)

	// Bottom-up order: deepest first, so a child dir precedes its parent.
	pos := map[string]int{}
	for i, d := range tr.order {
		pos[d] = i
	}
	if !(pos["a/b"] < pos["a"] && pos["a"] < pos[""]) {
		t.Fatalf("order not bottom-up: %v", tr.order)
	}

	// Immediate children only.
	if got := tr.childFiles["a"]; len(got) != 1 || got[0] != "a/d.go" {
		t.Fatalf("a childFiles = %v, want [a/d.go]", got)
	}
	if got := tr.subdirs["a"]; len(got) != 1 || got[0] != "a/b" {
		t.Fatalf("a subdirs = %v, want [a/b]", got)
	}
	if got := tr.childFiles[""]; len(got) != 1 || got[0] != "e.go" {
		t.Fatalf("root childFiles = %v, want [e.go]", got)
	}

	// Empty input → root composite "" (nothing to roll up).
	if buildDirTree(nil).composite[""] != "" {
		t.Fatal("empty tree root composite should be \"\"")
	}
}

func TestDirHelpers(t *testing.T) {
	cases := []struct {
		path      string
		dir, base string
	}{
		{"a/b/c.go", "a/b", "c.go"},
		{"g.go", "", "g.go"},
	}
	for _, c := range cases {
		if got := dirOf(c.path); got != c.dir {
			t.Errorf("dirOf(%q)=%q want %q", c.path, got, c.dir)
		}
		if got := baseName(c.path); got != c.base {
			t.Errorf("baseName(%q)=%q want %q", c.path, got, c.base)
		}
	}
	if dirDepth("") != 0 || dirDepth("a") != 1 || dirDepth("a/b") != 2 {
		t.Errorf("dirDepth wrong: %d %d %d", dirDepth(""), dirDepth("a"), dirDepth("a/b"))
	}
	if dirLabel("") != "(repo root)" || dirLabel("a/b") != "a/b" {
		t.Errorf("dirLabel wrong")
	}
}
