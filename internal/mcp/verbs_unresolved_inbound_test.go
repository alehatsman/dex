package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

// fakeInbound is a toolSurface (via the noopSurface stubs) that also answers the
// optional unresolvedInbounder capability from a fixed table, so foldUnresolved-
// Inbound can be exercised without a real store.
type fakeInbound struct {
	noopSurface
	rows map[string][]store.UnresolvedInbound
}

func (f *fakeInbound) unresolvedInbound(_ context.Context, _, file string, _ int) ([]store.UnresolvedInbound, error) {
	return f.rows[file], nil
}

func TestFoldUnresolvedInbound(t *testing.T) {
	h := &fakeInbound{rows: map[string][]store.UnresolvedInbound{
		"packages/bright-common/src/UuidCodec.ts": {
			{Specifier: "@bright/common/Uuid", Count: 5},
		},
		"packages/bright-common/src/Other.ts": {
			{Specifier: "@bright/common/Uuid", Count: 4}, // merges with above -> 9
			{Specifier: "@bright/common/Thing", Count: 1},
		},
	}}

	t.Run("populates, merges, sorts", func(t *testing.T) {
		out := &TraceOutput{Targets: []TargetMatch{
			{Path: "packages/bright-common/src/UuidCodec.ts"},
			{Path: "packages/bright-common/src/Other.ts"},
			{Path: "some/thing.go"}, // Go target: skipped
		}}
		foldUnresolvedInbound(context.Background(), h, "", out)

		want := []store.UnresolvedInbound{
			{Specifier: "@bright/common/Uuid", Count: 9},
			{Specifier: "@bright/common/Thing", Count: 1},
		}
		if len(out.UnresolvedInbound) != len(want) {
			t.Fatalf("got %v, want %v", out.UnresolvedInbound, want)
		}
		for i := range want {
			if out.UnresolvedInbound[i] != want[i] {
				t.Errorf("row %d = %+v, want %+v", i, out.UnresolvedInbound[i], want[i])
			}
		}
		if out.Recall != "partial" {
			t.Errorf("Recall = %q, want partial", out.Recall)
		}
		if !strings.Contains(out.Hint, "@bright/common/Uuid") || !strings.Contains(out.Hint, "grep") {
			t.Errorf("hint missing specifier/grep cue: %q", out.Hint)
		}
	})

	t.Run("go-only targets are a no-op", func(t *testing.T) {
		out := &TraceOutput{Targets: []TargetMatch{{Path: "pkg/x.go"}}}
		foldUnresolvedInbound(context.Background(), h, "", out)
		if out.UnresolvedInbound != nil || out.Hint != "" {
			t.Errorf("go-only should no-op, got %+v / %q", out.UnresolvedInbound, out.Hint)
		}
	})

	t.Run("surface without the capability is a no-op", func(t *testing.T) {
		out := &TraceOutput{Targets: []TargetMatch{{Path: "packages/bright-common/src/UuidCodec.ts"}}}
		foldUnresolvedInbound(context.Background(), &noopSurface{}, "", out)
		if out.UnresolvedInbound != nil || out.Hint != "" {
			t.Errorf("uncapable surface should skip, got %+v / %q", out.UnresolvedInbound, out.Hint)
		}
	})
}
