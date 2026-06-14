package graph

import (
	"os"
	"path/filepath"
	"testing"
)

// TestJSTagsParity / TestTSTagsParity are the 468c acceptance gates at the
// unit level: the query-driven extractor must produce a graph identical to
// the hand-rolled walker. Node/edge IDs are content-addressed, so equal ID
// sets prove identical nodes, containment, and resolved call edges.
func TestJSTagsParity(t *testing.T) {
	walker := extractWith(t, "js_simple", newJSExtractor)
	tags := extractWith(t, "js_simple", newJSTagsExtractor)
	assertIDSetsEqual(t, "js nodes", nodeIDs(walker.Nodes), nodeIDs(tags.Nodes))
	assertIDSetsEqual(t, "js edges", edgeIDs(walker.Edges), edgeIDs(tags.Edges))
}

func TestTSTagsParity(t *testing.T) {
	walker := extractWith(t, "ts_simple", newTSExtractor)
	tags := extractWith(t, "ts_simple", newTSTagsExtractor)
	assertIDSetsEqual(t, "ts nodes", nodeIDs(walker.Nodes), nodeIDs(tags.Nodes))
	assertIDSetsEqual(t, "ts edges", edgeIDs(walker.Edges), edgeIDs(tags.Edges))
}

// TestTSTagsParityCornerCases exercises shapes the simple fixture doesn't:
// exported decls, arrow consts, nested functions (dropped), class field
// initializers (calls dropped), methods of nested classes, interfaces,
// and namespaced/`new` calls. Any divergence shows up as an ID-set
// mismatch against the walker.
func TestTSTagsParityCornerCases(t *testing.T) {
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
	write("src/util.ts", `
export function top(): void {
  helper();
  function nested() { droppedCall(); }   // nested def: not a node
  nested();
}

export const arrow = () => { arrowCall(); };
const plain = function () { plainCall(); };
var legacy = () => { legacyCall(); };     // var: ts walker ignores it

export interface Shape { area(): number; }

export class Outer {
  field = makeField();                    // field initializer call: dropped
  method(): void {
    this.helper();
    const local = () => { localDropped(); }; // field/local arrow: dropped
    new Thing();
  }
  class Inner {}
}

export default class Service {
  run() { doWork(); }
}
`)
	writeIndexAll(t, root)

	walker := extractRootWith(t, root, newTSExtractor)
	tags := extractRootWith(t, root, newTSTagsExtractor)
	assertIDSetsEqual(t, "ts nodes", nodeIDs(walker.Nodes), nodeIDs(tags.Nodes))
	assertIDSetsEqual(t, "ts edges", edgeIDs(walker.Edges), edgeIDs(tags.Edges))
}

func TestJSTagsProvenance(t *testing.T) {
	tags := extractWith(t, "js_simple", newJSTagsExtractor)
	if len(tags.Edges) == 0 {
		t.Fatal("no edges produced")
	}
	for _, e := range tags.Edges {
		if got, _ := e.Metadata["sitter_lang"].(string); got != "javascript-tags" {
			t.Errorf("edge missing sitter_lang=javascript-tags; got %q", got)
			break
		}
	}
}
