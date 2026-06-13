package store

import "testing"

// TestCalibratedDefaultsMatchesArtifact pins the embedded artifact's values so
// a regeneration (or accidental edit) is a visible, reviewed change.
func TestCalibratedDefaultsMatchesArtifact(t *testing.T) {
	c := CalibratedDefaults()
	if c.FusionMode != "linear" {
		t.Errorf("FusionMode = %q, want linear", c.FusionMode)
	}
	if c.FusionAlpha != 0.7 {
		t.Errorf("FusionAlpha = %v, want 0.7", c.FusionAlpha)
	}
	if c.RRFK != 60 {
		t.Errorf("RRFK = %d, want 60", c.RRFK)
	}
	if c.GraphHopCap != 4 {
		t.Errorf("GraphHopCap = %d, want 4", c.GraphHopCap)
	}
}

// TestCalibratedDefaultsWireThrough confirms the package vars that consume the
// artifact actually reflect it (guards against a future decoupling).
func TestCalibratedDefaultsWireThrough(t *testing.T) {
	c := CalibratedDefaults()
	if rrfK != c.RRFK {
		t.Errorf("rrfK var = %d, calibration = %d", rrfK, c.RRFK)
	}
	if defaultGraphGamma != c.GraphGamma {
		t.Errorf("defaultGraphGamma = %v, calibration = %v", defaultGraphGamma, c.GraphGamma)
	}
	if defaultGraphLaneWeight != c.GraphLaneWeight {
		t.Errorf("defaultGraphLaneWeight = %v, calibration = %v", defaultGraphLaneWeight, c.GraphLaneWeight)
	}
	if defaultGraphHopCap != c.GraphHopCap {
		t.Errorf("defaultGraphHopCap = %d, calibration = %d", defaultGraphHopCap, c.GraphHopCap)
	}
}

func TestLoadCalibrationFallsBackOnGarbage(t *testing.T) {
	got := loadCalibration([]byte("::: not yaml :::"))
	if got != builtinCalibration {
		t.Errorf("garbage input: got %+v, want builtin %+v", got, builtinCalibration)
	}
}

func TestLoadCalibrationClampsOutOfRange(t *testing.T) {
	raw := []byte(`
fusion_mode: nonsense
fusion_alpha: 5.0
rrf_k: -3
graph_gamma: 0
graph_lane_weight: -1
graph_hop_cap: 0
rerank_pool: -10
`)
	got := loadCalibration(raw)
	if got.FusionMode != builtinCalibration.FusionMode {
		t.Errorf("FusionMode = %q, want clamped %q", got.FusionMode, builtinCalibration.FusionMode)
	}
	if got.FusionAlpha != builtinCalibration.FusionAlpha {
		t.Errorf("FusionAlpha = %v, want clamped %v", got.FusionAlpha, builtinCalibration.FusionAlpha)
	}
	if got.RRFK != builtinCalibration.RRFK {
		t.Errorf("RRFK = %d, want clamped %d", got.RRFK, builtinCalibration.RRFK)
	}
	if got.GraphGamma != builtinCalibration.GraphGamma {
		t.Errorf("GraphGamma = %v, want clamped %v", got.GraphGamma, builtinCalibration.GraphGamma)
	}
	if got.GraphLaneWeight != builtinCalibration.GraphLaneWeight {
		t.Errorf("GraphLaneWeight = %v, want clamped %v", got.GraphLaneWeight, builtinCalibration.GraphLaneWeight)
	}
	if got.GraphHopCap != builtinCalibration.GraphHopCap {
		t.Errorf("GraphHopCap = %d, want clamped %d", got.GraphHopCap, builtinCalibration.GraphHopCap)
	}
	if got.RerankPool != builtinCalibration.RerankPool {
		t.Errorf("RerankPool = %d, want clamped %d", got.RerankPool, builtinCalibration.RerankPool)
	}
}

// TestMarshalCalibrationRoundTrips guards the regen path: bytes written by
// `dex eval --emit-calibration` must parse back to the same config.
func TestMarshalCalibrationRoundTrips(t *testing.T) {
	in := builtinCalibration
	in.FusionMode = "rrf"
	in.FusionAlpha = 0.5
	in.Provenance.Source = "test"
	raw, err := MarshalCalibration(in)
	if err != nil {
		t.Fatalf("MarshalCalibration: %v", err)
	}
	got := loadCalibration(raw)
	if got != in {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, in)
	}
}

func TestFusionModeStringRoundTrips(t *testing.T) {
	for _, m := range []FusionMode{FusionLinear, FusionRRF} {
		got, ok := ParseFusionMode(FusionModeString(m))
		if !ok || got != m {
			t.Errorf("FusionModeString/ParseFusionMode round-trip for %v: got (%v,%v)", m, got, ok)
		}
	}
}

func TestParseFusionMode(t *testing.T) {
	cases := []struct {
		in   string
		want FusionMode
		ok   bool
	}{
		{"linear", FusionLinear, true},
		{"RRF", FusionRRF, true},
		{" Linear ", FusionLinear, true},
		{"", FusionRRF, false},
		{"bogus", FusionRRF, false},
	}
	for _, tc := range cases {
		got, ok := ParseFusionMode(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseFusionMode(%q) = (%v,%v), want (%v,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
