package graph

import (
	"os"
	"path/filepath"
	"testing"
)

// TestJavaTagsParity is the 468c acceptance gate at the unit level: the
// query-driven extractor must produce a graph identical to the hand-rolled
// walker. Node/edge IDs are content-addressed, so equal ID sets prove
// identical nodes, containment, and resolved call edges.
func TestJavaTagsParity(t *testing.T) {
	walker := extractWith(t, "java_simple", newJavaExtractor)
	tags := extractWith(t, "java_simple", newJavaTagsExtractor)
	assertIDSetsEqual(t, "java nodes", nodeIDs(walker.Nodes), nodeIDs(tags.Nodes))
	assertIDSetsEqual(t, "java edges", edgeIDs(walker.Edges), edgeIDs(tags.Edges))
}

// TestJavaTagsParityCornerCases exercises shapes the simple fixture doesn't:
// overloaded methods (first-overload bare-name registration), constructors,
// interfaces with abstract methods, enums (whose methods the walker skips),
// nested/local/anonymous types (not modelled), calls in field initializers
// (dropped at top-level, kept inside an anonymous class body), and `new`
// expressions. Any divergence shows up as an ID-set mismatch.
func TestJavaTagsParityCornerCases(t *testing.T) {
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
	write("com/example/Corner.java", `
package com.example;

public class Corner {
  int field = compute();          // field init: call dropped (no enclosing method)

  public Corner() { init(); }     // constructor
  public Corner(int x) { init(); } // overloaded constructor

  void format(String s) { sink(s); }
  void format(int n) { sink(n); } // overload: bareQN "Corner.format" -> first

  void outer() {
    helper();
    Runnable r = new Runnable() {
      int inner = made();          // anon-class field init: attributed to outer
      public void run() { dropped(); } // anon method body: dropped
    };
    class Local { void m() { alsoDropped(); } } // local type: not a node
    new Thing();
  }

  static class Nested {            // nested type: not a node
    void nope() { neverSeen(); }
  }
}

interface Shape {
  double area();                   // abstract interface method: a node
}

enum Color {
  RED, GREEN;
  void shade() { tint(); }         // enum method: walker skips it
}
`)
	writeIndexAll(t, root)

	walker := extractRootWith(t, root, newJavaExtractor)
	tags := extractRootWith(t, root, newJavaTagsExtractor)
	assertIDSetsEqual(t, "java nodes", nodeIDs(walker.Nodes), nodeIDs(tags.Nodes))
	assertIDSetsEqual(t, "java edges", edgeIDs(walker.Edges), edgeIDs(tags.Edges))
}

func TestJavaTagsProvenance(t *testing.T) {
	tags := extractWith(t, "java_simple", newJavaTagsExtractor)
	if len(tags.Edges) == 0 {
		t.Fatal("no edges produced")
	}
	for _, e := range tags.Edges {
		if got, _ := e.Metadata["sitter_lang"].(string); got != "java-tags" {
			t.Errorf("edge missing sitter_lang=java-tags; got %q", got)
			break
		}
	}
}
