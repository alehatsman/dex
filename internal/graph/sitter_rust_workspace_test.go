package graph

import (
	"context"
	"testing"
)

// TestRustExtractorWorkspace exercises the Cargo-workspace path of the Rust
// extractor (#162): package paths are crate-relative (crates/<name>/src/… →
// crate::mod, …/src/lib.rs → crate root) and cross-crate `use` imports carry a
// resolved internal target, so the package/crate DAG has real edges. Contrast
// TestRustExtractorFixture, which covers the single-crate (src/ only) layout.
func TestRustExtractorWorkspace(t *testing.T) {
	root := copyFixture(t, "rust_workspace")
	reg := NewRegistry()
	reg.Register(newRustTagsExtractor)

	res, err := ExtractSitterWith(context.Background(), root, reg)
	if err != nil {
		t.Fatalf("ExtractSitterWith: %v", err)
	}

	// ---- Crate-relative package nodes ----
	// core-lib/src/lib.rs → crate root `core_lib`; core-lib/src/util.rs →
	// `core_lib::util`; app/src/main.rs → crate root `app`. The hyphen in the
	// Cargo name becomes an underscore in the Rust crate identifier.
	for _, p := range []string{"core_lib", "core_lib::util", "app"} {
		if findNode(res.Nodes, NodePackage, p) == nil {
			t.Errorf("missing crate-relative package %q; packages=%v", p, nodesOfKind(res.Nodes, NodePackage))
		}
	}
	// The pre-fix synthetic path must be gone.
	if findNode(res.Nodes, NodePackage, "crates::core-lib::src::lib") != nil {
		t.Errorf("found synthetic layout package path — crate identity not applied")
	}

	// ---- Cross-crate imports carry a resolved internal target ----
	appPkgID := NodeID("", "app", NodePackage, "app")
	for _, tc := range []struct{ use, target string }{
		{"core_lib::Widget", "core_lib"},           // item in crate root → crate root
		{"core_lib::util::help", "core_lib::util"}, // item in submodule → submodule
	} {
		imp := findNode(res.Nodes, NodeImport, tc.use)
		if imp == nil {
			t.Errorf("missing import node %q; imports=%v", tc.use, nodesOfKind(res.Nodes, NodeImport))
			continue
		}
		if got, _ := imp.Metadata["target"].(string); got != tc.target {
			t.Errorf("import %q target = %q, want %q", tc.use, got, tc.target)
		}
		impID := NodeID("", "app", NodeImport, tc.use)
		if findEdge(res.Edges, EdgeImports, appPkgID, impID) == nil {
			t.Errorf("missing cross-crate imports edge app → %s", tc.use)
		}
	}

	// ---- External use resolves to no internal target (dropped, like a bare dep) ----
	if imp := findNode(res.Nodes, NodeImport, "std::collections::HashMap"); imp != nil {
		if got, _ := imp.Metadata["target"].(string); got != "" {
			t.Errorf("external import got internal target %q, want none", got)
		}
	}
}
