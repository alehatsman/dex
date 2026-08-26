package mcp

import "testing"

// deriveAskNext gives ask the same structured `next` key the exact verbs expose
// (#110), without downgrading ask's richer trust/next_action. These pin the
// derivation: grounded in a suggested read, non-destructive, and silent when
// there is nothing concrete to point at.

func TestDeriveAskNextFromTopRead(t *testing.T) {
	out := &ContextOutput{
		SuggestedReads: []SuggestedRead{
			{Path: "internal/mcp/context.go", StartLine: 42, Reason: "router entry point"},
			{Path: "internal/mcp/look.go", StartLine: 1},
		},
	}
	deriveAskNext(out)
	if len(out.Next) != 1 {
		t.Fatalf("want 1 next step, got %d", len(out.Next))
	}
	n := out.Next[0]
	// #207: the emitter now names the live read verb (query/input) directly,
	// rather than emitting look/target for a later normalizeNext pass to rewrite.
	if n.Verb != "query" {
		t.Errorf("verb = %q, want query", n.Verb)
	}
	if got := n.Args["input"]; got != "internal/mcp/context.go:42" {
		t.Errorf("input = %v, want internal/mcp/context.go:42", got)
	}
	if n.Why != "router entry point" {
		t.Errorf("why = %q, want the read's reason", n.Why)
	}
	assertLiveNextSteps(t, out.Next)
}

func TestDeriveAskNextNoLineNumber(t *testing.T) {
	out := &ContextOutput{SuggestedReads: []SuggestedRead{{Path: "README.md"}}}
	deriveAskNext(out)
	if len(out.Next) != 1 || out.Next[0].Args["input"] != "README.md" {
		t.Fatalf("want bare-path input, got %+v", out.Next)
	}
	if out.Next[0].Why != "open the top suggested read" {
		t.Errorf("why = %q, want default", out.Next[0].Why)
	}
}

func TestDeriveAskNextEmptyWhenNoReads(t *testing.T) {
	out := &ContextOutput{}
	deriveAskNext(out)
	if out.Next != nil {
		t.Fatalf("want no next step without suggested reads, got %+v", out.Next)
	}
}

func TestDeriveAskNextDoesNotOverwrite(t *testing.T) {
	explicit := []NextStep{{Verb: "act", Why: "router chose this"}}
	out := &ContextOutput{
		Next:           explicit,
		SuggestedReads: []SuggestedRead{{Path: "x.go", StartLine: 3}},
	}
	deriveAskNext(out)
	if len(out.Next) != 1 || out.Next[0].Verb != "act" {
		t.Fatalf("deriveAskNext overwrote an explicit next: %+v", out.Next)
	}
}
