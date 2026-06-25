package proxy

import (
	"math"
	"os"
	"testing"
)

func TestComputeCost_KnownModel(t *testing.T) {
	pricing := map[string]ModelPricing{
		"claude-sonnet-4-6": {3.00, 15.00, 0.30, 3.75},
	}
	u := ProviderUsage{
		InputTokens:      1_000_000,
		OutputTokens:     100_000,
		CacheReadTokens:  500_000,
		CacheWriteTokens: 50_000,
	}
	// 1M*3.00/1M + 100k*15.00/1M + 500k*0.30/1M + 50k*3.75/1M
	// = 3.00 + 1.50 + 0.15 + 0.1875 = 4.8375
	got := ComputeCost(u, "claude-sonnet-4-6", pricing)
	want := 4.8375
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("ComputeCost = %v, want %v", got, want)
	}
}

func TestComputeCost_UnknownModel(t *testing.T) {
	got := ComputeCost(ProviderUsage{InputTokens: 1000}, "unknown-model", defaultPricing)
	if got != 0 {
		t.Errorf("unknown model: want 0, got %v", got)
	}
}

func TestComputeCost_ZeroTokens(t *testing.T) {
	got := ComputeCost(ProviderUsage{}, "claude-sonnet-4-6", defaultPricing)
	if got != 0 {
		t.Errorf("zero tokens: want 0, got %v", got)
	}
}

func TestLoadPricing_Default(t *testing.T) {
	os.Unsetenv("DEX_MODEL_PRICING_JSON")
	p := LoadPricing()
	if _, ok := p["claude-sonnet-4-6"]; !ok {
		t.Error("default pricing missing claude-sonnet-4-6")
	}
}

func TestLoadPricing_Override(t *testing.T) {
	t.Setenv("DEX_MODEL_PRICING_JSON", `{"my-model":{"input_per_1m":1.0,"output_per_1m":2.0}}`)
	p := LoadPricing()
	if _, ok := p["my-model"]; !ok {
		t.Error("override pricing missing my-model")
	}
	if _, ok := p["claude-sonnet-4-6"]; !ok {
		t.Error("default entry missing after override merge")
	}
}

func TestLoadPricing_BadJSON(t *testing.T) {
	t.Setenv("DEX_MODEL_PRICING_JSON", `not-json`)
	p := LoadPricing()
	// Falls back to defaults on parse error.
	if _, ok := p["claude-sonnet-4-6"]; !ok {
		t.Error("bad JSON should fall back to defaults")
	}
}

func TestStatsRecordCost(t *testing.T) {
	var s Stats
	s.recordCost(1.5)
	s.recordCost(0.5)
	snap := s.Snapshot()
	if math.Abs(snap.SessionCostUSD-2.0) > 1e-6 {
		t.Errorf("SessionCostUSD = %v, want 2.0", snap.SessionCostUSD)
	}
}

func TestStatsRecordCost_Negative(t *testing.T) {
	var s Stats
	s.recordCost(-1.0) // should be a no-op
	if s.SessionCostMicroUSD.Load() != 0 {
		t.Error("negative cost should not be recorded")
	}
}
