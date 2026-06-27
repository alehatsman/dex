package main

import (
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/output"
	"github.com/alehatsman/dex/internal/store"
)

func TestBuildFindEnvelope(t *testing.T) {
	hits := []store.Hit{
		{Path: "internal/store/store.go", Name: "(*Store).Search", Kind: "method", StartLine: 10, EndLine: 40},
		{Path: "internal/mcp/server.go", Name: "Run", Kind: "func", StartLine: 5, EndLine: 9},
	}
	env := buildFindEnvelope(hits, time.Now().Add(-time.Hour))
	if err := env.Validate(); err != nil {
		t.Fatalf("find envelope invalid: %v", err)
	}
	if env.Confidence.Level != output.LevelHigh {
		t.Fatalf("results present should be high confidence, got %q", env.Confidence.Level)
	}
	if len(env.Evidence) != len(hits) {
		t.Fatalf("evidence should mirror hits: got %d want %d", len(env.Evidence), len(hits))
	}
	if env.Evidence[0].Path != hits[0].Path || env.Evidence[0].Start != 10 {
		t.Fatalf("evidence span mismatch: %+v", env.Evidence[0])
	}
	if env.Stale.Coverage != output.CoverageAgeOnly {
		t.Fatalf("find with an index should be age_only, got %q", env.Stale.Coverage)
	}
	if len(env.NextCalls) == 0 || env.NextCalls[0].Tool != "read" {
		t.Fatalf("find should suggest reading the top hit, got %+v", env.NextCalls)
	}
}

func TestBuildFindEnvelopeNoHits(t *testing.T) {
	env := buildFindEnvelope(nil, time.Time{})
	if err := env.Validate(); err != nil {
		t.Fatalf("empty find envelope invalid: %v", err)
	}
	if env.Evidence == nil || len(env.Evidence) != 0 {
		t.Fatalf("no hits must yield empty (non-nil) evidence, got %+v", env.Evidence)
	}
	if env.Stale.Coverage != output.CoverageUnknown {
		t.Fatalf("no index time should be unknown coverage, got %q", env.Stale.Coverage)
	}
}

func TestBuildReadEnvelope(t *testing.T) {
	t.Run("full range exact", func(t *testing.T) {
		env := buildReadEnvelope("foo.go", "full", 0, 0, 120)
		if err := env.Validate(); err != nil {
			t.Fatalf("read envelope invalid: %v", err)
		}
		if env.Confidence.Level != output.LevelHigh {
			t.Fatalf("full read should be high confidence, got %q", env.Confidence.Level)
		}
		if len(env.Evidence) != 1 || env.Evidence[0].Start != 1 || env.Evidence[0].End != 120 {
			t.Fatalf("full read span should cover whole file: %+v", env.Evidence)
		}
		if env.Stale.Coverage != output.CoverageUnknown {
			t.Fatalf("read serves live bytes, coverage should be unknown, got %q", env.Stale.Coverage)
		}
	})
	t.Run("compressed mode degrades confidence", func(t *testing.T) {
		env := buildReadEnvelope("foo.go", "signatures", 0, 0, 80)
		if err := env.Validate(); err != nil {
			t.Fatalf("read envelope invalid: %v", err)
		}
		if env.Confidence.Level != output.LevelMedium {
			t.Fatalf("signatures mode should be medium confidence, got %q", env.Confidence.Level)
		}
		if len(env.Confidence.Gaps) == 0 {
			t.Fatal("compressed mode must record a gap explaining the elision")
		}
		if len(env.NextCalls) == 0 {
			t.Fatal("compressed mode should suggest a full read")
		}
	})
}
