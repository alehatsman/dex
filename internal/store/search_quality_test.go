package store

import (
	"testing"
)

func TestPathPenalty(t *testing.T) {
	tests := []struct {
		path string
		want float64
	}{
		// No penalty — pure implementation files
		{"internal/store/store.go", 1.0},
		{"cmd/dex/main.go", 1.0},
		{"src/components/Button.tsx", 1.0},

		// Test files: 0.3×
		{"internal/store/store_test.go", 0.3},
		{"src/utils/parse.test.ts", 0.3},
		{"tests/test_parser.py", 0.3},
		{"internal/mcp/server_test.go", 0.3},

		// Mock/fake/stub directories: 0.3×
		{"internal/mocks/embed_mock.go", 0.3},
		{"src/fake/server.go", 0.3},
		{"pkg/stubs/client.go", 0.3},
		{"testutil/helper.go", 0.3},

		// Compat/legacy/deprecated: 0.3×
		{"internal/compat/old_api.go", 0.3},
		{"src/legacy/handler.go", 0.3},
		{"pkg/deprecated/method.go", 0.3},

		// Examples/docs/demo: 0.3×
		{"examples/basic/main.go", 0.3},
		{"demo/app.go", 0.3},
		{"docs_src/snippet.py", 0.3},
		{"samples/hello.go", 0.3},

		// Re-export barrels: 0.5×
		{"src/index.ts", 0.5},
		{"src/components/index.tsx", 0.5},
		{"mypackage/__init__.py", 0.5},
		{"com/myapp/package-info.java", 0.5}, // barrel only (no example segment)
		{"src/lib/mod.rs", 0.5},

		// Type stubs: 0.7×
		{"types/api.d.ts", 0.7},
		{"stubs/utils.pyi", 0.7 * 0.3}, // also in stubs dir → 0.7 × 0.3
	}
	for _, tt := range tests {
		got := pathPenalty(tt.path)
		if got != tt.want {
			t.Errorf("pathPenalty(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// TestApplyLocalRerank_MMRNoDominance verifies the MMR pass interleaves a
// second file even when one file holds the top raw scores.
func TestApplyLocalRerank_MMRNoDominance(t *testing.T) {
	hits := []Hit{
		{Name: "a1", Path: "a.go", RRFScore: 0.9},
		{Name: "a2", Path: "a.go", RRFScore: 0.8},
		{Name: "a3", Path: "a.go", RRFScore: 0.7},
		{Name: "b1", Path: "b.go", RRFScore: 0.6},
		{Name: "b2", Path: "b.go", RRFScore: 0.5},
	}
	out := ApplyLocalRerank(hits, false, 0)
	if len(out) != 5 {
		t.Fatalf("want 5 results, got %d", len(out))
	}
	// Highest-scored chunk still leads.
	if out[0].Path != "a.go" {
		t.Errorf("expected first result from a.go, got %q", out[0].Path)
	}
	// b.go must surface in the top-4 — MMR decay punishes a.go's 3rd chunk.
	var bInTop4 bool
	for _, h := range out[:4] {
		if h.Path == "b.go" {
			bInTop4 = true
		}
	}
	if !bInTop4 {
		t.Error("b.go should appear in top-4 results via MMR diversity")
	}
}

// TestApplyLocalRerank_RespectsCrossEncoder verifies that a non-zero
// RerankScore (cross-encoder ran) short-circuits the local reranker so it
// does not clobber the authoritative ordering.
func TestApplyLocalRerank_RespectsCrossEncoder(t *testing.T) {
	hits := []Hit{
		{Name: "x", Path: "a.go", RRFScore: 0.1, RerankScore: 0.9},
		{Name: "y", Path: "b.go", RRFScore: 0.9, RerankScore: 0.1},
	}
	out := ApplyLocalRerank(hits, false, 0)
	if out[0].Name != "x" {
		t.Errorf("cross-encoder order must be preserved; got first = %q", out[0].Name)
	}
	// Cross-encoder short-circuit must NOT stamp SortScore — RerankScore
	// stays the authoritative sort key (and DisplayScore falls back to it).
	for _, h := range out {
		if h.SortScore != 0 {
			t.Errorf("cross-encoder path should leave SortScore zero; got %v for %q", h.SortScore, h.Name)
		}
	}
}

// TestApplyLocalRerank_SetsMonotonicSortScore guards #518: the local reranker
// must stamp SortScore so a rendered score= is monotonic with the visible
// order, even though coherence/MMR reorder relative to RRFScore.
func TestApplyLocalRerank_SetsMonotonicSortScore(t *testing.T) {
	hits := []Hit{
		{Name: "a1", Path: "a.go", RRFScore: 0.9},
		{Name: "a2", Path: "a.go", RRFScore: 0.8},
		{Name: "b1", Path: "b.go", RRFScore: 0.6},
	}
	out := ApplyLocalRerank(hits, false, 0)
	for i, h := range out {
		if h.SortScore <= 0 {
			t.Errorf("hit %d (%q) missing SortScore; got %v", i, h.Name, h.SortScore)
		}
		if i > 0 && out[i-1].SortScore < h.SortScore {
			t.Errorf("SortScore non-monotonic: out[%d]=%v < out[%d]=%v",
				i-1, out[i-1].SortScore, i, h.SortScore)
		}
	}
}

// TestHit_DisplayScore_Precedence pins the score= precedence used by `dex find`
// default output: SortScore > RerankScore > RRFScore > cosine Score.
func TestHit_DisplayScore_Precedence(t *testing.T) {
	cases := []struct {
		name string
		h    Hit
		want float32
	}{
		{"semantic-only", Hit{Score: 0.42}, 0.42},
		{"rrf-over-cosine", Hit{Score: 0.42, RRFScore: 0.7}, 0.7},
		{"rerank-over-rrf", Hit{Score: 0.42, RRFScore: 0.7, RerankScore: 0.9}, 0.9},
		{"sortscore-wins", Hit{Score: 0.42, RRFScore: 0.7, RerankScore: 0.9, SortScore: 0.55}, 0.55},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.h.DisplayScore(); got != tc.want {
				t.Errorf("DisplayScore() = %v, want %v", got, tc.want)
			}
		})
	}
}
