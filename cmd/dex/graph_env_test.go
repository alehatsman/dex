package main

import "testing"

// graphGamma / graphHopCap return 0 when unset or invalid, signalling "let the
// store apply its compiled-in default" rather than substituting it here.

func TestGraphGammaUnsetIsZero(t *testing.T) {
	t.Setenv("DEX_GRAPH_GAMMA", "")
	if got := graphGamma(); got != 0 {
		t.Errorf("graphGamma() = %v, want 0 (unset → store default)", got)
	}
}

func TestGraphGammaHonoredInRange(t *testing.T) {
	t.Setenv("DEX_GRAPH_GAMMA", "0.7")
	if got := graphGamma(); got != 0.7 {
		t.Errorf("graphGamma() = %v, want 0.7", got)
	}
	// Boundary: 1.0 is valid (inclusive upper bound).
	t.Setenv("DEX_GRAPH_GAMMA", "1")
	if got := graphGamma(); got != 1 {
		t.Errorf("graphGamma() = %v, want 1", got)
	}
}

func TestGraphGammaFallbackOnOutOfRange(t *testing.T) {
	for _, raw := range []string{"0", "-0.5", "1.5", "not-a-float"} {
		t.Setenv("DEX_GRAPH_GAMMA", raw)
		if got := graphGamma(); got != 0 {
			t.Errorf("graphGamma() = %v for %q, want 0 (fallback)", got, raw)
		}
	}
}

func TestGraphHopCapUnsetIsZero(t *testing.T) {
	t.Setenv("DEX_GRAPH_HOP_CAP", "")
	if got := graphHopCap(); got != 0 {
		t.Errorf("graphHopCap() = %d, want 0 (unset → store default)", got)
	}
}

func TestGraphHopCapHonoredWhenPositive(t *testing.T) {
	t.Setenv("DEX_GRAPH_HOP_CAP", "3")
	if got := graphHopCap(); got != 3 {
		t.Errorf("graphHopCap() = %d, want 3", got)
	}
}

func TestGraphHopCapFallbackOnInvalid(t *testing.T) {
	for _, raw := range []string{"0", "-2", "not-an-int"} {
		t.Setenv("DEX_GRAPH_HOP_CAP", raw)
		if got := graphHopCap(); got != 0 {
			t.Errorf("graphHopCap() = %d for %q, want 0 (fallback)", got, raw)
		}
	}
}

func TestGraphLaneWeightUnsetIsZero(t *testing.T) {
	t.Setenv("DEX_GRAPH_WEIGHT", "")
	if got := graphLaneWeight(); got != 0 {
		t.Errorf("graphLaneWeight() = %v, want 0 (unset → store default)", got)
	}
}

func TestGraphLaneWeightHonoredWhenPositive(t *testing.T) {
	t.Setenv("DEX_GRAPH_WEIGHT", "2.5")
	if got := graphLaneWeight(); got != 2.5 {
		t.Errorf("graphLaneWeight() = %v, want 2.5", got)
	}
}

func TestGraphLaneWeightFallbackOnInvalid(t *testing.T) {
	for _, raw := range []string{"0", "-1", "not-a-float"} {
		t.Setenv("DEX_GRAPH_WEIGHT", raw)
		if got := graphLaneWeight(); got != 0 {
			t.Errorf("graphLaneWeight() = %v for %q, want 0 (fallback)", got, raw)
		}
	}
}
