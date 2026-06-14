package graph

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestPythonTagsParity is the 468b acceptance gate at the unit level: the
// query-driven extractor must produce a graph byte-identical to the
// hand-rolled walker. Node and edge IDs are content-addressed (they key
// off package / qualified-name / kind / file / line, never the extractor
// name), so equal ID sets prove identical nodes, identical containment,
// and — critically — identical resolved call edges. Resolution itself is
// shared code; this guards the discovery rewrite.
func TestPythonTagsParity(t *testing.T) {
	walker := extractWith(t, "python_simple", newPythonExtractor)
	tags := extractWith(t, "python_simple", newPythonTagsExtractor)

	assertIDSetsEqual(t, "nodes", nodeIDs(walker.Nodes), nodeIDs(tags.Nodes))
	assertIDSetsEqual(t, "edges", edgeIDs(walker.Edges), edgeIDs(tags.Edges))
}

// TestPythonTagsParityCornerCases exercises shapes the simple fixture
// doesn't: nested functions (must NOT become nodes, their calls dropped),
// methods of nested classes, calls in lambdas / default args / class
// bodies (dropped), and a class defined inside a function (unreachable).
// Any divergence between walker and tags discovery shows up as an ID-set
// mismatch.
func TestPythonTagsParityCornerCases(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/__init__.py", "")
	write("pkg/mod.py", `
import os

def top():
    inner_call()
    def nested():       # nested def: not a node; its calls dropped
        dropped_call()
    return nested

def with_default(x=factory()):   # default-arg call: not collected
    real()

lambda_holder = lambda: lambda_call()   # lambda body call: dropped

class Outer:
    base_attr = class_body_call()        # class-body call: dropped

    def method(self):
        self.helper()
        [comp_call() for _ in range(3)]  # comprehension call: collected

    class Inner:                          # nested class: a node
        def inner_method(self):
            inner_method_call()

def make():
    class Local:                          # class in function body: unreachable
        def m(self):
            pass
    return Local
`)
	writeIndexAll(t, root)

	walker := extractRootWith(t, root, newPythonExtractor)
	tags := extractRootWith(t, root, newPythonTagsExtractor)

	assertIDSetsEqual(t, "nodes", nodeIDs(walker.Nodes), nodeIDs(tags.Nodes))
	assertIDSetsEqual(t, "edges", edgeIDs(walker.Edges), edgeIDs(tags.Edges))
}

// TestPythonTagsProvenance documents the distinct A/B attribution stamp:
// the tags front-end records sitter_lang=python-tags while emitting an
// otherwise identical graph.
func TestPythonTagsProvenance(t *testing.T) {
	tags := extractWith(t, "python_simple", newPythonTagsExtractor)
	if len(tags.Edges) == 0 {
		t.Fatal("no edges produced")
	}
	for _, e := range tags.Edges {
		if got, _ := e.Metadata["sitter_lang"].(string); got != "python-tags" {
			t.Errorf("edge missing sitter_lang=python-tags; %+v", e)
			break
		}
	}
}

func extractWith(t *testing.T, fixture string, f ExtractorFactory) *ExtractResult {
	t.Helper()
	return extractRootWith(t, copyFixture(t, fixture), f)
}

func extractRootWith(t *testing.T, root string, f ExtractorFactory) *ExtractResult {
	t.Helper()
	reg := NewRegistry()
	reg.Register(f)
	res, err := ExtractSitterWith(context.Background(), root, reg)
	if err != nil {
		t.Fatalf("ExtractSitterWith: %v", err)
	}
	return res
}

func nodeIDs(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

func edgeIDs(edges []Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, string(e.Kind)+" "+e.SrcID+" -> "+e.DstID)
	}
	return out
}

func assertIDSetsEqual(t *testing.T, label string, want, got []string) {
	t.Helper()
	wSet := map[string]int{}
	for _, s := range want {
		wSet[s]++
	}
	gSet := map[string]int{}
	for _, s := range got {
		gSet[s]++
	}
	var missing, extra []string
	for s, n := range wSet {
		if gSet[s] < n {
			missing = append(missing, s)
		}
	}
	for s, n := range gSet {
		if wSet[s] < n {
			extra = append(extra, s)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%s only in walker (tags is missing %d):\n  %v", label, len(missing), missing)
	}
	if len(extra) > 0 {
		t.Errorf("%s only in tags (walker lacks %d):\n  %v", label, len(extra), extra)
	}
}
