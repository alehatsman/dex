package mcp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// captureAroundSurface embeds *Server — inheriting the full toolSurface — and
// overrides the three call-graph backends mapAround drives (graphCallers,
// graphCallees, graphDiff) with canned data, recording each input. The
// embedded *Server satisfies the methods mapAround never touches. Mirrors
// captureCommSurface in verbs_map_test.go.
type captureAroundSurface struct {
	*Server
	callers, callees CallEdgeOutput
	diff             DiffOutput

	gotCallers, gotCallees CallEdgeInput
	gotDiff                DiffInput
}

func (f *captureAroundSurface) graphCallers(_ context.Context, _ *sdk.CallToolRequest, in CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error) {
	f.gotCallers = in
	return nil, f.callers, nil
}

func (f *captureAroundSurface) graphCallees(_ context.Context, _ *sdk.CallToolRequest, in CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error) {
	f.gotCallees = in
	return nil, f.callees, nil
}

func (f *captureAroundSurface) graphDiff(_ context.Context, _ *sdk.CallToolRequest, in DiffInput) (*sdk.CallToolResult, DiffOutput, error) {
	f.gotDiff = in
	return nil, f.diff, nil
}

// TestMapAroundQuery covers the --around <query> region: the seed's callers ∪
// callees rendered as one map, default neighbor budget, hard-status surfacing,
// the not-found seed, and dedup across the two lanes.
func TestMapAroundQuery(t *testing.T) {
	ctx := context.Background()

	t.Run("renders callers and callees as one region", func(t *testing.T) {
		fake := &captureAroundSurface{
			Server: stubServer(t),
			callers: CallEdgeOutput{
				Status:  "ok",
				Targets: []TargetMatch{{QualifiedName: "pkg.Seed", Kind: "func", Package: "pkg", Path: "pkg/seed.go", StartLine: 10}},
				Hits:    []CallSite{{QualifiedName: "pkg.CallerA", Kind: "func", Package: "pkg", Path: "pkg/a.go", StartLine: 3}},
			},
			callees: CallEdgeOutput{
				Status: "ok",
				Hits:   []CallSite{{QualifiedName: "pkg.CalleeB", Kind: "func", Package: "pkg", Path: "pkg/b.go", StartLine: 7}},
			},
		}
		_, out, err := mapVerb(ctx, fake, nil, MapInput{Around: "Seed"})
		if err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if out.Status != "ok" || out.Zoom != "around" {
			t.Fatalf("out = {%q, zoom %q}, want {ok, around}", out.Status, out.Zoom)
		}
		// The renderer lists each symbol by file:line, so assert on the unique
		// paths rather than the qualified names.
		for _, want := range []string{"pkg/seed.go:10", "pkg/a.go:3", "pkg/b.go:7"} {
			if !strings.Contains(out.Map, want) {
				t.Errorf("map missing %q\n%s", want, out.Map)
			}
		}
		if fake.gotCallers.Name != "Seed" || fake.gotCallees.Name != "Seed" {
			t.Errorf("seed name = callers %q / callees %q, want Seed", fake.gotCallers.Name, fake.gotCallees.Name)
		}
		if fake.gotCallers.K != aroundNeighborK {
			t.Errorf("default K = %d, want %d", fake.gotCallers.K, aroundNeighborK)
		}
	})

	t.Run("explicit K passes through", func(t *testing.T) {
		fake := &captureAroundSurface{Server: stubServer(t), callers: CallEdgeOutput{Status: "ok"}, callees: CallEdgeOutput{Status: "ok"}}
		if _, _, err := mapVerb(ctx, fake, nil, MapInput{Around: "Seed", K: 8}); err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if fake.gotCallers.K != 8 || fake.gotCallees.K != 8 {
			t.Errorf("K = callers %d / callees %d, want 8", fake.gotCallers.K, fake.gotCallees.K)
		}
	})

	t.Run("both not-found is not-found", func(t *testing.T) {
		fake := &captureAroundSurface{
			Server:  stubServer(t),
			callers: CallEdgeOutput{Status: "not-found"},
			callees: CallEdgeOutput{Status: "not-found"},
		}
		_, out, err := mapVerb(ctx, fake, nil, MapInput{Around: "Ghost"})
		if err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if out.Status != "not-found" {
			t.Errorf("status = %q, want not-found", out.Status)
		}
	})

	t.Run("hard backend status is surfaced", func(t *testing.T) {
		fake := &captureAroundSurface{
			Server:  stubServer(t),
			callers: CallEdgeOutput{Status: "no-index", Hint: "run dex index"},
			callees: CallEdgeOutput{Status: "not-found"},
		}
		_, out, err := mapVerb(ctx, fake, nil, MapInput{Around: "Seed"})
		if err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if out.Status != "no-index" || out.Hint != "run dex index" {
			t.Errorf("out = {%q, %q}, want {no-index, run dex index}", out.Status, out.Hint)
		}
	})

	t.Run("a symbol in both lanes appears once", func(t *testing.T) {
		dup := CallSite{QualifiedName: "pkg.Shared", Kind: "func", Package: "pkg", Path: "pkg/s.go", StartLine: 1}
		fake := &captureAroundSurface{
			Server:  stubServer(t),
			callers: CallEdgeOutput{Status: "ok", Hits: []CallSite{dup}},
			callees: CallEdgeOutput{Status: "ok", Hits: []CallSite{dup}},
		}
		_, out, err := mapVerb(ctx, fake, nil, MapInput{Around: "Seed"})
		if err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if n := strings.Count(out.Map, "pkg/s.go:1"); n != 1 {
			t.Errorf("pkg/s.go:1 appears %d times, want 1\n%s", n, out.Map)
		}
	})
}

