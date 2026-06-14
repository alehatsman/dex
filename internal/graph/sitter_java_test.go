package graph

import (
	"context"
	"testing"
)

// TestJavaExtractorFixture exercises the Java extractor against
// testdata/java_simple. Covers: package declaration overrides dir
// path; class/method extraction; bare in-class call; this.X dispatch;
// `ClassName.staticMethod()` via dotted import; `new Foo()` to a
// same-package class.
func TestJavaExtractorFixture(t *testing.T) {
	root := copyFixture(t, "java_simple")
	reg := NewRegistry()
	reg.Register(newJavaTagsExtractor)

	res, err := ExtractSitterWith(context.Background(), root, reg)
	if err != nil {
		t.Fatalf("ExtractSitterWith: %v", err)
	}

	// ---- Files ----
	wantFiles := []string{
		"com/example/Main.java",
		"com/example/Handler.java",
		"com/example/util/Text.java",
	}
	for _, rel := range wantFiles {
		if findNode(res.Nodes, NodeFile, rel) == nil {
			t.Errorf("missing file node %q; files=%v", rel, nodesOfKind(res.Nodes, NodeFile))
		}
	}

	// ---- Packages — declared, not derived from path ----
	appPkg := "com.example"
	utilPkg := "com.example.util"
	for _, p := range []string{appPkg, utilPkg} {
		if findNode(res.Nodes, NodePackage, p) == nil {
			t.Errorf("missing package node %q; packages=%v", p, nodesOfKind(res.Nodes, NodePackage))
		}
	}

	// ---- Classes ----
	mainClsID := NodeID("", appPkg, NodeClass, "Main")
	handlerClsID := NodeID("", appPkg, NodeClass, "Handler")
	textClsID := NodeID("", utilPkg, NodeClass, "Text")
	for _, id := range []string{mainClsID, handlerClsID, textClsID} {
		if findByID(res.Nodes, id) == nil {
			t.Errorf("missing class node id=%s; classes=%v", id, nodesOfKindWithPkg(res.Nodes, NodeClass))
		}
	}

	// ---- Methods ----
	// Node keys carry the parameter-type signature so overloads stay
	// distinct (#444): format(String) and format(int) are two nodes.
	type m struct{ pkg, qn string }
	for _, c := range []m{
		{appPkg, "Main.run()"},
		{appPkg, "Main.helper()"},
		{appPkg, "Handler.<init>(String)"}, // constructor
		{appPkg, "Handler.greet()"},
		{appPkg, "Handler.format(String)"},
		{appPkg, "Handler.format(int)"}, // overload — distinct node
		{utilPkg, "Text.upper(String)"},
		{utilPkg, "Text.lower(String)"},
	} {
		id := NodeID("", c.pkg, NodeMethod, c.qn)
		if findByID(res.Nodes, id) == nil {
			t.Errorf("missing method %s::%s; methods=%v",
				c.pkg, c.qn, nodesOfKindWithPkg(res.Nodes, NodeMethod))
		}
	}

	// ---- Overloads are distinct nodes, both attached to the class (#444) ----
	formatStrID := NodeID("", appPkg, NodeMethod, "Handler.format(String)")
	formatIntID := NodeID("", appPkg, NodeMethod, "Handler.format(int)")
	if formatStrID == formatIntID {
		t.Fatal("overload node ids collided — signature not in key")
	}
	for _, fid := range []string{formatStrID, formatIntID} {
		if findByID(res.Nodes, fid) == nil {
			t.Errorf("overload node %s missing — overloads collapsed", fid)
		}
		if findEdge(res.Edges, EdgeHasMethod, handlerClsID, fid) == nil {
			t.Errorf("missing has_method Handler → %s", fid)
		}
	}

	// ---- has_method edges ----
	greetID := NodeID("", appPkg, NodeMethod, "Handler.greet()")
	if findEdge(res.Edges, EdgeHasMethod, handlerClsID, greetID) == nil {
		t.Errorf("missing has_method Handler → Handler.greet()")
	}

	// ---- Package-level imports edge ----
	mainPkgID := NodeID("", appPkg, NodePackage, appPkg)
	textImpID := NodeID("", appPkg, NodeImport, "com.example.util.Text")
	if findEdge(res.Edges, EdgeImports, mainPkgID, textImpID) == nil {
		t.Errorf("missing package-level imports edge %s → com.example.util.Text", appPkg)
	}

	// ---- Calls ----
	mainRunID := NodeID("", appPkg, NodeMethod, "Main.run()")
	helperID := NodeID("", appPkg, NodeMethod, "Main.helper()")
	upperID := NodeID("", utilPkg, NodeMethod, "Text.upper(String)")
	// Unsignatured `this.format(...)` resolves to the first-declared
	// overload via the bare-name symbol entry — format(String) here.
	formatID := formatStrID

	calls := []struct {
		name     string
		src, dst string
		why      string
	}{
		{"Main.run → helper (bare same-class)", mainRunID, helperID, "bare call inside Main.run resolves to Main.helper"},
		{"Main.run → Text.upper (imported static-style call)", mainRunID, upperID, "Text.upper() — Text in import table, method lookup"},
		{"Main.run → Handler (new same-package)", mainRunID, handlerClsID, "new Handler(...) resolves to same-package class node"},
		{"Handler.greet → Handler.format (this.X)", greetID, formatID, "this.format(...) in greet resolves to same class"},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			if findEdge(res.Edges, EdgeCalls, c.src, c.dst) == nil {
				t.Errorf("missing calls edge %s\n  why: %s\n  all calls=%v",
					c.name, c.why, edgeKinds(res.Edges, EdgeCalls))
			}
		})
	}

	// ---- Provenance ----
	for _, e := range res.Edges {
		if got, _ := e.Metadata["provenance"].(string); got != "sitter" {
			t.Errorf("edge missing provenance=sitter; %+v", e)
		}
		if got, _ := e.Metadata["sitter_lang"].(string); got != "java" {
			t.Errorf("edge missing sitter_lang=java; %+v", e)
		}
	}
}
