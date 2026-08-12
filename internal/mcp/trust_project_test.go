package mcp

import (
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/retrieve"
)

// A zero Trust envelope projects to nil so empty responses stay byte-neutral
// (the wire field is omitempty).
func TestFromPackTrustZeroIsNil(t *testing.T) {
	if got := fromPackTrust(retrieve.Trust{}); got != nil {
		t.Fatalf("zero Trust must project to nil, got %+v", got)
	}
}

// A populated envelope carries every field through onto the unified EnvTrust
// (#110 step 2): provenance is "semantic", the two freshness booleans fold into
// fresh, evidence signals pass through, IndexedAt renders RFC3339 UTC.
func TestFromPackTrustPopulated(t *testing.T) {
	at := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	got := fromPackTrust(retrieve.Trust{
		Stale:         true,
		Indexing:      true,
		IndexedAt:     at,
		Confidence:    "high",
		TopScore:      0.83,
		LowConf:       true,
		GraphResolved: true,
		RecallPartial: true,
		Caveat:        retrieve.RecallCaveat,
	})
	if got == nil {
		t.Fatal("populated Trust must project to a non-nil envelope")
	}
	if got.Provenance != "semantic" {
		t.Fatalf("Provenance=%q, want semantic", got.Provenance)
	}
	if got.Fresh == nil || *got.Fresh {
		t.Fatalf("stale+indexing must fold to fresh=false, got %+v", got.Fresh)
	}
	if !got.LowConfidence || !got.GraphResolved || !got.RecallPartial {
		t.Fatalf("evidence bool fields dropped: %+v", got)
	}
	if got.Confidence != "high" {
		t.Fatalf("Confidence=%q, want high", got.Confidence)
	}
	if got.TopScore != 0.83 {
		t.Fatalf("TopScore=%v, want 0.83", got.TopScore)
	}
	if got.Caveat != retrieve.RecallCaveat {
		t.Fatalf("Caveat=%q, want the recall caveat", got.Caveat)
	}
	if got.IndexedAt != "2026-08-05T12:00:00Z" {
		t.Fatalf("IndexedAt=%q, want RFC3339 UTC", got.IndexedAt)
	}
}

// An indexing-in-flight Trust with no explicit caveat gets the build-in-progress
// caveat so the folded freshness state stays visible.
func TestFromPackTrustIndexingCaveat(t *testing.T) {
	got := fromPackTrust(retrieve.Trust{Indexing: true})
	if got == nil || got.Fresh == nil || *got.Fresh {
		t.Fatalf("indexing Trust must project with fresh=false, got %+v", got)
	}
	if got.Caveat == "" {
		t.Fatal("indexing-in-flight must surface a caveat")
	}
}

// Freshness alone (no evidence signals) still projects — a stale index is worth
// surfacing even on an otherwise empty envelope.
func TestFromPackTrustFreshnessOnly(t *testing.T) {
	got := fromPackTrust(retrieve.Trust{Stale: true})
	if got == nil || got.Fresh == nil || *got.Fresh {
		t.Fatalf("freshness-only Trust must project with fresh=false, got %+v", got)
	}
	if got.IndexedAt != "" {
		t.Fatalf("zero IndexedAt must stay empty, got %q", got.IndexedAt)
	}
}