// TestMapAroundDiff covers the --around-diff region: blast-radius nodes rendered
// as one map, the ref forwarded to graphDiff, and non-ok propagation.
func TestMapAroundDiff(t *testing.T) {
	ctx := context.Background()

	t.Run("renders blast radius for the ref", func(t *testing.T) {
		fake := &captureAroundSurface{
			Server: stubServer(t),
			diff: DiffOutput{
				Status: "ok",
				Ref:    "HEAD~2",
				Nodes: []ImpactNode{
					{QualifiedName: "pkg.Touched", Kind: "func", Package: "pkg", Path: "pkg/t.go", StartLine: 4, PageRank: 0.9},
				},
			},
		}
		_, out, err := mapVerb(ctx, fake, nil, MapInput{AroundDiff: "HEAD~2"})
		if err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if out.Status != "ok" || out.Zoom != "around" {
			t.Fatalf("out = {%q, zoom %q}, want {ok, around}", out.Status, out.Zoom)
		}
		if !strings.Contains(out.Map, "pkg/t.go:4") {
			t.Errorf("map missing pkg/t.go:4\n%s", out.Map)
		}
		if fake.gotDiff.Ref != "HEAD~2" {
			t.Errorf("diff ref = %q, want HEAD~2", fake.gotDiff.Ref)
		}
	})

	t.Run("non-ok status is propagated", func(t *testing.T) {
		fake := &captureAroundSurface{Server: stubServer(t), diff: DiffOutput{Status: "no-graph", Hint: "no call edges"}}
		_, out, err := mapVerb(ctx, fake, nil, MapInput{AroundDiff: "HEAD~1"})
		if err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if out.Status != "no-graph" || out.Hint != "no call edges" {
			t.Errorf("out = {%q, %q}, want {no-graph, no call edges}", out.Status, out.Hint)
		}
	})

	// #510: a diff that changed files but produced an empty blast radius (new
	// uncalled symbols, deletions, or non-graph lines) must be annotated so it
	// doesn't read as "nothing changed".
	t.Run("empty radius with changed files is annotated", func(t *testing.T) {
		fake := &captureAroundSurface{
			Server: stubServer(t),
			diff: DiffOutput{
				Status:       "ok",
				Ref:          "HEAD~1",
				ChangedFiles: []string{"internal/eval/trace/trace.go", "internal/eval/trace/doc.go"},
				Nodes:        nil, // new files added symbols nothing calls yet
			},
		}
		_, out, err := mapVerb(ctx, fake, nil, MapInput{AroundDiff: "HEAD~1"})
		if err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if out.Status != "ok" {
			t.Fatalf("status = %q, want ok", out.Status)
		}
		for _, want := range []string{"2 changed file(s)", "empty blast radius", "internal/eval/trace/trace.go"} {
			if !strings.Contains(out.Map, want) {
				t.Errorf("annotation missing %q\n%s", want, out.Map)
			}
		}
	})

	t.Run("empty radius with no changed files is not annotated", func(t *testing.T) {
		fake := &captureAroundSurface{Server: stubServer(t), diff: DiffOutput{Status: "ok", Ref: "HEAD~1"}}
		_, out, err := mapVerb(ctx, fake, nil, MapInput{AroundDiff: "HEAD~1"})
		if err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if strings.Contains(out.Map, "empty blast radius") {
			t.Errorf("unexpected annotation when no files changed\n%s", out.Map)
		}
	})
}

// TestMapAroundMutualExclusion covers the guard rails: around + around_diff, and
// around + cluster, both reject before any backend call.
func TestMapAroundMutualExclusion(t *testing.T) {
	ctx := context.Background()
	cluster := 1

	cases := []struct {
		name string
		in   MapInput
	}{
		{"around and around_diff", MapInput{Around: "Seed", AroundDiff: "HEAD~1"}},
		{"around and cluster", MapInput{Around: "Seed", Cluster: &cluster}},
		{"around_diff and cluster", MapInput{AroundDiff: "HEAD~1", Cluster: &cluster}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &captureAroundSurface{Server: stubServer(t)}
			_, out, err := mapVerb(ctx, fake, nil, tc.in)
			if err != nil {
				t.Fatalf("mapVerb: %v", err)
			}
			if out.Status != "error" {
				t.Errorf("status = %q, want error", out.Status)
			}
			if fake.gotCallers.Name != "" || fake.gotDiff.Ref != "" {
				t.Errorf("backend was called despite a rejected combination")
			}
		})
	}
}
