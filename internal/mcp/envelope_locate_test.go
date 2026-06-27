package mcp

import (
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/output"
)

func TestFinalizeLocateEnvelope(t *testing.T) {
	t.Run("ok resolves to a high-confidence span", func(t *testing.T) {
		out := &LocateOutput{
			Status: "ok", Path: "internal/mcp/server.go", Symbol: "Run",
			Kind: "func", StartLine: 10, EndLine: 30, Risk: "High",
		}
		finalizeLocateEnvelope(out, time.Now().Add(-time.Hour))
		if err := out.Envelope.Validate(); err != nil {
			t.Fatalf("ok envelope invalid: %v", err)
		}
		if out.Confidence.Level != output.LevelHigh {
			t.Fatalf("resolved symbol should be high confidence, got %q", out.Confidence.Level)
		}
		if len(out.Evidence) != 1 || out.Evidence[0].Symbol != "Run" || out.Evidence[0].Start != 10 {
			t.Fatalf("evidence span mismatch: %+v", out.Evidence)
		}
		if len(out.RiskFlags) != 1 {
			t.Fatalf("Risk should surface as a risk flag, got %+v", out.RiskFlags)
		}
		if out.Stale.Coverage != output.CoverageAgeOnly {
			t.Fatalf("locate over an index should be age_only, got %q", out.Stale.Coverage)
		}
	})

	t.Run("not-found is a valid low-confidence envelope", func(t *testing.T) {
		out := &LocateOutput{Status: "not-found", Hint: "no symbol matched"}
		finalizeLocateEnvelope(out, time.Time{})
		if err := out.Envelope.Validate(); err != nil {
			t.Fatalf("not-found envelope invalid: %v", err)
		}
		if out.Confidence.Level != output.LevelLow {
			t.Fatalf("not-found should be low confidence, got %q", out.Confidence.Level)
		}
		if len(out.Evidence) != 0 {
			t.Fatalf("not-found must not fabricate a span, got %+v", out.Evidence)
		}
		if len(out.Confidence.Gaps) == 0 {
			t.Fatal("not-found should record the hint as a gap")
		}
	})
}
