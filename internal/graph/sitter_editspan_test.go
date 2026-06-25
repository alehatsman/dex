// Tests that the tree-sitter extractors populate the #591 byte-span fields on
// definition nodes. Signatures stay Go-authoritative (the tree-sitter path
// leaves them empty), but the byte span — the load-bearing edit target — must
// be filled for every language. Python stands in for the shared emit path.

package graph

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPythonExtractorByteSpans(t *testing.T) {
	root := t.TempDir()
	src := "def greet(name):\n    return \"hi \" + name\n\n\nclass Server:\n    def addr(self):\n        return self._addr\n"
	p := filepath.Join(root, "mod.py")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	writeIndexAll(t, root)

	reg := NewRegistry()
	reg.Register(newPythonTagsExtractor)
	res, err := ExtractSitterWith(context.Background(), root, reg)
	if err != nil {
		t.Fatalf("ExtractSitterWith: %v", err)
	}

	// Top-level function: span slices the whole `def greet` block out of src.
	greet := findNode(res.Nodes, NodeFunction, "greet")
	if greet == nil {
		t.Fatalf("missing function node greet; funcs=%v", nodesOfKind(res.Nodes, NodeFunction))
	}
	assertByteSpanSlices(t, greet, src, "def greet(name):")

	// Class node carries a span too.
	server := findNode(res.Nodes, NodeClass, "Server")
	if server == nil {
		t.Fatalf("missing class node Server; classes=%v", nodesOfKind(res.Nodes, NodeClass))
	}
	assertByteSpanSlices(t, server, src, "class Server:")

	// Method inside the class.
	addr := findNode(res.Nodes, NodeMethod, "Server.addr")
	if addr == nil {
		t.Fatalf("missing method node Server.addr; methods=%v", nodesOfKind(res.Nodes, NodeMethod))
	}
	assertByteSpanSlices(t, addr, src, "def addr(self):")

	// The tree-sitter path is byte-spans-only: signatures stay empty (Go owns
	// signature extraction). Locking this keeps the scope boundary explicit.
	if greet.Signature != "" {
		t.Errorf("tree-sitter signature should be empty (Go-authoritative), got %q", greet.Signature)
	}
}

func assertByteSpanSlices(t *testing.T, n *Node, src, wantPrefix string) {
	t.Helper()
	if n.StartByte < 0 || n.EndByte <= n.StartByte || n.EndByte > len(src) {
		t.Fatalf("byte span (%d,%d) invalid for src len %d", n.StartByte, n.EndByte, len(src))
	}
	slice := src[n.StartByte:n.EndByte]
	if !strings.HasPrefix(slice, wantPrefix) {
		t.Errorf("src[%d:%d]=%q, want prefix %q", n.StartByte, n.EndByte, slice, wantPrefix)
	}
}
