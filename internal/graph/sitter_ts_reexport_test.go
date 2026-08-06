package graph

import (
	"context"
	"testing"
)

// TestTSReExportBinding exercises cross-package symbol binding through a barrel
// index.ts that only re-exports (#127 Phase 2). testdata/ts_reexport has a
// consumer calling three symbols imported from `@bright/common`, each defined in
// a sibling module and surfaced only via a re-export shape (star, named+rename,
// namespace). Before Phase 2 these calls bound to nothing — the barrel defines
// no such symbol. A fourth call (`ghost`) goes through a re-export cycle and
// must terminate without binding.
func TestTSReExportBinding(t *testing.T) {
	root := copyFixture(t, "ts_reexport")
	reg := NewRegistry()
	reg.Register(newTSTagsExtractor)

	res, err := ExtractSitterWith(context.Background(), root, reg)
	if err != nil {
		t.Fatalf("ExtractSitterWith: %v", err)
	}

	const consumerPkg = "apps/consumer/src/main"
	runID := NodeID("", consumerPkg, NodeFunction, "run")
	if findByID(res.Nodes, runID) == nil {
		t.Fatalf("missing caller run; funcs=%v", nodesOfKindWithPkg(res.Nodes, NodeFunction))
	}

	cases := []struct {
		name, dstPkg, dstName, shape string
	}{
		{"star re-export", "packages/bright-common/src/String", "capitalize",
			"export * from './String'"},
		{"named+rename re-export", "packages/bright-common/src/Base64", "encode",
			"export { encode as b64encode } from './Base64'"},
		{"namespace re-export", "packages/bright-common/src/Arr", "first",
			"export * as Arr from './Arr'; Arr.first()"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dstID := NodeID("", c.dstPkg, NodeFunction, c.dstName)
			if findByID(res.Nodes, dstID) == nil {
				t.Fatalf("missing callee %s in %s", c.dstName, c.dstPkg)
			}
			if findEdge(res.Edges, EdgeCalls, runID, dstID) == nil {
				t.Errorf("missing cross-package calls edge run → %s.%s via %s\n  all calls=%v",
					c.dstPkg, c.dstName, c.shape, edgeKinds(res.Edges, EdgeCalls))
			}
		})
	}

	// ghost() is exported by nothing and reachable only through a star re-export
	// cycle (loop_a ↔ loop_b). resolveExport must terminate and bind no edge.
	t.Run("re-export cycle terminates without binding", func(t *testing.T) {
		for _, e := range res.Edges {
			if e.Kind != EdgeCalls || e.SrcID != runID {
				continue
			}
			if n := findByID(res.Nodes, e.DstID); n != nil && n.Name == "ghost" {
				t.Errorf("ghost() bound through a re-export cycle to %s — expected no edge", e.DstID)
			}
		}
	})
}
