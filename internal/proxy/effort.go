package proxy

import (
	"encoding/json"
	"strings"
)

// EffortLevel is a provider-agnostic reasoning budget hint.
type EffortLevel string

const (
	EffortLow    EffortLevel = "low"
	EffortMedium EffortLevel = "medium"
	EffortHigh   EffortLevel = "high"
)

// ParseEffortLevel normalises a raw string from env/flag. Returns "" if unrecognised.
func ParseEffortLevel(s string) EffortLevel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return EffortLow
	case "medium", "med":
		return EffortMedium
	case "high":
		return EffortHigh
	default:
		return ""
	}
}

// EffortStats is the per-request outcome of ApplyEffort.
type EffortStats struct {
	Applied bool
	Reason  string // "disabled" | "no-effort" | "client-set" | "non-reasoning" | "applied"
	Effort  string // the effort level written, empty when not applied
}

// reasoningModelPrefixes are model name substrings that indicate extended-thinking support.
// Only models matching one of these get the effort rewrite.
var reasoningModelPrefixes = []string{
	"claude-3-7", "claude-3-5",
	"claude-opus-4", "claude-sonnet-4",
	"o1", "o3",
	"gemini-2",
}

func isReasoningModel(model string) bool {
	lower := strings.ToLower(model)
	for _, p := range reasoningModelPrefixes {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// ApplyEffort rewrites the reasoning-effort field in an Anthropic /v1/messages
// body according to effort. It is a no-op (fail-open) when:
//   - effort is empty (DEX_PROXY_EFFORT unset)
//   - body cannot be parsed
//   - the model is not a known reasoning model
//   - the client already set an explicit effort field (never override)
//
// The rewrite is deterministic: same effort + same request → identical bytes,
// so the provider prompt cache stays warm across turns.
func ApplyEffort(body []byte, effort EffortLevel) ([]byte, EffortStats) {
	if effort == "" {
		return body, EffortStats{Reason: "disabled"}
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body, EffortStats{Reason: "parse-error"}
	}

	model := unmarshalString(root["model"])
	if !isReasoningModel(model) {
		return body, EffortStats{Reason: "non-reasoning"}
	}

	// Detect provider from model name and apply the correct field.
	// Anthropic: thinking object under "thinking" key, or output_config.effort.
	// We write to "thinking": {"type":"enabled","budget_tokens":N} for Anthropic
	// extended-thinking, but the issue targets output_config.effort which is the
	// newer API. Check both paths: if client already set either, skip.
	if isAnthropicModel(model) {
		return applyAnthropicEffort(root, body, effort)
	}
	if isOpenAIModel(model) {
		return applyOpenAIEffort(root, body, effort)
	}
	if isGeminiModel(model) {
		return applyGeminiEffort(root, body, effort)
	}
	return body, EffortStats{Reason: "non-reasoning"}
}

func isAnthropicModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.HasPrefix(lower, "claude")
}

func isOpenAIModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.HasPrefix(lower, "o1") || strings.HasPrefix(lower, "o3") ||
		strings.HasPrefix(lower, "gpt")
}

func isGeminiModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "gemini")
}

// applyAnthropicEffort writes to "thinking": {"type":"enabled","budget_tokens":N}.
// Anthropic extended thinking is enabled by a "thinking" object; the newer
// output_config.effort field is also checked for client-set detection.
func applyAnthropicEffort(root map[string]json.RawMessage, body []byte, effort EffortLevel) ([]byte, EffortStats) {
	// Skip if client already set "thinking" or "output_config".
	if _, ok := root["thinking"]; ok {
		return body, EffortStats{Reason: "client-set"}
	}
	if _, ok := root["output_config"]; ok {
		return body, EffortStats{Reason: "client-set"}
	}

	budget := effortToBudgetTokens(effort)
	thinking := map[string]any{
		"type":          "enabled",
		"budget_tokens": budget,
	}
	enc, err := json.Marshal(thinking)
	if err != nil {
		return body, EffortStats{Reason: "marshal-error"}
	}
	root["thinking"] = enc
	out, err := json.Marshal(root)
	if err != nil {
		return body, EffortStats{Reason: "marshal-error"}
	}
	return out, EffortStats{Applied: true, Reason: "applied", Effort: string(effort)}
}

// applyOpenAIEffort writes "reasoning_effort": "low"|"medium"|"high".
func applyOpenAIEffort(root map[string]json.RawMessage, body []byte, effort EffortLevel) ([]byte, EffortStats) {
	if _, ok := root["reasoning_effort"]; ok {
		return body, EffortStats{Reason: "client-set"}
	}
	enc, err := json.Marshal(string(effort))
	if err != nil {
		return body, EffortStats{Reason: "marshal-error"}
	}
	root["reasoning_effort"] = enc
	out, err := json.Marshal(root)
	if err != nil {
		return body, EffortStats{Reason: "marshal-error"}
	}
	return out, EffortStats{Applied: true, Reason: "applied", Effort: string(effort)}
}

// applyGeminiEffort writes "generationConfig": {"thinkingConfig": {"thinkingBudget": N}}.
func applyGeminiEffort(root map[string]json.RawMessage, body []byte, effort EffortLevel) ([]byte, EffortStats) {
	// Check if generationConfig already has thinkingConfig set.
	if gcRaw, ok := root["generationConfig"]; ok {
		var gc map[string]json.RawMessage
		if json.Unmarshal(gcRaw, &gc) == nil {
			if _, ok := gc["thinkingConfig"]; ok {
				return body, EffortStats{Reason: "client-set"}
			}
		}
	}
	budget := effortToBudgetTokens(effort)
	thinkingConfig := map[string]any{"thinkingBudget": budget}
	var gc map[string]json.RawMessage
	if gcRaw, ok := root["generationConfig"]; ok {
		_ = json.Unmarshal(gcRaw, &gc)
	}
	if gc == nil {
		gc = make(map[string]json.RawMessage)
	}
	tcEnc, err := json.Marshal(thinkingConfig)
	if err != nil {
		return body, EffortStats{Reason: "marshal-error"}
	}
	gc["thinkingConfig"] = tcEnc
	gcEnc, err := json.Marshal(gc)
	if err != nil {
		return body, EffortStats{Reason: "marshal-error"}
	}
	root["generationConfig"] = gcEnc
	out, err := json.Marshal(root)
	if err != nil {
		return body, EffortStats{Reason: "marshal-error"}
	}
	return out, EffortStats{Applied: true, Reason: "applied", Effort: string(effort)}
}

// effortToBudgetTokens maps an EffortLevel to a concrete token budget.
// These are conservative defaults; users can tune via model-specific config.
func effortToBudgetTokens(e EffortLevel) int {
	switch e {
	case EffortLow:
		return 1_024
	case EffortMedium:
		return 4_096
	case EffortHigh:
		return 10_000
	default:
		return 4_096
	}
}
