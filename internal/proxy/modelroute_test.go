package proxy

import (
	"encoding/json"
	"testing"
)

func TestRouteModel(t *testing.T) {
	cfg := ModelRouteConfig{
		Enabled:      true,
		LowThreshold: 2000,
		LowModel:     "claude-haiku-4-5-20251001",
		MidThreshold: 20000,
		MidModel:     "claude-sonnet-4-6",
	}

	body := func(model string) []byte {
		b, _ := json.Marshal(map[string]any{"model": model, "messages": []any{}})
		return b
	}
	readModel := func(b []byte) string {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(b, &m)
		return unmarshalString(m["model"])
	}

	tests := []struct {
		name        string
		tokens      int
		requested   string
		wantModel   string
		wantApplied bool
		wantReason  string
	}{
		{"below low threshold", 500, "claude-opus-4-7", "claude-haiku-4-5-20251001", true, "low-tokens"},
		{"at low threshold", 2000, "claude-opus-4-7", "claude-sonnet-4-6", true, "mid-tokens"},
		{"between thresholds", 5000, "claude-opus-4-7", "claude-sonnet-4-6", true, "mid-tokens"},
		{"at mid threshold", 20000, "claude-opus-4-7", "claude-opus-4-7", false, "pass-through"},
		{"above mid threshold", 50000, "claude-opus-4-7", "claude-opus-4-7", false, "pass-through"},
		{"already low model", 500, "claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", false, "low-tokens"},
		{"disabled", 500, "claude-opus-4-7", "claude-opus-4-7", false, "disabled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cfg
			if tt.wantReason == "disabled" {
				c = ModelRouteConfig{}
			}
			out, stats := RouteModel(body(tt.requested), tt.tokens, c)
			if stats.Applied != tt.wantApplied {
				t.Errorf("Applied=%v want %v", stats.Applied, tt.wantApplied)
			}
			if stats.Reason != tt.wantReason {
				t.Errorf("Reason=%q want %q", stats.Reason, tt.wantReason)
			}
			if got := readModel(out); got != tt.wantModel {
				t.Errorf("model=%q want %q", got, tt.wantModel)
			}
		})
	}
}

func TestRouteModelFailOpen(t *testing.T) {
	cfg := ModelRouteConfig{Enabled: true, LowThreshold: 100, LowModel: "claude-haiku-4-5-20251001"}
	out, stats := RouteModel([]byte("not json"), 50, cfg)
	if stats.Applied {
		t.Error("should not apply on parse error")
	}
	if string(out) != "not json" {
		t.Error("should return original body on error")
	}
}
