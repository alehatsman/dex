package mcp

import (
	"context"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// captureCommSurface embeds *Server — inheriting the full toolSurface — and
// overrides graphCommunities to return canned data while recording the input it
// was handed. That lets the mapVerb seam be driven directly without building a
// real graph index: the embedded *Server satisfies the other 36 methods that
// mapVerb never touches.
type captureCommSurface struct {
	*Server
	got CommunitiesInput
	out CommunitiesOutput
	err error
}

func (f *captureCommSurface) graphCommunities(_ context.Context, _ *sdk.CallToolRequest, in CommunitiesInput) (*sdk.CallToolResult, CommunitiesOutput, error) {
	f.got = in
	return nil, f.out, f.err
}

// TestMapVerbDispatch exercises mapVerb's routing directly (#355 L3): the
// default fill of min_members/k/top_k, explicit-value passthrough, the non-ok
// status propagation, and the L0/L1 + cluster-not-found branches. The
// surface-level presence assertion in verbs_test.go reaches none of these.
func TestMapVerbDispatch(t *testing.T) {
	ctx := context.Background()
	okOneCluster := CommunitiesOutput{Status: "ok", Communities: []Community{{ID: 1, Size: 5}}}

	t.Run("defaults fill when zero", func(t *testing.T) {
		fake := &captureCommSurface{Server: stubServer(t), out: CommunitiesOutput{Status: "ok"}}
		if _, _, err := mapVerb(ctx, fake, nil, MapInput{}); err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if fake.got.MinMembers != 3 || fake.got.K != 50 || fake.got.TopK != 25 {
			t.Errorf("defaults = min=%d k=%d topK=%d, want 3/50/25", fake.got.MinMembers, fake.got.K, fake.got.TopK)
		}
	})

	t.Run("explicit values pass through", func(t *testing.T) {
		fake := &captureCommSurface{Server: stubServer(t), out: CommunitiesOutput{Status: "ok"}}
		if _, _, err := mapVerb(ctx, fake, nil, MapInput{MinMembers: 7, K: 9, TopK: 11}); err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if fake.got.MinMembers != 7 || fake.got.K != 9 || fake.got.TopK != 11 {
			t.Errorf("passthrough = min=%d k=%d topK=%d, want 7/9/11", fake.got.MinMembers, fake.got.K, fake.got.TopK)
		}
	})

	t.Run("non-ok status is propagated", func(t *testing.T) {
		fake := &captureCommSurface{Server: stubServer(t), out: CommunitiesOutput{Status: "no-graph", Hint: "no calls edges"}}
		_, out, err := mapVerb(ctx, fake, nil, MapInput{})
		if err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if out.Status != "no-graph" || out.Hint != "no calls edges" {
			t.Errorf("out = {%q, %q}, want {no-graph, no calls edges}", out.Status, out.Hint)
		}
	})

	t.Run("unknown cluster id is not-found", func(t *testing.T) {
		fake := &captureCommSurface{Server: stubServer(t), out: okOneCluster}
		miss := 2
		_, out, err := mapVerb(ctx, fake, nil, MapInput{Cluster: &miss})
		if err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if out.Status != "not-found" {
			t.Errorf("status = %q, want not-found", out.Status)
		}
	})

	t.Run("known cluster id zooms to l1", func(t *testing.T) {
		fake := &captureCommSurface{Server: stubServer(t), out: okOneCluster}
		hit := 1
		_, out, err := mapVerb(ctx, fake, nil, MapInput{Cluster: &hit})
		if err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if out.Status != "ok" || out.Zoom != "l1" {
			t.Errorf("out = {%q, zoom %q}, want {ok, l1}", out.Status, out.Zoom)
		}
	})

	t.Run("no cluster renders the orient bundle", func(t *testing.T) {
		fake := &captureCommSurface{Server: stubServer(t), out: okOneCluster}
		_, out, err := mapVerb(ctx, fake, nil, MapInput{})
		if err != nil {
			t.Fatalf("mapVerb: %v", err)
		}
		if out.Status != "ok" || out.Zoom != "orient" {
			t.Errorf("out = {%q, zoom %q}, want {ok, orient}", out.Status, out.Zoom)
		}
	})
}
