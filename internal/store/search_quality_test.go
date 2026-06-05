package store

import (
	"testing"
)

func TestNoisePenalty(t *testing.T) {
	cases := []struct {
		path string
		want float64
	}{
		{"internal/store/store_test.go", 0.3},
		{"internal/store/store.go", 1.0},
		{"/tests/integration_test.go", 0.3},
		{"pkg/legacy/old.go", 0.3},
		{"examples/demo.go", 0.3},
		{"fixtures/data.json", 0.3},
		{"testdata/input.txt", 0.3},
		{"types/index.d.ts", 0.7},
		{"stubs/api.pyi", 0.7},
		{"internal/mcp/server.go", 1.0},
	}
	for _, c := range cases {
		got := noisePenalty(c.path)
		if got != c.want {
			t.Errorf("noisePenalty(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestApplyMMR_NoDominance(t *testing.T) {
	// Three chunks from file A score highest, but MMR should interleave file B.
	pathFor := map[int64]string{
		1: "a.go", 2: "a.go", 3: "a.go",
		4: "b.go", 5: "b.go",
	}
	pool := []scored{
		{1, 0.9}, {2, 0.8}, {3, 0.7},
		{4, 0.6}, {5, 0.5},
	}
	result := applyMMR(pool, pathFor, 4)
	if len(result) != 4 {
		t.Fatalf("want 4 results, got %d", len(result))
	}
	// First result must be the highest-scored (id=1 from a.go).
	if result[0].id != 1 {
		t.Errorf("expected first result id=1, got %d", result[0].id)
	}
	// b.go must appear in top-4 (decay punishes a.go after first hit).
	var bSeen bool
	for _, r := range result {
		if pathFor[r.id] == "b.go" {
			bSeen = true
		}
	}
	if !bSeen {
		t.Error("b.go should appear in top-4 results via MMR diversity")
	}
}

func TestApplyMMR_SmallPool(t *testing.T) {
	// Pool smaller than k — must return all items unchanged.
	pathFor := map[int64]string{1: "a.go", 2: "b.go"}
	pool := []scored{{1, 0.9}, {2, 0.8}}
	result := applyMMR(pool, pathFor, 5)
	if len(result) != 2 {
		t.Errorf("want 2, got %d", len(result))
	}
}
