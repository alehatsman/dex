package store

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// calibrationYAML is the checked-in, tuned retrieval-fusion default set. It is
// embedded at build time so the binary is self-contained — nothing is read
// from disk at runtime. Regenerate it with `dex eval --alpha-sweep
// --emit-calibration`; treat the values as data, not source.
//
//go:embed calibration.yml
var calibrationYAML []byte

// Calibration holds the tuned retrieval-fusion defaults. The fields mirror
// calibration.yml. Env vars (read at the cmd layer) override these per
// process; this artifact replaces what used to be hard-coded literals.
type Calibration struct {
	FusionMode      string  `yaml:"fusion_mode"`       // "linear" | "rrf"
	FusionAlpha     float32 `yaml:"fusion_alpha"`      // dense weight for linear fusion, (0,1]
	RRFK            int     `yaml:"rrf_k"`             // RRF rank constant
	GraphGamma      float32 `yaml:"graph_gamma"`       // per-hop decay, (0,1]
	GraphLaneWeight float32 `yaml:"graph_lane_weight"` // flat multiplier on the graph lane
	GraphHopCap     int     `yaml:"graph_hop_cap"`     // spreading-activation depth
	RerankPool      int     `yaml:"rerank_pool"`       // 0 = no cap

	Provenance struct {
		Source string `yaml:"source"`
		Date   string `yaml:"date"`
		Metric string `yaml:"metric"`
	} `yaml:"provenance"`
}

// builtinCalibration is the compiled safety net used when the embedded
// artifact is unparseable or carries an out-of-range value (a repo/build bug,
// not a runtime condition). It mirrors the shipped calibration.yml so the
// binary degrades to a known-good config rather than a zero value.
var builtinCalibration = Calibration{
	FusionMode:      "linear",
	FusionAlpha:     0.7,
	RRFK:            60,
	GraphGamma:      0.6,
	GraphLaneWeight: 1.0,
	GraphHopCap:     4,
	RerankPool:      0,
}

var (
	calibOnce sync.Once
	calib     Calibration
)

// CalibratedDefaults returns the embedded calibration artifact, parsed once.
// On a parse error or an out-of-range field it falls back to builtinCalibration
// (per field), so callers always receive a usable config.
func CalibratedDefaults() Calibration {
	calibOnce.Do(func() { calib = loadCalibration(calibrationYAML) })
	return calib
}

// loadCalibration parses the artifact and clamps each field to the built-in
// fallback when it is missing or nonsensical. Split out from CalibratedDefaults
// so it is unit-testable with arbitrary input.
func loadCalibration(raw []byte) Calibration {
	c := builtinCalibration
	var parsed Calibration
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "warning: calibration.yml unparseable (%v); using built-in defaults\n", err)
		return c
	}
	c = parsed
	if _, ok := ParseFusionMode(c.FusionMode); !ok {
		c.FusionMode = builtinCalibration.FusionMode
	}
	if c.FusionAlpha <= 0 || c.FusionAlpha > 1 {
		c.FusionAlpha = builtinCalibration.FusionAlpha
	}
	if c.RRFK <= 0 {
		c.RRFK = builtinCalibration.RRFK
	}
	if c.GraphGamma <= 0 || c.GraphGamma > 1 {
		c.GraphGamma = builtinCalibration.GraphGamma
	}
	if c.GraphLaneWeight <= 0 {
		c.GraphLaneWeight = builtinCalibration.GraphLaneWeight
	}
	if c.GraphHopCap <= 0 {
		c.GraphHopCap = builtinCalibration.GraphHopCap
	}
	if c.RerankPool < 0 {
		c.RerankPool = builtinCalibration.RerankPool
	}
	return c
}

// ParseFusionMode maps a calibration/env string to a FusionMode. ok is false
// for an unrecognized value so callers can warn and fall back.
func ParseFusionMode(s string) (FusionMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "linear":
		return FusionLinear, true
	case "rrf":
		return FusionRRF, true
	default:
		return FusionRRF, false
	}
}
