package codemap

import (
	"strings"
	"testing"
)

func TestLayerizeDAG(t *testing.T) {
	// b→a, c→b, c→a : a is foundational, b above it, c on top.
	edges := []ImportEdge{
		{"mod/b", "mod/a"},
		{"mod/c", "mod/b"},
		{"mod/c", "mod/a"},
	}
	layers, cyclic := layerize(edges)
	if len(cyclic) != 0 {
		t.Fatalf("DAG must have no cycles, got %v", cyclic)
	}
	if len(layers) != 3 {
		t.Fatalf("want 3 layers, got %d: %v", len(layers), layers)
	}
	want := [][]string{{"mod/a"}, {"mod/b"}, {"mod/c"}}
	for i, w := range want {
		if len(layers[i]) != 1 || layers[i][0] != w[0] {
			t.Errorf("layer %d = %v, want %v", i, layers[i], w)
		}
	}
}

func TestLayerizeCycleGuarded(t *testing.T) {
	// p↔q is impossible in Go but possible via tree-sitter; layerize must not
	// hang and must surface the cyclic packages instead of mislayering forever.
	edges := []ImportEdge{{"m/p", "m/q"}, {"m/q", "m/p"}}
	_, cyclic := layerize(edges)
	if len(cyclic) == 0 {
		t.Fatal("expected at least one package flagged cyclic")
	}
}

func TestRenderLayers(t *testing.T) {
	edges := []ImportEdge{
		{"mod/internal/b", "mod/internal/a"},
		{"mod/internal/c", "mod/internal/b"},
	}
	got := RenderLayers(edges, DefaultLayersBudget)
	if !strings.HasPrefix(got, layersHeader) {
		t.Fatalf("missing header:\n%s", got)
	}
	// Foundational first, short names.
	if !strings.Contains(got, "- L0: a\n") {
		t.Errorf("want L0 with a:\n%s", got)
	}
	if !strings.Contains(got, "- L1: b\n") || !strings.Contains(got, "- L2: c\n") {
		t.Errorf("want L1 b and L2 c:\n%s", got)
	}
}

func TestRenderLayersEmpty(t *testing.T) {
	if got := RenderLayers(nil, 0); got != "" {
		t.Errorf("no edges should render empty, got %q", got)
	}
}

func TestLayerLineCapsPackages(t *testing.T) {
	many := make([]string, 0, 12)
	for _, c := range "abcdefghijkl" {
		many = append(many, "mod/"+string(c))
	}
	line := layerLine(0, many, disambiguateNames(many))
	if !strings.Contains(line, "(+") || !strings.HasSuffix(line, "more)\n") {
		t.Errorf("expected an overflow summary, got %q", line)
	}
}

func TestDisambiguateNamesCollision(t *testing.T) {
	// Two packages sharing a tail get widened to two segments.
	edges := []ImportEdge{
		{"m/internal/a", "m/internal/compress"},
		{"m/internal/a", "m/internal/bench/compress"},
	}
	got := RenderLayers(edges, DefaultLayersBudget)
	if !strings.Contains(got, "internal/compress") || !strings.Contains(got, "bench/compress") {
		t.Errorf("colliding compress packages should be disambiguated:\n%s", got)
	}
	if strings.Count(got, " compress,")+strings.Count(got, " compress\n") > 0 {
		t.Errorf("bare 'compress' should not appear once disambiguated:\n%s", got)
	}
}
