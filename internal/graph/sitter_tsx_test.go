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

// TestHOCComponentEmitted locks #237: a top-level const whose value is a
// call_expression (styled(...)(...), forwardRef(...), memo(...)) is a common
// React HOC-wrapped component shape. emitArrowDeclarator only recognized
// arrow_function/function/function_expression values, so these were silently
// dropped — no graph node, so JSX call-sites referencing them never resolved
// an edge. PascalCase naming (the React component convention) gates the
// call_expression case to avoid mislabeling ordinary factory-call consts.
func TestHOCComponentEmitted(t *testing.T) {
	dir := t.TempDir()
	writeIndexAll(t, dir)
	src := `import { forwardRef } from 'react';
import { styled } from '@mui/material';

export const FlexSpacer = styled('span')({ flexGrow: 1 });

export const Widget = forwardRef((props, ref) => {
  return <div ref={ref} />;
});

const notAComponent = createThing();
`
	if err := os.WriteFile(filepath.Join(dir, "Components.tsx"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	reg.Register(newTSXTagsExtractor)
	res, err := ExtractSitterWith(context.Background(), dir, reg)
	if err != nil {
		t.Fatalf("ExtractSitterWith: %v", err)
	}

	if findNode(res.Nodes, NodeFunction, "FlexSpacer") == nil {
		t.Errorf("FlexSpacer (styled HOC) not emitted as a function node; nodes=%v", nodesOfKind(res.Nodes, NodeFunction))
	}
	if findNode(res.Nodes, NodeFunction, "Widget") == nil {
		t.Errorf("Widget (forwardRef HOC) not emitted as a function node; nodes=%v", nodesOfKind(res.Nodes, NodeFunction))
	}
	if findNode(res.Nodes, NodeFunction, "notAComponent") != nil {
		t.Errorf("notAComponent (non-PascalCase call-valued const) should NOT be emitted as a function node")
	}
}
