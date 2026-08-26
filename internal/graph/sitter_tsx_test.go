package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestTSXComponentSignature locks #232: an exported const arrow-function
// React component whose body is JSX must still be captured as a NodeFunction.
// Parsing a .tsx file with the plain TypeScript grammar (no jsx_* node types)
// mislexes `<div>` as a type assertion, turning the arrow function's value
// node into a binary_expression and silently dropping the symbol.
func TestTSXComponentSignature(t *testing.T) {
	dir := t.TempDir()
	writeIndexAll(t, dir)
	src := `import { ReactNode } from 'react';

export const Greeting = (props: { name: string }): ReactNode => {
  return <div className="greeting"><span>{props.name}</span></div>;
};
`
	if err := os.WriteFile(filepath.Join(dir, "Greeting.tsx"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	reg.Register(newTSXTagsExtractor)
	res, err := ExtractSitterWith(context.Background(), dir, reg)
	if err != nil {
		t.Fatalf("ExtractSitterWith: %v", err)
	}

	pkg := "Greeting"
	id := NodeID("", pkg, NodeFunction, "Greeting")
	if findNode(res.Nodes, NodeFunction, "Greeting") == nil {
		t.Errorf("Greeting component not emitted as a function node; nodes=%v", nodesOfKind(res.Nodes, NodeFunction))
	}
	_ = id
}

// TestTSTypeAliasIndexed locks #232: TS `type X = ...` aliases were never
// captured by the tags query at all (only interface/class/function), so
// `kind:type` / `type:` selector queries silently returned nothing for them.
func TestTSTypeAliasIndexed(t *testing.T) {
	dir := t.TempDir()
	writeIndexAll(t, dir)
	src := "export type UserID = string;\n"
	if err := os.WriteFile(filepath.Join(dir, "types.ts"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	reg.Register(newTSTagsExtractor)
	res, err := ExtractSitterWith(context.Background(), dir, reg)
	if err != nil {
		t.Fatalf("ExtractSitterWith: %v", err)
	}

	if findNode(res.Nodes, NodeType, "UserID") == nil {
		t.Errorf("UserID type alias not emitted as a NodeType; type nodes=%v", nodesOfKind(res.Nodes, NodeType))
	}
}
