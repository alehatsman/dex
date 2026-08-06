package graph

import (
	"context"
	"testing"
)

// TestTSWorkspaceResolution exercises workspace-aware module resolution (#127
// Phase 1) against testdata/ts_workspace, a two-package monorepo with a
// tsconfig `paths` alias (`@/*`), an exact alias to a workspace package
// (`@bright/common`), a relative import, and a bare npm dep. Each import node in
// apps/base-view/src/main must be annotated with the right resolution outcome.
func TestTSWorkspaceResolution(t *testing.T) {
	root := copyFixture(t, "ts_workspace")
	reg := NewRegistry()
	reg.Register(newTSTagsExtractor)

	res, err := ExtractSitterWith(context.Background(), root, reg)
	if err != nil {
		t.Fatalf("ExtractSitterWith: %v", err)
	}

	// importTarget returns the resolved target metadata for the import node with
	// the given raw specifier, imported from mainPkg.
	const mainPkg = "apps/base-view/src/main"
	find := func(specifier string) *Node {
		id := NodeID("", mainPkg, NodeImport, specifier)
		return findByID(res.Nodes, id)
	}

	cases := []struct {
		specifier  string
		wantTarget string // "" ⇒ expect external instead
		external   bool
	}{
		{specifier: "@bright/common", wantTarget: "packages/bright-common/src/index"},
		{specifier: "@/util", wantTarget: "apps/base-view/src/util"},
		{specifier: "./sibling", wantTarget: "apps/base-view/src/sibling"},
		{specifier: "react", external: true},
	}

	for _, tc := range cases {
		t.Run(tc.specifier, func(t *testing.T) {
			n := find(tc.specifier)
			if n == nil {
				t.Fatalf("no import node for %q; imports=%v",
					tc.specifier, nodesOfKindWithPkg(res.Nodes, NodeImport))
			}
			if tc.external {
				if ext, _ := n.Metadata["external"].(bool); !ext {
					t.Errorf("import %q: want external=true, metadata=%v", tc.specifier, n.Metadata)
				}
				if _, hasTarget := n.Metadata["target"]; hasTarget {
					t.Errorf("import %q: external dep must not carry a target: %v", tc.specifier, n.Metadata)
				}
				return
			}
			got, _ := n.Metadata["target"].(string)
			if got != tc.wantTarget {
				t.Errorf("import %q: target = %q, want %q (metadata=%v)",
					tc.specifier, got, tc.wantTarget, n.Metadata)
			}
			if ext, _ := n.Metadata["external"].(bool); ext {
				t.Errorf("import %q: internal target must not be flagged external", tc.specifier)
			}
		})
	}
}
