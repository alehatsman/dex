package eval

import "testing"

func ptr(f float64) *float64 { return &f }

// TestRerankDegraded pins the gate that #865 adds: a run whose cross-encoder
// served only part of its eligible calls is the same experiment measured badly,
// not a different experiment — so it is reported here, not via Incompatible.
func TestRerankDegraded(t *testing.T) {
	cases := []struct {
		name         string
		rate         *float64
		wantDegraded bool
	}{
		{"nothing eligible -> not degraded", nil, false},
		{"fully served -> not degraded", ptr(1.0), false},
		{"partial outage -> degraded", ptr(0.63), true},
		{"total outage while wired -> degraded", ptr(0.0), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := EvalManifest{RerankObservedRate: tc.rate}
			rate, degraded := m.RerankDegraded()
			if degraded != tc.wantDegraded {
				t.Errorf("RerankDegraded() degraded = %v, want %v (rate %v)", degraded, tc.wantDegraded, rate)
			}
		})
	}
}

// TestRerankObservedRateIsNotIdentity guards the deliberate omission: the rate
// must NOT join Incompatible. A partial outage does not make two runs describe
// different experiments, and folding it in would invalidate every committed
// baseline (none of which carry a rate) on the next comparison.
func TestRerankObservedRateIsNotIdentity(t *testing.T) {
	base := EvalManifest{SchemaVersion: ManifestSchemaVersion, RerankEnabled: true}
	withRate := base
	withRate.RerankObservedRate = ptr(0.5)

	if diffs := withRate.Incompatible(base); len(diffs) > 0 {
		t.Errorf("rerank_observed_rate leaked into the identity gate: %v — a baseline without a rate would stop comparing", diffs)
	}
}
