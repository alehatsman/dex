package proxy

import (
	"encoding/json"
	"os"
)

// ModelPricing holds the USD cost per 1M tokens for one model variant.
type ModelPricing struct {
	InputPer1M      float64 `json:"input_per_1m"`
	OutputPer1M     float64 `json:"output_per_1m"`
	CacheReadPer1M  float64 `json:"cache_read_per_1m"`
	CacheWritePer1M float64 `json:"cache_write_per_1m"`
}

// defaultPricing covers the Claude models most commonly used with dex.
// Prices are USD per 1M tokens as of 2026-06. Override via DEX_MODEL_PRICING_JSON.
var defaultPricing = map[string]ModelPricing{
	"claude-opus-4-7":               {15.00, 75.00, 1.50, 18.75},
	"claude-sonnet-4-6":             {3.00, 15.00, 0.30, 3.75},
	"claude-haiku-4-5":              {0.80, 4.00, 0.08, 1.00},
	"claude-haiku-4-5-20251001":     {0.80, 4.00, 0.08, 1.00},
	"claude-3-5-sonnet-20241022":    {3.00, 15.00, 0.30, 3.75},
	"claude-3-5-haiku-20241022":     {0.80, 4.00, 0.08, 1.00},
	"claude-3-opus-20240229":        {15.00, 75.00, 1.50, 18.75},
}

// LoadPricing returns the effective pricing table: defaultPricing merged with any
// overrides from DEX_MODEL_PRICING_JSON (a JSON object mapping model → ModelPricing).
// Override entries replace defaults; unmentioned models keep their default prices.
// On parse error the default table is returned unchanged.
func LoadPricing() map[string]ModelPricing {
	raw := os.Getenv("DEX_MODEL_PRICING_JSON")
	if raw == "" {
		return defaultPricing
	}
	var overrides map[string]ModelPricing
	if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
		return defaultPricing
	}
	merged := make(map[string]ModelPricing, len(defaultPricing)+len(overrides))
	for k, v := range defaultPricing {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	return merged
}

// ComputeCost returns the estimated USD cost for one provider response given
// the actual token counts and the model that served the request. Returns 0
// when the model is not in the pricing table (unknown models are free to avoid
// false positives).
func ComputeCost(u ProviderUsage, model string, pricing map[string]ModelPricing) float64 {
	p, ok := pricing[model]
	if !ok {
		return 0
	}
	const perM = 1_000_000.0
	return float64(u.InputTokens)*p.InputPer1M/perM +
		float64(u.OutputTokens)*p.OutputPer1M/perM +
		float64(u.CacheReadTokens)*p.CacheReadPer1M/perM +
		float64(u.CacheWriteTokens)*p.CacheWritePer1M/perM
}
