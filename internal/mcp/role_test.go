package mcp

import "testing"

func TestFormatRole(t *testing.T) {
	cases := []struct {
		name        string
		nameStr     string
		in, out, x  int
		betweenness float64
		want        string
	}{
		{"all-zero stays empty", "Foo", 0, 0, 0, 0, ""},
		{"high in_degree → central", "Indexer", 12, 4, 0, 0, "central:12"},
		{"cross_pkg ≥ 2 → central with pkg suffix", "Builder", 3, 1, 4, 0, "central:3/4pkg"},
		{"in_degree threshold + pkg suffix", "Hub", 5, 1, 2, 0, "central:5/2pkg"},
		{"exported with no callers", "Public", 0, 2, 0, 0, "exported-unused"},
		{"lower-case with no callers stays empty", "private", 0, 2, 0, 0, ""},
		{"leaf: in>0, out=0", "helper", 2, 0, 0, 0, "leaf"},
		{"middle of the road stays empty", "mid", 2, 3, 0, 0, ""},
		{"cross_pkg=1 alone is not enough", "thin", 1, 1, 1, 0, ""},
		{"bridge: high betweenness", "relay", 1, 1, 0, 0.15, "bridge:15%"},
		{"bridge suppressed when central wins", "hub", 10, 2, 3, 0.2, "central:10/3pkg"},
		{"bridge threshold is 0.1", "gatekeeper", 0, 1, 0, 0.09, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatRole(tc.nameStr, tc.in, tc.out, tc.x, tc.betweenness)
			if got != tc.want {
				t.Errorf("formatRole(%q, in=%d, out=%d, pkg=%d, bw=%.2f) = %q, want %q",
					tc.nameStr, tc.in, tc.out, tc.x, tc.betweenness, got, tc.want)
			}
		})
	}
}

func TestGraphAgreement(t *testing.T) {
	cases := []struct {
		name        string
		inDegree    int
		crossPkg    int
		betweenness float64
		want        int
	}{
		{"unremarkable", 0, 0, 0, 1},
		{"leaf-shaped (in>0, low everything else)", 2, 0, 0, 1},
		{"high in_degree → central", 12, 0, 0, 3},
		{"cross_pkg ≥ 2 → central", 1, 2, 0, 3},
		{"bridge: high betweenness", 1, 0, 0.15, 2},
		{"central wins over bridge", 10, 3, 0.2, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := graphAgreement(tc.inDegree, tc.crossPkg, tc.betweenness); got != tc.want {
				t.Errorf("graphAgreement(in=%d, pkg=%d, bw=%.2f) = %d, want %d",
					tc.inDegree, tc.crossPkg, tc.betweenness, got, tc.want)
			}
		})
	}
}

// TestReweightedPageRankPromotesCentralPeerOnMiss locks #220: extends the
// #731/#783 live feedback reweight to graph-lane (callers/callees) ordering.
// A central peer (multiple independent signals already agree it matters)
// should be promoted above a higher-raw-PageRank but unremarkable peer once
// the intent's open-rate signal shows a total miss at high confidence — the
// same shape as TestShadowReorderPrefersCrossLaneAgreementOnMiss for semantic
// hits, but for graph nodes via graphAgreement in place of lane count.
func TestReweightedPageRankPromotesCentralPeerOnMiss(t *testing.T) {
	const openRate, n = 0.0, 1000 // total miss, high confidence
	plain := reweightedPageRank(1.00, 0, 0, 0, openRate, n)
	central := reweightedPageRank(0.95, 12, 0, 0, openRate, n)
	if central <= plain {
		t.Errorf("central peer (%.4f) should be promoted above plain peer (%.4f) on total miss", central, plain)
	}
}

// TestReweightedPageRankNoSignalIsIdentity locks the off/no-data path: with
// no open-rate signal (n=0 — live reweight off or an intent with no history
// yet), reweightedPageRank must reduce to the raw PageRank unchanged.
func TestReweightedPageRankNoSignalIsIdentity(t *testing.T) {
	got := reweightedPageRank(0.42, 12, 0, 0, 0.0, 0)
	if got != 0.42 {
		t.Errorf("reweightedPageRank with n=0 = %v, want identity 0.42", got)
	}
}
