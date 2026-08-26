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
// call (or tagged-template call) to a known HOC — styled(...)(...),
// styled.div`...`, forwardRef(...), memo(...) — is a common React
// HOC-wrapped component shape. emitArrowDeclarator only recognized
// arrow_function/function/function_expression values, so these were
// silently dropped — no graph node, so JSX call-sites referencing them
// never resolved an edge. Gating on the callee identity (not just a
// PascalCase name) avoids mislabeling ordinary call-valued consts like
// `const Config = loadConfig()` as functions.
func TestHOCComponentEmitted(t *testing.T) {
	dir := t.TempDir()
	writeIndexAll(t, dir)
	src := "import { forwardRef } from 'react';\n" +
		"import { styled } from '@mui/material';\n" +
		"\n" +
		"export const FlexSpacer = styled('span')({ flexGrow: 1 });\n" +
		"\n" +
		"export const Bar = styled.div`color: red;`;\n" +
		"\n" +
		"export const Widget = forwardRef((props, ref) => {\n" +
		"  return <div ref={ref} />;\n" +
		"});\n" +
		"\n" +
		"const notAComponent = createThing();\n" +
		"\n" +
		"const Config = loadConfig();\n"
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
		t.Errorf("FlexSpacer (styled(...)(...) HOC) not emitted as a function node; nodes=%v", nodesOfKind(res.Nodes, NodeFunction))
	}
	if findNode(res.Nodes, NodeFunction, "Bar") == nil {
		t.Errorf("Bar (styled.div`...` HOC) not emitted as a function node; nodes=%v", nodesOfKind(res.Nodes, NodeFunction))
	}
	if findNode(res.Nodes, NodeFunction, "Widget") == nil {
		t.Errorf("Widget (forwardRef HOC) not emitted as a function node; nodes=%v", nodesOfKind(res.Nodes, NodeFunction))
	}
	if findNode(res.Nodes, NodeFunction, "notAComponent") != nil {
		t.Errorf("notAComponent (non-HOC call-valued const) should NOT be emitted as a function node")
	}
	if findNode(res.Nodes, NodeFunction, "Config") != nil {
		t.Errorf("Config (PascalCase but non-HOC call-valued const) should NOT be emitted as a function node")
	}
}
