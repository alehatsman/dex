package graph

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRustTagsParity is the 468c acceptance gate at the unit level: the
// query-driven extractor must produce a graph identical to the hand-rolled
// walker. Node/edge IDs are content-addressed, so equal ID sets prove
// identical nodes, containment, and resolved call edges.
func TestRustTagsParity(t *testing.T) {
	walker := extractWith(t, "rust_simple", newRustExtractor)
	tags := extractWith(t, "rust_simple", newRustTagsExtractor)
	assertIDSetsEqual(t, "rust nodes", nodeIDs(walker.Nodes), nodeIDs(tags.Nodes))
	assertIDSetsEqual(t, "rust edges", edgeIDs(walker.Edges), edgeIDs(tags.Edges))
}

// TestRustTagsParityCornerCases exercises shapes the simple fixture doesn't:
// free functions, inherent + trait impls (implements edge), trait default
// methods, generics in receiver types, calls in closures (dropped), nested
// functions (not nodes), and inline modules (skipped entirely, including
// everything inside them). Any divergence shows up as an ID-set mismatch.
func TestRustTagsParityCornerCases(t *testing.T) {
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
	write("src/corner.rs", `
use std::fmt;
mod sibling;            // bare mod decl: import edge

pub trait Greeter {
    fn hello(&self);
    fn greet(&self) {   // default method: a node with a body
        self.hello();
        helper();
    }
}

pub struct Robot { id: u32 }
pub enum Mode { On, Off }

impl Robot {
    pub fn new(id: u32) -> Self {
        let f = || { closured(); };  // closure body: call dropped
        f();
        Robot { id }
    }
    fn tick(&self) {
        self.spin();
        fn nested() { neverSeen(); } // nested fn: not a node, calls dropped
        nested();
    }
}

impl Greeter for Robot {
    fn hello(&self) { announce(); }
}

pub fn free_fn() {
    Robot::new(1);
}

mod inline {            // inline module: walker skips it entirely
    pub struct Hidden;
    pub fn hidden_fn() { invisible(); }
    impl Hidden { fn m(&self) { alsoHidden(); } }
}
`)
	write("src/sibling.rs", "pub fn sib() {}\n")
	writeIndexAll(t, root)

	walker := extractRootWith(t, root, newRustExtractor)
	tags := extractRootWith(t, root, newRustTagsExtractor)
	assertIDSetsEqual(t, "rust nodes", nodeIDs(walker.Nodes), nodeIDs(tags.Nodes))
	assertIDSetsEqual(t, "rust edges", edgeIDs(walker.Edges), edgeIDs(tags.Edges))
}

func TestRustTagsProvenance(t *testing.T) {
	tags := extractWith(t, "rust_simple", newRustTagsExtractor)
	if len(tags.Edges) == 0 {
		t.Fatal("no edges produced")
	}
	for _, e := range tags.Edges {
		if got, _ := e.Metadata["sitter_lang"].(string); got != "rust-tags" {
			t.Errorf("edge missing sitter_lang=rust-tags; got %q", got)
			break
		}
	}
}
