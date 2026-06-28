package main

import (
	"context"
	"strings"
	"testing"
)

// TestSearchRejectsNonPositiveK locks issue #523: `search` must reject --k<=0
// with a clear error instead of silently coercing it back to the default.
// Validation runs before index resolution, so it needs no index. (lookup was
// dropped in #685 — search subsumes exact symbol lookup.)
func TestSearchRejectsNonPositiveK(t *testing.T) {
	ctx := context.Background()
	cmds := map[string]func(context.Context, []string) error{
		"search": cmdSearchSemantic,
	}
	for name, fn := range cmds {
		for _, k := range []string{"0", "-1", "-8"} {
			t.Run(name+"/k="+k, func(t *testing.T) {
				err := fn(ctx, []string{"--k=" + k, "some query"})
				if err == nil {
					t.Fatalf("%s --k=%s = nil, want error", name, k)
				}
				if !strings.Contains(err.Error(), "invalid --k") {
					t.Errorf("error = %q, want it to contain %q", err.Error(), "invalid --k")
				}
			})
		}
	}
}

// TestSearchAcceptsPositiveK confirms a positive --k passes validation: it must
// fail later (no index in this temp dir), never with an "invalid --k" message.
func TestSearchAcceptsPositiveK(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Chdir(dir)

	cmds := map[string]func(context.Context, []string) error{
		"search": cmdSearchSemantic,
	}
	for name, fn := range cmds {
		t.Run(name, func(t *testing.T) {
			err := fn(ctx, []string{"--k=5", "some query"})
			if err != nil && strings.Contains(err.Error(), "invalid --k") {
				t.Errorf("%s rejected valid --k=5: %v", name, err)
			}
		})
	}
}
