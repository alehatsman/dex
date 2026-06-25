// Tests for the #591 refactor-target fields the Go extractor populates:
// signature, byte span, and declaration hash on function/method/type nodes.

package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const editSpanSrc = `package es

// Greet returns a greeting.
func Greet(name string) string {
	return "hi " + name
}

type Server struct {
	addr string
}

func (s *Server) Addr() string { return s.addr }

type Mode string

type Reader interface {
	Read(p []byte) (int, error)
}
`

func extractEditSpanFixture(t *testing.T) ([]Node, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/es\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "es.go"), []byte(editSpanSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ExtractGo(context.Background(), root)
	if err != nil {
		t.Fatalf("ExtractGo: %v (warnings=%v)", err, res.Warnings)
	}
	return res.Nodes, editSpanSrc
}

// assertSpanFields checks the three invariants every populated node must hold:
// the byte span is well-formed and slices the expected declaration prefix out
// of the source, and the declaration hash is exactly sha1 of the signature.
func assertSpanFields(t *testing.T, n *Node, src, wantSig, wantSlicePrefix string) {
	t.Helper()
	if n == nil {
		t.Fatal("node not found")
	}
	if n.Signature != wantSig {
		t.Errorf("Signature=%q, want %q", n.Signature, wantSig)
	}
	if n.StartByte <= 0 || n.EndByte <= n.StartByte || n.EndByte > len(src) {
		t.Fatalf("byte span (%d,%d) invalid for src len %d", n.StartByte, n.EndByte, len(src))
	}
	slice := src[n.StartByte:n.EndByte]
	if len(slice) < len(wantSlicePrefix) || slice[:len(wantSlicePrefix)] != wantSlicePrefix {
		t.Errorf("src[%d:%d] starts %q, want prefix %q", n.StartByte, n.EndByte, slice, wantSlicePrefix)
	}
	if n.DeclarationHash != declarationHash(n.Signature) {
		t.Errorf("DeclarationHash=%q, want sha1(sig)=%q", n.DeclarationHash, declarationHash(n.Signature))
	}
}

func TestExtractGoFunctionEditSpan(t *testing.T) {
	nodes, src := extractEditSpanFixture(t)
	assertSpanFields(t, findNode(nodes, NodeFunction, "Greet"), src,
		"func Greet(name string) string", "func Greet(")
}

func TestExtractGoMethodEditSpan(t *testing.T) {
	nodes, src := extractEditSpanFixture(t)
	assertSpanFields(t, findNode(nodes, NodeMethod, "(*Server).Addr"), src,
		"func (s *Server) Addr() string", "func (s *Server) Addr()")
}

func TestExtractGoStructEditSpan(t *testing.T) {
	nodes, src := extractEditSpanFixture(t)
	// Struct/interface signatures collapse the body to the keyword so a large
	// type doesn't bloat the column; the byte span still covers the full decl.
	assertSpanFields(t, findNode(nodes, NodeStruct, "Server"), src,
		"type Server struct", "Server struct")
}

func TestExtractGoNamedTypeEditSpan(t *testing.T) {
	nodes, src := extractEditSpanFixture(t)
	// A defined type over a compact underlying is printed in full.
	assertSpanFields(t, findNode(nodes, NodeType, "Mode"), src,
		"type Mode string", "Mode string")
}

func TestExtractGoInterfaceSignature(t *testing.T) {
	nodes, _ := extractEditSpanFixture(t)
	iface := findNode(nodes, NodeInterface, "Reader")
	if iface == nil {
		t.Fatal("interface node Reader not found")
	}
	if iface.Signature != "type Reader interface" {
		t.Errorf("Signature=%q, want %q", iface.Signature, "type Reader interface")
	}
	if iface.StartByte <= 0 || iface.EndByte <= iface.StartByte {
		t.Errorf("interface byte span (%d,%d) invalid", iface.StartByte, iface.EndByte)
	}
}

// TestExtractGoEmptySignatureHashEmpty locks the declarationHash contract: an
// empty signature hashes to the empty string, not sha1("").
func TestExtractGoEmptySignatureHashEmpty(t *testing.T) {
	if got := declarationHash(""); got != "" {
		t.Errorf("declarationHash(\"\")=%q, want empty", got)
	}
	if declarationHash("x") == "" {
		t.Error("declarationHash of a non-empty signature must not be empty")
	}
}
