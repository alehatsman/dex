package compress

import (
	"slices"
	"strings"
	"testing"
)

const anchorSample = `package store

func BuildSymbolMap(path string) SymbolMap {
	data := proj.ReadFile("internal/store/store.go")
	loc := proj.Locate(path, "internal/store/store.go:42")
	loc = proj.Locate(path, loc)
	loc = proj.Locate(path, loc)
	loc = proj.Locate(path, loc)
	return SymbolMap{path: data, loc: loc}
}
`

func TestExtractAnchors(t *testing.T) {
	a := ExtractAnchors(anchorSample)
	want := []string{
		"internal/store/store.go",    // path
		"internal/store/store.go:42", // path + line number
		"proj.ReadFile",              // qualified identifier
		"proj.Locate",                // qualified identifier
		"BuildSymbolMap",             // PascalCase type-shaped name
		"SymbolMap",                  // PascalCase type name
	}
	for _, w := range want {
		if !slices.Contains(a.Anchors(), w) {
			t.Errorf("ExtractAnchors: missing anchor %q\n got: %v", w, a.Anchors())
		}
	}
}

// TestAggressiveCompressStrictPreservesAnchors is the acceptance test: a strict
// compression pass leaves every anchor token byte-identical in its output.
func TestAggressiveCompressStrictPreservesAnchors(t *testing.T) {
	cases := []struct {
		name, ext, content string
	}{
		{"go", ".go", anchorSample},
		{"rust", ".rs", `fn build(path: &str) -> SymbolMap {
    let arc = std::sync::Arc::new(load("internal/store/store.rs:7"));
    let arc = std::sync::Arc::new(arc);
    let arc = std::sync::Arc::new(arc);
    SymbolMap { handle: arc }
}
`},
		{"ts", ".ts", `function buildSymbolMap(path: string): SymbolMap {
    const data = proj.readFile("internal/store/store.ts");
    const loc = proj.locate(path, "internal/store/store.ts:42");
    const loc2 = proj.locate(path, loc);
    return new SymbolMap(data, loc2);
}
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Anchors are scoped exactly as the strict pipeline scopes them:
			// from the comment-stripped source.
			stripped := strings.Join(stripComments(strings.Split(tc.content, "\n"), tc.ext), "\n")
			anchors := ExtractAnchors(stripped)
			if anchors.Empty() {
				t.Fatal("test sample produced no anchors")
			}
			out := AggressiveCompressStrict(tc.content, tc.ext)
			if miss := anchors.Missing(out); len(miss) > 0 {
				t.Errorf("strict compression mutated/dropped anchors: %v\n--- output ---\n%s", miss, out)
			}
		})
	}
}

// TestStrictLineDropAcrossAggressivenessLevels asserts no anchor-bearing line is
// ever dropped, regardless of how aggressive the entropy threshold is set.
func TestStrictLineDropAcrossAggressivenessLevels(t *testing.T) {
	lines := strings.Split(anchorSample, "\n")
	anchors := ExtractAnchors(anchorSample)

	// Sweep from "drop nothing" (0.0) past "drop everything low-novelty" (3.0).
	for _, threshold := range []float64{0.0, 0.3, 0.6, 0.9, 1.5, 3.0} {
		kept := dropLowEntropyLinesStrict(lines, threshold, anchors)
		keptText := strings.Join(kept, "\n")
		for _, line := range lines {
			if anchors.lineHasAnchor(line) && !strings.Contains(keptText, strings.TrimSpace(line)) {
				t.Errorf("threshold %.1f dropped anchor line %q", threshold, strings.TrimSpace(line))
			}
		}
		if miss := anchors.Missing(keptText); len(miss) > 0 {
			t.Errorf("threshold %.1f lost anchors %v", threshold, miss)
		}
	}
}

// TestStrictExcludesAnchorsFromSubstitution verifies the symbol-map and n-gram
// passes never rewrite an anchor token.
func TestStrictExcludesAnchorsFromSubstitution(t *testing.T) {
	anchors := ExtractAnchors(anchorSample)
	body := strings.Join(stripComments(strings.Split(anchorSample, "\n"), ".go"), "\n")

	sm := BuildSymbolMap(body).excludeAnchors(anchors)
	out := sm.ApplyWithLegend(body)
	if !strings.Contains(out, "SymbolMap") || !strings.Contains(out, "proj.Locate") {
		t.Errorf("symbol map mutated an anchor:\n%s", out)
	}

	ncb := BuildNgramCodebook(out).excludeAnchors(anchors)
	out = ncb.ApplyWithLegend(out)
	if miss := anchors.Missing(out); len(miss) > 0 {
		t.Errorf("n-gram codebook mutated anchors %v:\n%s", miss, out)
	}
}
