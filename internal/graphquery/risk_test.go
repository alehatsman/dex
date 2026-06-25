package graphquery

import (
	"testing"
)

func TestRiskLevelBuckets(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "Low"},
		{1, "Low"},
		{2, "Medium"},
		{4, "Medium"},
		{5, "High"},
		{10, "High"},
		{11, "Critical"},
		{100, "Critical"},
	}
	for _, tc := range cases {
		got := RiskLevel(tc.n)
		if got != tc.want {
			t.Errorf("RiskLevel(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestTransitiveCallerCount(t *testing.T) {
	// impactView has: seed <- caller1, caller2 <- depth2
	// total unique callers: 3 (caller1, caller2, depth2)
	view, seed := impactView()

	got := TransitiveCallerCount(view, []Node{seed}, 5)
	if got != 3 {
		t.Errorf("TransitiveCallerCount depth=5: want 3, got %d", got)
	}
}

func TestTransitiveCallerCountDepthCap(t *testing.T) {
	view, seed := impactView()

	// depth=1 only sees caller1 and caller2 (2 nodes); depth2 is at depth 2.
	got := TransitiveCallerCount(view, []Node{seed}, 1)
	if got != 2 {
		t.Errorf("TransitiveCallerCount depth=1: want 2, got %d", got)
	}
}

func TestTransitiveCallerCountNone(t *testing.T) {
	view, seed := impactView()

	// depth2 has no callers — count must be 0.
	depth2 := view.NodesByQualified["mcp.RunStdio"]
	if len(depth2) == 0 {
		t.Fatal("mcp.RunStdio not found in view")
	}
	got := TransitiveCallerCount(view, []Node{depth2[0]}, 5)
	if got != 0 {
		t.Errorf("TransitiveCallerCount for leaf node: want 0, got %d", got)
	}
	_ = seed
}

func TestRiskLevelFromTransitiveCount(t *testing.T) {
	// Integration: use the fixture view, verify risk classification end-to-end.
	view, seed := impactView()
	count := TransitiveCallerCount(view, []Node{seed}, 5)
	// count = 3 → Medium (2–4 range)
	risk := RiskLevel(count)
	if risk != "Medium" {
		t.Errorf("RiskLevel(%d) = %q, want %q", count, risk, "Medium")
	}
}
