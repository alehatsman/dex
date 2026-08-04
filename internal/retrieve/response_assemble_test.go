package retrieve

import (
	"strings"
	"testing"
)

func TestAssembleConcerns(t *testing.T) {
	// No keys → zero Concerns (transport maps this to a nil pointer).
	if c := AssembleConcerns([]SymHit{{QualifiedName: "Foo", Body: "x"}}, nil); len(c.Covered)+len(c.Dropped) != 0 {
		t.Fatalf("no keywords → zero Concerns, got %+v", c)
	}

	syms := []SymHit{
		{QualifiedName: "config.Parse", Signature: "func Parse(b []byte) Config", Body: "..."}, // inlined
		{QualifiedName: "history.Prune", Signature: "func Prune()"},                            // NOT inlined (no body)
	}
	c := AssembleConcerns(syms, []string{"parse", "config", "prune", "history"})

	// parse+config are in the inlined symbol's name/signature → covered.
	// prune+history only appear on the non-inlined symbol → dropped, proving
	// coverage is judged over the inlined working set, not every candidate.
	covered := strings.Join(c.Covered, ",")
	if !strings.Contains(covered, "parse") || !strings.Contains(covered, "config") {
		t.Errorf("parse+config must be covered by the inlined symbol, got covered=%v", c.Covered)
	}
	dropped := strings.Join(c.Dropped, ",")
	if !strings.Contains(dropped, "prune") || !strings.Contains(dropped, "history") {
		t.Errorf("prune+history live only on a non-inlined symbol → dropped, got dropped=%v", c.Dropped)
	}
}

func TestAssembleConcernsSignatureHaystack(t *testing.T) {
	// A concern present ONLY in the signature (not the qualified name) must
	// still count as covered — guards the toNeutralSyms Signature carry-through.
	syms := []SymHit{{QualifiedName: "x.Handler", Signature: "func Handler(store Store)", Body: "..."}}
	c := AssembleConcerns(syms, []string{"store"})
	if len(c.Covered) != 1 || c.Covered[0] != "store" {
		t.Errorf("signature-only concern must be covered, got %+v", c)
	}
}

func TestFirstInlinedAnchor(t *testing.T) {
	if got := firstInlinedAnchor(nil); got != "" {
		t.Errorf("empty set → no anchor, got %q", got)
	}
	if got := firstInlinedAnchor([]SymHit{{QualifiedName: "a"}, {QualifiedName: "b"}}); got != "" {
		t.Errorf("no inlined bodies → no anchor, got %q", got)
	}
	syms := []SymHit{{QualifiedName: "a"}, {QualifiedName: "b", Body: "x"}, {QualifiedName: "c", Body: "y"}}
	if got := firstInlinedAnchor(syms); got != "b" {
		t.Errorf("anchor = first inlined symbol, want b, got %q", got)
	}
}

func TestAssembleNextActionHint(t *testing.T) {
	// editing_context multi-site → nudge toward intent=assemble.
	got := AssembleNextActionHint(IntentEditingContext, "Edit here.", Concerns{}, 2, nil)
	if !strings.Contains(got, "intent=assemble") {
		t.Errorf("multi-site editing_context must nudge toward assemble, got %q", got)
	}

	// assemble with a dropped concern + an inlined anchor → concrete chained
	// trace directive naming the first inlined symbol.
	c := Concerns{Covered: []string{"parse"}, Dropped: []string{"prune"}}
	syms := []SymHit{{QualifiedName: "pkg.Parse", Body: "func Parse()"}}
	got = AssembleNextActionHint(IntentAssemble, "Base.", c, 0, syms)
	if !strings.Contains(got, "Trace callees of pkg.Parse") {
		t.Errorf("anchored partial must chain a concrete trace directive, got %q", got)
	}
	if !strings.Contains(got, "Do not treat this set as complete") {
		t.Errorf("partial must keep the honest-partial framing, got %q", got)
	}

	// zero Concerns (nil-pointer case) → assemble caveat skipped, next_action
	// returned unchanged.
	if got := AssembleNextActionHint(IntentAssemble, "Base.", Concerns{}, 0, syms); got != "Base." {
		t.Errorf("no dropped concerns → next_action unchanged, got %q", got)
	}
}
