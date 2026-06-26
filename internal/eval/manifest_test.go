package eval

import "testing"

func baseManifest() EvalManifest {
	return EvalManifest{
		SchemaVersion:  ManifestSchemaVersion,
		GoldenMode:     "git-history",
		QuerySetSHA256: "abc123",
		Lane:           "full",
		EmbedModel:     "qwen3-embedding:4b",
		EmbedDim:       2560,
		FusionMode:     "linear",
		FusionAlpha:    0.7,
		GraphWeight:    1.0,
		K:              10,
	}
}

func TestManifestIncompatible_Identical(t *testing.T) {
	a := baseManifest()
	b := baseManifest()
	if diffs := a.Incompatible(b); len(diffs) != 0 {
		t.Fatalf("identical manifests reported incompatible: %v", diffs)
	}
}

func TestManifestIncompatible_HeadDriftIsCompatible(t *testing.T) {
	// HEAD drift is staleness, not incompatibility — it must not break a
	// metric comparison (handled separately by StaleGolden).
	a := baseManifest()
	a.RepoHead, a.GoldenHead, a.GoldenSHA256 = "deadbeef", "cafef00d", "filehash-now"
	b := baseManifest()
	b.RepoHead, b.GoldenHead, b.GoldenSHA256 = "0000000", "1111111", "filehash-ref"
	if diffs := a.Incompatible(b); len(diffs) != 0 {
		t.Fatalf("HEAD/file-hash drift wrongly reported incompatible: %v", diffs)
	}
}

func TestManifestIncompatible_DetectsIdentityChanges(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*EvalManifest)
	}{
		{"k", func(m *EvalManifest) { m.K = 20 }},
		{"lane", func(m *EvalManifest) { m.Lane = "bm25" }},
		{"mode", func(m *EvalManifest) { m.GoldenMode = "orphan" }},
		{"model", func(m *EvalManifest) { m.EmbedModel = "other" }},
		{"dim", func(m *EvalManifest) { m.EmbedDim = 384 }},
		{"fusion_mode", func(m *EvalManifest) { m.FusionMode = "rrf" }},
		{"fusion_alpha", func(m *EvalManifest) { m.FusionAlpha = 0.5 }},
		{"graph_weight", func(m *EvalManifest) { m.GraphWeight = 2.0 }},
		{"query_corpus", func(m *EvalManifest) { m.QuerySetSHA256 = "different" }},
		{"schema", func(m *EvalManifest) { m.SchemaVersion = 99 }},
		{"rerank_enabled", func(m *EvalManifest) { m.RerankEnabled = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := baseManifest()
			tc.mut(&a)
			if diffs := a.Incompatible(baseManifest()); len(diffs) == 0 {
				t.Fatalf("change to %s not flagged incompatible", tc.name)
			}
		})
	}
}

func TestQuerySetSHA256_OrderIndependentAndContentSensitive(t *testing.T) {
	gs1 := GoldenSet{Queries: []GoldenQuery{
		{ID: "q1", Query: "parse config", RelevantFiles: []string{"a.go", "b.go"}},
		{ID: "q2", Query: "open store", RelevantFiles: []string{"c.go"}},
	}}
	// Same queries, reversed order + reversed relevant-file order → same hash.
	gs2 := GoldenSet{Queries: []GoldenQuery{
		{ID: "q2", Query: "open store", RelevantFiles: []string{"c.go"}},
		{ID: "q1", Query: "parse config", RelevantFiles: []string{"b.go", "a.go"}},
	}}
	if QuerySetSHA256(gs1) != QuerySetSHA256(gs2) {
		t.Fatal("hash not order-independent")
	}
	// Editing a relevant file changes the hash.
	gs3 := GoldenSet{Queries: []GoldenQuery{
		{ID: "q1", Query: "parse config", RelevantFiles: []string{"a.go", "z.go"}},
		{ID: "q2", Query: "open store", RelevantFiles: []string{"c.go"}},
	}}
	if QuerySetSHA256(gs1) == QuerySetSHA256(gs3) {
		t.Fatal("hash insensitive to relevant-file change")
	}
	// Dropping a query changes the hash.
	gs4 := GoldenSet{Queries: gs1.Queries[:1]}
	if QuerySetSHA256(gs1) == QuerySetSHA256(gs4) {
		t.Fatal("hash insensitive to query-count change")
	}
}

func TestStaleGolden(t *testing.T) {
	cases := []struct {
		golden, repo string
		want         bool
	}{
		{"abc", "abc", false},
		{"abc", "def", true},
		{"", "def", false}, // unknown golden HEAD → not stale
		{"abc", "", false}, // unknown repo HEAD → not stale
		{"", "", false},
	}
	for _, c := range cases {
		if got := StaleGolden(c.golden, c.repo); got != c.want {
			t.Errorf("StaleGolden(%q,%q)=%v want %v", c.golden, c.repo, got, c.want)
		}
	}
}
