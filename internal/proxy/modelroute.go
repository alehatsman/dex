package proxy

import "encoding/json"

// ModelRouteConfig is the token-count routing table. Zero value = disabled.
type ModelRouteConfig struct {
	Enabled      bool
	LowThreshold int    // input tokens < this → LowModel
	LowModel     string // e.g. "claude-haiku-4-5-20251001"
	MidThreshold int    // input tokens < this → MidModel
	MidModel     string // e.g. "claude-sonnet-4-6"
	// above MidThreshold → requested model passes through unchanged
}

// ModelRouteStats is the per-request outcome of RouteModel.
type ModelRouteStats struct {
	Applied        bool
	RequestedModel string
	RoutedModel    string
	Reason         string // "low-tokens" | "mid-tokens" | "pass-through" | "disabled"
}

// RouteModel rewrites the "model" field in body based on inputTokens and cfg.
// Fail-open: any parse error returns the original body unchanged with Applied:false.
func RouteModel(body []byte, inputTokens int, cfg ModelRouteConfig) ([]byte, ModelRouteStats) {
	if !cfg.Enabled || len(body) == 0 {
		return body, ModelRouteStats{Reason: "disabled"}
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body, ModelRouteStats{Reason: "parse-error"}
	}
	requested := unmarshalString(root["model"])

	target, reason := selectModel(inputTokens, cfg)
	if target == "" || target == requested {
		return body, ModelRouteStats{RequestedModel: requested, RoutedModel: requested, Reason: reason}
	}
	enc, err := json.Marshal(target)
	if err != nil {
		return body, ModelRouteStats{RequestedModel: requested, RoutedModel: requested, Reason: "marshal-error"}
	}
	root["model"] = enc
	out, err := json.Marshal(root)
	if err != nil {
		return body, ModelRouteStats{RequestedModel: requested, RoutedModel: requested, Reason: "marshal-error"}
	}
	return out, ModelRouteStats{
		Applied:        true,
		RequestedModel: requested,
		RoutedModel:    target,
		Reason:         reason,
	}
}

func selectModel(inputTokens int, cfg ModelRouteConfig) (string, string) {
	if cfg.LowModel != "" && cfg.LowThreshold > 0 && inputTokens < cfg.LowThreshold {
		return cfg.LowModel, "low-tokens"
	}
	if cfg.MidModel != "" && cfg.MidThreshold > 0 && inputTokens < cfg.MidThreshold {
		return cfg.MidModel, "mid-tokens"
	}
	return "", "pass-through"
}
