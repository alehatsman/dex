package mcp

import (
	"context"
	"testing"
)

// fakeProbeSurface satisfies toolSurface via an embedded nil interface (never
// dereferenced — flagRebuildIfEmpty only reaches for the indexProber facet) and
// implements indexRebuilding so we can drive the rebuild caveat directly.
type fakeProbeSurface struct {
	toolSurface
	rebuilding bool
	note       string
}

func (f fakeProbeSurface) indexRebuilding(context.Context, string) (bool, string) {
	return f.rebuilding, f.note
}

// noProbeSurface satisfies toolSurface but NOT indexProber (no indexRebuilding),
// so the type assertion in flagRebuildIfEmpty misses and it is a no-op.
type noProbeSurface struct{ toolSurface }

func TestFlagRebuildIfEmpty(t *testing.T) {
	const note = "index rebuild in progress"

	cases := []struct {
		name       string
		surface    toolSurface
		status     string
		wantCaveat bool
	}{
		{"not-found while rebuilding → caveat", fakeProbeSurface{rebuilding: true, note: note}, "not-found", true},
		{"no-graph while rebuilding → caveat", fakeProbeSurface{rebuilding: true, note: note}, "no-graph", true},
		{"no-path while rebuilding → caveat", fakeProbeSurface{rebuilding: true, note: note}, "no-path", true},
		{"not-found, index stable → untouched", fakeProbeSurface{rebuilding: false}, "not-found", false},
		{"ok while rebuilding → untouched (only empties)", fakeProbeSurface{rebuilding: true, note: note}, "ok", false},
		{"no-index while rebuilding → untouched (own signal)", fakeProbeSurface{rebuilding: true, note: note}, "no-index", false},
		{"surface without prober → no-op", noProbeSurface{}, "not-found", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := LookOutput{Status: tc.status, Trust: exactTrust()}
			flagRebuildIfEmpty(context.Background(), tc.surface, "/repo", tc.status, &out)

			gotCaveat := out.Trust.Caveat != ""
			if gotCaveat != tc.wantCaveat {
				t.Fatalf("caveat present = %v, want %v (caveat=%q)", gotCaveat, tc.wantCaveat, out.Trust.Caveat)
			}
			if tc.wantCaveat {
				if out.Trust.Fresh == nil || *out.Trust.Fresh {
					t.Errorf("Trust.Fresh = %v, want &false", out.Trust.Fresh)
				}
				if out.Trust.Provenance != "exact" {
					t.Errorf("Provenance = %q, want unchanged \"exact\"", out.Trust.Provenance)
				}
			} else if out.Trust.Fresh != nil {
				t.Errorf("Trust.Fresh = %v, want nil (untouched)", *out.Trust.Fresh)
			}
		})
	}
}
