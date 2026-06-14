package retrieve

import "testing"

// TestInlineCapsFor pins the per-intent budget split: exploration
// intents (architecture, package_topology) get a denser bundle than
// targeted ones. Guards against a future edit silently flattening the
// two tiers, which would either starve exploration or bloat targeted
// responses.
func TestInlineCapsFor(t *testing.T) {
	exploration := []string{IntentArchitecture, IntentPackageTopology}
	targeted := []string{
		IntentBehaviorSearch, IntentSymbolLookup, IntentCallers,
		IntentCallees, IntentEditingContext,
	}
	for _, intent := range exploration {
		c := InlineCapsFor(intent)
		if c.TotalBytesCap < 32*1024 {
			t.Errorf("%s TotalBytesCap=%d, want ≥32 KB for exploration", intent, c.TotalBytesCap)
		}
		if c.MaxLinesPerRead < 100 {
			t.Errorf("%s MaxLinesPerRead=%d, want ≥100 for exploration", intent, c.MaxLinesPerRead)
		}
	}
	for _, intent := range targeted {
		c := InlineCapsFor(intent)
		if c.TotalBytesCap > 24*1024 {
			t.Errorf("%s TotalBytesCap=%d, want ≤24 KB for targeted intents", intent, c.TotalBytesCap)
		}
	}
}
