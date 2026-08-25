package mcp

import "testing"

// The cost envelope is stamped uniformly at the choke point (#110 step 2):
// tokens_returned on every four-verb success; budget_left only when the input
// carried a budget, floored at 0.
func TestStampEnvelopeCost(t *testing.T) {
	// tokens_returned is always set; budget_left stays 0 without a budget.
	out := LookOutput{Status: "ok", Result: LookResult{Kind: "grep"}}
	stampEnvelopeCost(&out, LookInput{})
	if out.Cost == nil || out.Cost.TokensReturned <= 0 {
		t.Fatalf("tokens_returned not stamped: %+v", out.Cost)
	}
	if out.Cost.BudgetLeft != 0 {
		t.Fatalf("budget_left must be 0 without a budget, got %d", out.Cost.BudgetLeft)
	}

	// With a budget, budget_left = budget − tokens_returned.
	withBudget := LookOutput{Status: "ok"}
	stampEnvelopeCost(&withBudget, LookInput{Budget: 100000})
	if withBudget.Cost.BudgetLeft <= 0 || withBudget.Cost.BudgetLeft >= 100000 {
		t.Fatalf("budget_left not computed: %+v", withBudget.Cost)
	}

	// A budget smaller than the response floors at 0, never negative.
	tiny := LookOutput{Status: "ok"}
	stampEnvelopeCost(&tiny, LookInput{Budget: 1})
	if tiny.Cost.BudgetLeft != 0 {
		t.Fatalf("budget_left must floor at 0, got %d", tiny.Cost.BudgetLeft)
	}

	// An output that doesn't implement costStamper is a silent no-op.
	stampEnvelopeCost(&struct{ X int }{X: 1}, LookInput{})
}

// All verb outputs implement costStamper and all inputs implement
// budgetCarrier — the invariant the choke point relies on.
func TestFourVerbEnvelopeInterfaces(t *testing.T) {
	var outs = []any{&ContextOutput{}, &LookOutput{}, &RememberOutput{}}
	for _, o := range outs {
		if _, ok := o.(costStamper); !ok {
			t.Errorf("%T does not implement costStamper", o)
		}
	}
	var ins = []any{ContextInput{}, LookInput{}, RememberInput{}}
	for _, in := range ins {
		if _, ok := in.(budgetCarrier); !ok {
			t.Errorf("%T does not implement budgetCarrier", in)
		}
	}
}
