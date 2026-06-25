package proxy

import (
	"encoding/json"
	"testing"
)

func TestParseEffortLevel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"low", "low"}, {"LOW", "low"}, {"  medium  ", "medium"},
		{"med", "medium"}, {"high", "high"}, {"", ""}, {"extreme", ""},
	}
	for _, c := range cases {
		got := string(ParseEffortLevel(c.in))
		if got != c.want {
			t.Errorf("ParseEffortLevel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestApplyEffort_Disabled(t *testing.T) {
	body := `{"model":"claude-sonnet-4-6","messages":[]}`
	out, st := ApplyEffort([]byte(body), "")
	if string(out) != body {
		t.Error("disabled: body should be unchanged")
	}
	if st.Applied || st.Reason != "disabled" {
		t.Errorf("disabled: got Applied=%v Reason=%q", st.Applied, st.Reason)
	}
}

func TestApplyEffort_NonReasoning(t *testing.T) {
	body := `{"model":"claude-haiku-4-5-20251001","messages":[]}`
	_, st := ApplyEffort([]byte(body), EffortLow)
	if st.Applied {
		t.Error("non-reasoning model should not apply effort")
	}
	if st.Reason != "non-reasoning" {
		t.Errorf("reason = %q, want non-reasoning", st.Reason)
	}
}

func TestApplyEffort_AnthropicSetsThinking(t *testing.T) {
	body := `{"model":"claude-sonnet-4-6","messages":[]}`
	out, st := ApplyEffort([]byte(body), EffortMedium)
	if !st.Applied {
		t.Fatalf("expected Applied=true, got reason=%q", st.Reason)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	thinkingRaw, ok := result["thinking"]
	if !ok {
		t.Fatal("thinking key not set")
	}
	var thinking map[string]any
	if err := json.Unmarshal(thinkingRaw, &thinking); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	if thinking["type"] != "enabled" {
		t.Errorf("thinking.type = %v, want enabled", thinking["type"])
	}
	if thinking["budget_tokens"] != float64(effortToBudgetTokens(EffortMedium)) {
		t.Errorf("budget_tokens = %v, want %d", thinking["budget_tokens"], effortToBudgetTokens(EffortMedium))
	}
}

func TestApplyEffort_AnthropicClientSet(t *testing.T) {
	body := `{"model":"claude-sonnet-4-6","thinking":{"type":"enabled","budget_tokens":100},"messages":[]}`
	out, st := ApplyEffort([]byte(body), EffortHigh)
	if st.Applied {
		t.Error("should not override client-set thinking")
	}
	if st.Reason != "client-set" {
		t.Errorf("reason = %q, want client-set", st.Reason)
	}
	if string(out) != body {
		t.Error("body should be unchanged when client already set thinking")
	}
}

func TestApplyEffort_OpenAI(t *testing.T) {
	body := `{"model":"o1-mini","messages":[]}`
	out, st := ApplyEffort([]byte(body), EffortLow)
	if !st.Applied {
		t.Fatalf("expected Applied=true, got reason=%q", st.Reason)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var effort string
	if err := json.Unmarshal(result["reasoning_effort"], &effort); err != nil {
		t.Fatalf("unmarshal reasoning_effort: %v", err)
	}
	if effort != "low" {
		t.Errorf("reasoning_effort = %q, want low", effort)
	}
}

func TestApplyEffort_OpenAIClientSet(t *testing.T) {
	body := `{"model":"o3","reasoning_effort":"high","messages":[]}`
	_, st := ApplyEffort([]byte(body), EffortLow)
	if st.Applied {
		t.Error("should not override client-set reasoning_effort")
	}
}

func TestApplyEffort_Gemini(t *testing.T) {
	body := `{"model":"gemini-2.0-flash","contents":[]}`
	out, st := ApplyEffort([]byte(body), EffortHigh)
	if !st.Applied {
		t.Fatalf("expected Applied=true, got reason=%q", st.Reason)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var gc map[string]json.RawMessage
	if err := json.Unmarshal(result["generationConfig"], &gc); err != nil {
		t.Fatalf("unmarshal generationConfig: %v", err)
	}
	if _, ok := gc["thinkingConfig"]; !ok {
		t.Error("thinkingConfig not set in generationConfig")
	}
}

func TestApplyEffort_GeminiClientSet(t *testing.T) {
	body := `{"model":"gemini-2.0-flash","generationConfig":{"thinkingConfig":{"thinkingBudget":5000}},"contents":[]}`
	_, st := ApplyEffort([]byte(body), EffortLow)
	if st.Applied {
		t.Error("should not override client-set thinkingConfig")
	}
}

func TestApplyEffort_ParseError(t *testing.T) {
	_, st := ApplyEffort([]byte(`not-json`), EffortMedium)
	if st.Applied {
		t.Error("parse error should not apply")
	}
}
