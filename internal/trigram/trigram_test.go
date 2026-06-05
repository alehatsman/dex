package trigram

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWordTrigrams(t *testing.T) {
	tests := []struct {
		query    string
		wantSome bool // at least one trigram expected
	}{
		{"searchGrep", true},
		{".*", false},        // pure regex — no word runs ≥ 3
		{"ab", false},        // too short
		{"abc", true},
		{"foo.*bar", true},   // has word runs on both sides
		{"[A-Z]", false},     // metacharacters only
		{"_ab", true},        // "_ab" is a 3-char word run → one trigram
		{"_abc", true},
	}
	for _, tt := range tests {
		got := wordTrigrams(tt.query)
		if tt.wantSome && len(got) == 0 {
			t.Errorf("wordTrigrams(%q): expected trigrams, got none", tt.query)
		}
		if !tt.wantSome && len(got) != 0 {
			t.Errorf("wordTrigrams(%q): expected no trigrams, got %v", tt.query, got)
		}
	}
}

func TestBuildAndNarrow(t *testing.T) {
	dir := t.TempDir()

	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	a := write("a.go", "package main\nfunc searchGrep(pattern string) {}")
	b := write("b.go", "package main\nfunc doSomethingElse() {}")
	c := write("c.go", "package main\n// searchGrep reference here")

	idx := Build([]string{a, b, c})

	t.Run("narrows to matching files", func(t *testing.T) {
		candidates, ok := idx.Narrow("searchGrep")
		if !ok {
			t.Fatal("Narrow returned ok=false for word query")
		}
		if len(candidates) == 0 {
			t.Fatal("expected candidates, got none")
		}
		// b.go must not be in candidates
		for _, p := range candidates {
			if p == b {
				t.Errorf("b.go should not match 'searchGrep'")
			}
		}
		// a.go and c.go must be present
		found := map[string]bool{}
		for _, p := range candidates {
			found[p] = true
		}
		if !found[a] {
			t.Errorf("a.go missing from candidates")
		}
		if !found[c] {
			t.Errorf("c.go missing from candidates")
		}
	})

	t.Run("regex pattern falls back", func(t *testing.T) {
		// Pure metachar pattern — no word runs of length ≥ 3.
		_, ok := idx.Narrow(".*")
		if ok {
			t.Error("expected ok=false for pure-regex pattern with no word trigrams")
		}
	})

	t.Run("impossible query returns empty", func(t *testing.T) {
		candidates, ok := idx.Narrow("xyzXYZnothere")
		if !ok {
			t.Fatal("expected ok=true")
		}
		if len(candidates) != 0 {
			t.Errorf("expected no candidates for absent token, got %v", candidates)
		}
	})

	t.Run("no false negatives", func(t *testing.T) {
		// Every file whose content contains a literal word must be in candidates.
		candidates, ok := idx.Narrow("doSomethingElse")
		if !ok {
			t.Fatal("expected ok=true")
		}
		found := map[string]bool{}
		for _, p := range candidates {
			found[p] = true
		}
		if !found[b] {
			t.Errorf("b.go must be in candidates for 'doSomethingElse'")
		}
	})
}

func TestIntersectSorted(t *testing.T) {
	a := []uint32{1, 3, 5, 7}
	b := []uint32{3, 5, 6, 9}
	got := intersectSorted(a, b)
	want := []uint32{3, 5}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestStale(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	if err := os.WriteFile(p, []byte("hello world trigram"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := Build([]string{p})
	if idx.Stale() {
		t.Error("fresh index reported stale immediately")
	}
}
