package mcp

import "testing"

// The cost envelope is stamped uniformly at the choke point (#110 step 2):
// tokens_returned on every four-verb success; budget_left only when the input
// carried a budget, floored at 0. BudgetLeft is a *int (#231) so an exhausted
// budget (0 left) is distinguishable from no budget having been set at all —
// both must round-trip through JSON distinctly rather than the field being
// silently omitted in either case.
func TestStampEnvelopeCost(t *testing.T) {
	// tokens_returned is always set; budget_left stays nil (unset) without a budget.
	out := LookOutput{Status: "ok", Result: LookResult{Kind: "grep"}}
	stampEnvelopeCost(&out, LookInput{})
	if out.Cost == nil || out.Cost.TokensReturned <= 0 {
		t.Fatalf("tokens_returned not stamped: %+v", out.Cost)
	}
	if out.Cost.BudgetLeft != nil {
		t.Fatalf("budget_left must be nil without a budget, got %v", *out.Cost.BudgetLeft)
	}

	// With a budget, budget_left = budget − tokens_returned.
	withBudget := LookOutput{Status: "ok"}
	stampEnvelopeCost(&withBudget, LookInput{Budget: 100000})
	if withBudget.Cost.BudgetLeft == nil || *withBudget.Cost.BudgetLeft <= 0 || *withBudget.Cost.BudgetLeft >= 100000 {
		t.Fatalf("budget_left not computed: %+v", withBudget.Cost)
	}

	// A budget smaller than the response floors at 0 (still set, not omitted —
	// "budget exhausted" must not look like "no budget was passed").
	tiny := LookOutput{Status: "ok"}
	stampEnvelopeCost(&tiny, LookInput{Budget: 1})
	if tiny.Cost.BudgetLeft == nil || *tiny.Cost.BudgetLeft != 0 {
		t.Fatalf("budget_left must be present and floor at 0, got %+v", tiny.Cost)
	}

	// An output that doesn't implement costStamper is a silent no-op.
	stampEnvelopeCost(&struct{ X int }{X: 1}, LookInput{})
}

// All verb outputs implement costStamper and all inputs implement
// budgetCarrier — the invariant the choke point relies on.
func TestFourVerbEnvelopeInterfaces(t *testing.T) {
	var outs = []any{&ContextOutput{}, &LookOutput{}}
	for _, o := range outs {
		if _, ok := o.(costStamper); !ok {
			t.Errorf("%T does not implement costStamper", o)
		}
	}
	var ins = []any{ContextInput{}, LookInput{}}
	for _, in := range ins {
		if _, ok := in.(budgetCarrier); !ok {
			t.Errorf("%T does not implement budgetCarrier", in)
		}
	}
}
