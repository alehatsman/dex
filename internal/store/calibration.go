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

// FusionModeString is the inverse of ParseFusionMode — the canonical lowercase
// token for a FusionMode, suitable for writing back into the calibration artifact.
func FusionModeString(m FusionMode) string {
	if m == FusionLinear {
		return "linear"
	}
	return "rrf"
}

// calibrationHeader is prepended to a regenerated artifact so the instructional
// preamble survives a `--emit-calibration` rewrite (yaml.Marshal drops comments).
const calibrationHeader = `# Calibrated retrieval-fusion defaults.
#
# These values are DATA, not source — the product of ` + "`dex eval --alpha-sweep`" + `
# runs over the retrieval corpus. Regenerate with
#   dex eval --alpha-sweep --emit-calibration
# and commit the diff; do not hand-edit values without a corresponding eval run.
#
# Env vars still override per-process: DEX_FUSION_MODE, DEX_FUSION_ALPHA,
# DEX_GRAPH_GAMMA, DEX_GRAPH_HOP_CAP, DEX_GRAPH_WEIGHT, DEX_RERANK_POOL.
# Precedence: env  >  this artifact  >  built-in fallback.

`

// MarshalCalibration renders a Calibration as the artifact bytes (instructional
// header + YAML). It is the single writer used by ` + "`dex eval --emit-calibration`" + `
// so regeneration stays in lockstep with the parse path.
func MarshalCalibration(c Calibration) ([]byte, error) {
	body, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	return append([]byte(calibrationHeader), body...), nil
}
