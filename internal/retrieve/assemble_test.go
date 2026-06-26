package retrieve

import (
	"slices"
	"strings"
	"testing"
)

func TestSplitIdentWords(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"(*Store).parseConfig", []string{"store", "parse", "config"}},
		{"prune_older_than", []string{"prune", "older", "than"}},
		{"PruneIndex", []string{"prune", "index"}},
		{"search", []string{"search"}},
		{"", nil},
	}
	for _, c := range cases {
		got := splitIdentWords(c.in)
		if !slices.Equal(got, c.want) {
			t.Errorf("splitIdentWords(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The headline #723 case: a prose question with no code-shaped identifiers
// must still yield coverage keys, or SelectMaxCoverage degrades to natural
// order and assemble inlines arbitrary bodies.
func TestAssembleKeywordsProseIsNotEmpty(t *testing.T) {
	keys := AssembleKeywords(nil, "how does pruning interact with recovery", nil)
	if len(keys) == 0 {
		t.Fatal("prose query produced no coverage keys — submodular selection would degrade to natural order")
	}
	for _, want := range []string{"pruning", "recovery"} {
		if !slices.Contains(keys, want) {
			t.Errorf("keys %v missing content word %q", keys, want)
		}
	}
}

// Multi-concern: one CamelCase token plus prose. Both the identifier stems
// and the prose concepts must become keys so coverage spans every concern.
func TestAssembleKeywordsMultiConcern(t *testing.T) {
	keys := AssembleKeywords([]string{"CCR"}, "how does pruning interact with CCR recovery", nil)
	for _, want := range []string{"ccr", "pruning", "recovery"} {
		if !slices.Contains(keys, want) {
			t.Errorf("keys %v missing %q", keys, want)
		}
	}
}

func TestAssembleKeywordsAnchorStems(t *testing.T) {
	keys := AssembleKeywords(nil, "where does the prune step run", []string{"(*Store).PruneIndex"})
	for _, want := range []string{"prune", "index"} {
		if !slices.Contains(keys, want) {
			t.Errorf("anchor stems missing %q in %v", want, keys)
		}
	}
}

func TestAssembleKeywordsHygiene(t *testing.T) {
	keys := AssembleKeywords([]string{"(*Store).Search"}, "is it ok to do so", nil)
	seen := map[string]struct{}{}
	for _, k := range keys {
		if len(k) < 3 {
			t.Errorf("key %q shorter than 3 bytes", k)
		}
		if k != strings.ToLower(k) {
			t.Errorf("key %q not lowercased", k)
		}
		if _, dup := seen[k]; dup {
			t.Errorf("duplicate key %q", k)
		}
		seen[k] = struct{}{}
	}
	// Qualified identifier split into clean stems.
	for _, want := range []string{"store", "search"} {
		if !slices.Contains(keys, want) {
			t.Errorf("keys %v missing identifier stem %q", keys, want)
		}
	}
}
