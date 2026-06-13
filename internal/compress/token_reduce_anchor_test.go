package compress

import (
	"strconv"
	"strings"
	"testing"
)

// TestStrictTokenReductionsNeverMutateAnchors gates EVERY tokenizer-gated
// substitution rule on the anchor-preservation floor (#291): when a rule's
// source text sits inside an anchor, the strict path
// (applyTokenReductionsExcept) must leave that anchor byte-identical.
//
// This is the #358 acceptance. The test enumerates the LIVE rule slices, so a
// rule added by #292 (per-tokenizer rule eligibility) is covered with no extra
// wiring — a new rule that could corrupt an anchor fails here at author-time,
// rather than slipping past #292's ratio-only acceptance and surfacing only if
// #296's small bench corpus happens to contain the affected token.
func TestStrictTokenReductionsNeverMutateAnchors(t *testing.T) {
	groups := []struct {
		name  string
		ext   string
		rules []tokenRule
	}{
		{"global", ".go", globalTokenRules},
		{"rust", ".rs", rustTokenRules},
		{"jsts", ".ts", jstsTokenRules},
	}
	// lead/trail are anchor-shaped padding that no rule matches. Wrapping a
	// rule's `from` in them models a real anchor that merely CONTAINS the
	// rule's source text — the #357 corruption shape (e.g. a "Result" rule
	// firing inside the "std::io::Result" anchor). blocksText must then skip
	// the rule in either overlap direction.
	const lead, trail = "AnchorL9", "AnchorR9"
	for _, g := range groups {
		for _, r := range g.rules {
			r := r
			t.Run(g.name+"/"+strconv.Quote(r.from), func(t *testing.T) {
				anchor := lead + r.from + trail
				anchors := AnchorSet{anchors: []string{anchor}}
				content := "x := " + anchor + " // tail"
				out := applyTokenReductionsExcept(content, g.ext, anchors)
				if miss := anchors.Missing(out); len(miss) > 0 {
					t.Errorf("rule %q->%q mutated anchor %q\n in:  %q\n out: %q",
						r.from, r.to, anchor, content, out)
				}
			})
		}
	}
}

// TestRelaxedTokenReductionsMutateAnchors_StrictDoesNot pins the documented
// asymmetry (#358): the relaxed path (ApplyTokenReductions, taken for strong
// targets like Claude) is by-design lossy on anchors, while the strict path
// holds substitutions off them. Encoding it as a test keeps the asymmetry
// intentional rather than an oversight, and proves the protection runs through
// real ExtractAnchors extraction — not just hand-built anchor sets.
func TestRelaxedTokenReductionsMutateAnchors_StrictDoesNot(t *testing.T) {
	const src = `let a = std::sync::Arc::new(load("internal/store/store.rs:7"));`

	anchors := ExtractAnchors(src)
	if !anchors.blocksText("std::sync::Arc") {
		t.Fatalf("precondition: no extracted anchor overlaps the std::sync::Arc rule: %v", anchors.Anchors())
	}

	// Relaxed: the std::sync::Arc -> Arc rule fires, mutating the anchor.
	if relaxed := ApplyTokenReductions(src, ".rs"); strings.Contains(relaxed, "std::sync::Arc") {
		t.Errorf("relaxed path unexpectedly preserved std::sync::Arc — the strong-target asymmetry assumption broke:\n%s", relaxed)
	}

	// Strict: the same rule is held off the anchor; nothing is mutated.
	if strict := applyTokenReductionsExcept(src, ".rs", anchors); anchors.Missing(strict) != nil {
		t.Errorf("strict path mutated anchors %v:\n%s", anchors.Missing(strict), applyTokenReductionsExcept(src, ".rs", anchors))
	}
}
