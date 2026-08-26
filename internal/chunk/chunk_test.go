package chunk

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStructuralAndOrphanChunks(t *testing.T) {
	src := []byte(`package main

import "fmt"

const Important = "x"

// Greet prints a greeting.
func Greet() {
	fmt.Println("hi")
}

var TrailingVar = 42
`)
	chunks, err := Chunks(context.Background(), "x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	var sawFunc, sawOrphanHead, sawOrphanTail bool
	for _, c := range chunks {
		switch {
		case c.Kind == "function_declaration" && strings.Contains(c.Content, "func Greet"):
			sawFunc = true
			// Backfilled doc comment must be included.
			if !strings.Contains(c.Content, "Greet prints a greeting") {
				t.Errorf("function chunk missing doc comment: %q", c.Content)
			}
		case c.Kind == "orphan" && strings.Contains(c.Content, "Important"):
			sawOrphanHead = true
		case c.Kind == "orphan" && strings.Contains(c.Content, "TrailingVar"):
			sawOrphanTail = true
		}
	}
	if !sawFunc {
		t.Error("expected function_declaration chunk for Greet")
	}
	if !sawOrphanHead {
		t.Error("expected orphan chunk covering top-level const")
	}
	if !sawOrphanTail {
		t.Error("expected orphan chunk covering trailing var")
	}
}

// TestTSXChunkCapturesJSXComponent locks #236: .tsx must chunk with the TSX
// grammar, not plain TypeScript — the plain grammar mislexes `<div>` as a
// type-assertion expression, turning the arrow function's JSX-bodied value
// into something the lexical_declaration chunk kind never surfaces as a
// structural chunk (silently falls back to an orphan window instead).
func TestTSXChunkCapturesJSXComponent(t *testing.T) {
	src := []byte(`import { ReactNode } from 'react';

export const Greeting = (props: { name: string }): ReactNode => {
  return <div className="greeting"><span>{props.name}</span></div>;
};
`)
	chunks, err := Chunks(context.Background(), "Greeting.tsx", src)
	if err != nil {
		t.Fatal(err)
	}
	var sawComponent bool
	for _, c := range chunks {
		if c.Kind == "export_statement" && strings.Contains(c.Content, "Greeting") && strings.Contains(c.Content, "<div") {
			sawComponent = true
		}
	}
	if !sawComponent {
		t.Errorf("expected a structural chunk containing the JSX-bodied Greeting component with its <div> body intact; chunks=%+v", chunks)
	}
}

func TestLongLineFallsBackToByteWindows(t *testing.T) {
	// A single line longer than MaxBytes. Without byte-window fallback,
	// halveAndChunk returned nil and the file produced zero chunks.
	long := strings.Repeat("ab", MaxBytes) // 2*MaxBytes bytes, no newline
	chunks, err := Chunks(context.Background(), "blob.txt", []byte(long))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk for an oversized single-line file")
	}
	for _, c := range chunks {
		if len(c.Content) > MaxBytes {
			t.Errorf("chunk len %d > MaxBytes %d", len(c.Content), MaxBytes)
		}
	}
	total := 0
	for _, c := range chunks {
		total += len(c.Content)
	}
	if total != len(long) {
		t.Errorf("byte-window coverage = %d bytes, want %d", total, len(long))
	}
}

func TestUnknownExtensionUsesWindow(t *testing.T) {
	src := []byte("title: hello\nbody: this is plain text\n")
	chunks, err := Chunks(context.Background(), "notes.unknown", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 window chunk; got %d", len(chunks))
	}
	if chunks[0].Kind != "window" {
		t.Errorf("kind = %q, want window", chunks[0].Kind)
	}
}

func TestEmptyAndWhitespaceOnly(t *testing.T) {
	for _, src := range []string{"", "   \n\n\t\n"} {
		chunks, err := Chunks(context.Background(), "blank.go", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) != 0 {
			t.Errorf("expected 0 chunks for %q; got %d", src, len(chunks))
		}
	}
}

func TestEmbedTextStampsPathAndKind(t *testing.T) {
	c := Chunk{Path: "pkg/x.go", Kind: "function_declaration", Content: "func f(){}"}
	got := c.EmbedText()
	if !strings.HasPrefix(got, "// path: pkg/x.go\n// kind: function_declaration\nfunc f(){}") {
		t.Errorf("EmbedText prefix wrong: %q", got)
	}

	w := Chunk{Path: "x.md", Kind: "window", Content: "hello"}
	got = w.EmbedText()
	// "window" kind is suppressed in the embed-text header to keep
	// noise out of plain text embeddings.
	if strings.Contains(got, "kind:") {
		t.Errorf("window chunks shouldn't emit a kind header: %q", got)
	}
}

func TestUTF8BoundaryInByteWindows(t *testing.T) {
	// Each `é` is 2 bytes in UTF-8; an oversized line of them must
	// never be sliced through a rune boundary.
	line := strings.Repeat("é", MaxBytes) // 2*MaxBytes bytes
	chunks, err := Chunks(context.Background(), "u.txt", []byte(line))
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range chunks {
		if !utf8.ValidString(c.Content) {
			t.Errorf("chunk %d contains invalid UTF-8: % x", i, []byte(c.Content)[:32])
		}
	}
}

// Sanity that the structural pass picks up Python defs/classes.
func TestPythonStructural(t *testing.T) {
	src := []byte(`"""Module docstring."""

def hello(name):
    """Say hi."""
    return f"hello {name}"

class Greeter:
    def __init__(self):
        self.x = 1
`)
	chunks, err := Chunks(context.Background(), "g.py", src)
	if err != nil {
		t.Fatal(err)
	}
	var sawFn, sawCls bool
	for _, c := range chunks {
		if c.Kind == "function_definition" && strings.Contains(c.Content, "def hello") {
			sawFn = true
		}
		if c.Kind == "class_definition" && strings.Contains(c.Content, "class Greeter") {
			sawCls = true
		}
	}
	if !sawFn {
		t.Error("expected function_definition chunk for hello()")
	}
	if !sawCls {
		t.Error("expected class_definition chunk for Greeter")
	}
}

func TestChunkNameExtraction(t *testing.T) {
	src := []byte("package main\n\nfunc cmdIndex(ctx context.Context) error { return nil }\n\nfunc helper() {}\n")
	chunks, err := Chunks(context.Background(), "main.go", src)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range chunks {
		if c.Name != "" {
			names[c.Name] = true
		}
	}
	for _, want := range []string{"cmdIndex", "helper"} {
		if !names[want] {
			t.Errorf("expected Name=%q in chunks; got names: %v", want, names)
		}
	}
}

func TestChunkGoTypeNameExtraction(t *testing.T) {
	// Go's `type_declaration` wraps one or more `type_spec` nodes;
	// the wrapper itself has no `name` field. Confirm we descend to
	// type_spec so single-spec types name their chunk correctly.
	src := []byte(`package x

// Hit is a search result.
type Hit struct {
	Path string
}

type Options struct {
	Verbose bool
}

type AliasedType = int

// Parenthesized block — only the first spec gets named (acceptable
// degradation, beats empty).
type (
	A struct{}
	B int
)
`)
	chunks, err := Chunks(context.Background(), "x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range chunks {
		if c.Kind == "type_declaration" && c.Name != "" {
			names[c.Name] = true
		}
	}
	for _, want := range []string{"Hit", "Options", "AliasedType"} {
		if !names[want] {
			t.Errorf("expected type-declaration chunk named %q; got names: %v", want, names)
		}
	}
	// Multi-spec block should still get a name (the first spec).
	if !names["A"] {
		t.Errorf("multi-spec type_declaration should yield the first spec name (A); got names: %v", names)
	}
}

func TestEmbedTextStampsName(t *testing.T) {
	c := Chunk{Path: "main.go", Kind: "function_declaration", Name: "cmdIndex", Content: "func cmdIndex() {}"}
	got := c.EmbedText()
	if !strings.Contains(got, "// name: cmdIndex\n") {
		t.Errorf("EmbedText missing name header: %q", got)
	}
	// windows and orphans have no Name — ensure no spurious header
	w := Chunk{Path: "main.go", Kind: "window", Content: "some code"}
	if strings.Contains(w.EmbedText(), "// name:") {
		t.Errorf("window chunk should not emit name header: %q", w.EmbedText())
	}
}

func TestLineCountsAreOneBased(t *testing.T) {
	src := []byte("package x\n\nfunc A() {}\n")
	chunks, _ := Chunks(context.Background(), "a.go", src)
	for _, c := range chunks {
		if c.StartLine < 1 {
			t.Errorf("StartLine = %d, want ≥1 (chunk: %q)", c.StartLine, c.Content)
		}
	}
}

func TestCSharpStructural(t *testing.T) {
	src := []byte(`using System;

namespace MyApp {
    public class MyService {
        public string GetName() => "hello";
        public int Compute(int x) { return x * 2; }
    }

    public interface IRunner {
        void Run();
    }

    public enum Status { Active, Inactive }
}
`)
	chunks, err := Chunks(context.Background(), "service.cs", src)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	names := map[string]bool{}
	for _, c := range chunks {
		kinds[c.Kind] = true
		if c.Name != "" {
			names[c.Name] = true
		}
	}
	if !kinds["namespace_declaration"] && !kinds["class_declaration"] {
		t.Errorf("expected namespace_declaration or class_declaration chunks; kinds: %v", kinds)
	}
	for _, want := range []string{"MyService", "IRunner"} {
		if !names[want] {
			t.Errorf("expected chunk named %q; names: %v", want, names)
		}
	}
}

func TestKotlinStructural(t *testing.T) {
	src := []byte(`package com.example

class MyClass {
    fun greet(): String = "hello"
    fun compute(x: Int): Int = x * 2
}

fun topLevel(): Int = 42

object Singleton {
    fun getInstance() = Singleton
}
`)
	chunks, err := Chunks(context.Background(), "main.kt", src)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range chunks {
		if c.Name != "" {
			names[c.Name] = true
		}
	}
	for _, want := range []string{"MyClass", "topLevel", "Singleton"} {
		if !names[want] {
			t.Errorf("expected chunk named %q; names: %v", want, names)
		}
	}
}

func TestSwiftStructural(t *testing.T) {
	src := []byte(`import Foundation

class MyService {
    func process() -> String { return "ok" }
    init() {}
}

struct Point {
    var x: Int
    func magnitude() -> Double { return Double(x) }
}

protocol Runnable {
    func run()
}

func topLevelHelper() -> Int { return 0 }
`)
	chunks, err := Chunks(context.Background(), "service.swift", src)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range chunks {
		if c.Name != "" {
			names[c.Name] = true
		}
	}
	for _, want := range []string{"MyService", "topLevelHelper"} {
		if !names[want] {
			t.Errorf("expected chunk named %q; names: %v", want, names)
		}
	}
	// Confirm structural chunking fired (not pure windows).
	hasStructural := false
	for _, c := range chunks {
		if c.Kind != "window" && c.Kind != "orphan" {
			hasStructural = true
			break
		}
	}
	if !hasStructural {
		t.Error("expected at least one structural (non-window) chunk for Swift source")
	}
}

func TestPHPStructural(t *testing.T) {
	src := []byte(`<?php
namespace App;

class UserService {
    public function getUser(int $id): string {
        return "user_" . $id;
    }

    public static function create(): self {
        return new self();
    }
}

function helperFunction(string $s): string {
    return strtoupper($s);
}

interface Repository {
    public function findById(int $id): string;
}
`)
	chunks, err := Chunks(context.Background(), "service.php", src)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range chunks {
		if c.Name != "" {
			names[c.Name] = true
		}
	}
	for _, want := range []string{"UserService", "helperFunction", "Repository"} {
		if !names[want] {
			t.Errorf("expected chunk named %q; names: %v", want, names)
		}
	}
}

func TestScalaStructural(t *testing.T) {
	src := []byte(`package com.example

class Calculator {
  def add(a: Int, b: Int): Int = a + b
  def multiply(a: Int, b: Int): Int = a * b
}

object MathUtils {
  def square(x: Int): Int = x * x
}

trait Printable {
  def print(): Unit
}

def standaloneFunc(x: Int): Int = x + 1
`)
	chunks, err := Chunks(context.Background(), "calc.scala", src)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range chunks {
		if c.Name != "" {
			names[c.Name] = true
		}
	}
	for _, want := range []string{"Calculator", "MathUtils", "Printable"} {
		if !names[want] {
			t.Errorf("expected chunk named %q; names: %v", want, names)
		}
	}
}

func TestElixirStructural(t *testing.T) {
	src := []byte(`defmodule MyApp.Calculator do
  def add(a, b) do
    a + b
  end

  defp validate(x) when is_integer(x), do: :ok

  def multiply(a, b), do: a * b
end
`)
	chunks, err := Chunks(context.Background(), "calculator.ex", src)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range chunks {
		if c.Name != "" {
			names[c.Name] = true
		}
	}
	if !names["MyApp.Calculator"] {
		t.Errorf("expected defmodule chunk named 'MyApp.Calculator'; names: %v", names)
	}
	// Should have structural chunks, not just windows.
	hasStructural := false
	for _, c := range chunks {
		if c.Kind != "window" && c.Kind != "orphan" {
			hasStructural = true
			break
		}
	}
	if !hasStructural {
		t.Error("expected structural chunks for Elixir source")
	}
}

// TestASTSiblingMergeOrphans verifies that orphan chunks produced for a
// known-language file use cAST sibling-merge: each orphan chunk covers
// complete AST nodes rather than arbitrary line boundaries.
func TestASTSiblingMergeOrphans(t *testing.T) {
	// A Go file with several top-level non-function declarations followed
	// by a function. The pre-function content (package + imports + consts
	// + vars) must end up as orphan chunks that each span complete AST
	// nodes — not as sliding-window fragments with overlap.
	src := []byte(`package main

import (
	"fmt"
	"os"
	"strings"
)

const Alpha = "a"
const Beta  = "b"

var GlobalX = 1
var GlobalY = 2

func Run() {
	fmt.Println(strings.Join(os.Args, " "))
}
`)
	chunks, err := Chunks(context.Background(), "x.go", src)
	if err != nil {
		t.Fatal(err)
	}

	var orphans []Chunk
	var sawFunc bool
	for _, c := range chunks {
		if c.Kind == KindOrphan {
			orphans = append(orphans, c)
		}
		if c.Kind == "function_declaration" {
			sawFunc = true
		}
	}
	if !sawFunc {
		t.Fatal("expected function_declaration chunk for Run")
	}
	if len(orphans) == 0 {
		t.Fatal("expected at least one orphan chunk for imports/consts/vars")
	}

	// No orphan chunk may contain partial overlap (WindowOverlap lines
	// repeated). Verify by checking that the union of orphan line ranges
	// has no duplicate line numbers — sibling-merge packs without overlap.
	linesSeen := map[int]string{}
	for _, o := range orphans {
		for ln := o.StartLine; ln <= o.EndLine; ln++ {
			if prev, dup := linesSeen[ln]; dup {
				t.Errorf("line %d appears in both orphan %q and %q — sliding-window overlap detected",
					ln, prev, o.Content[:min(40, len(o.Content))])
			}
			linesSeen[ln] = o.Content[:min(40, len(o.Content))]
		}
	}

	// Each orphan chunk must contain at least one non-blank line.
	for i, o := range orphans {
		if strings.TrimSpace(o.Content) == "" {
			t.Errorf("orphan chunk %d is blank", i)
		}
	}

	// The import block should be self-contained in one orphan chunk.
	var importChunk *Chunk
	for i := range orphans {
		if strings.Contains(orphans[i].Content, `"fmt"`) {
			importChunk = &orphans[i]
			break
		}
	}
	if importChunk == nil {
		t.Error("expected an orphan chunk containing the import block")
	} else if !strings.Contains(importChunk.Content, `"os"`) || !strings.Contains(importChunk.Content, `"strings"`) {
		// The whole import declaration should be in one chunk.
		t.Errorf("import block split across chunks; got: %q", importChunk.Content)
	}
}

// TestASTSiblingMergeNoOverlap confirms that sibling-merge orphan chunks
// never duplicate lines (the old sliding-window produced overlap).
func TestASTSiblingMergeNoOverlap(t *testing.T) {
	// Build a Go file whose header section is large enough that the old
	// WindowLines=40 sliding-window would have produced overlapping windows.
	var sb strings.Builder
	sb.WriteString("package big\n\n")
	for i := range 60 {
		fmt.Fprintf(&sb, "const C%d = %d\n", i, i)
	}
	sb.WriteString("\nfunc Sentinel() {}\n")
	src := []byte(sb.String())

	chunks, err := Chunks(context.Background(), "big.go", src)
	if err != nil {
		t.Fatal(err)
	}

	// Collect all orphan line ranges and verify no line is repeated.
	seen := map[int]bool{}
	for _, c := range chunks {
		if c.Kind != KindOrphan {
			continue
		}
		for ln := c.StartLine; ln <= c.EndLine; ln++ {
			if seen[ln] {
				t.Errorf("line %d appears in more than one orphan chunk (sliding-window overlap)", ln)
			}
			seen[ln] = true
		}
	}
}

// TestASTSiblingMergeWholeFileNoStructural verifies that a Go file
// containing only imports and constants (no functions) is packed via
// AST sibling-merge rather than falling through to line windows.
func TestASTSiblingMergeWholeFileNoStructural(t *testing.T) {
	src := []byte(`package cfg

import "os"

const Host = "localhost"
const Port = 8080
const Debug = false
`)
	chunks, err := Chunks(context.Background(), "cfg.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected chunks for const-only Go file")
	}
	for _, c := range chunks {
		if c.Kind == KindWindow {
			t.Errorf("const-only Go file produced a window chunk — expected orphan from AST merge: %q", c.Content)
		}
	}
	// Must cover the const declarations.
	all := ""
	for _, c := range chunks {
		all += c.Content
	}
	for _, want := range []string{"Host", "Port", "Debug"} {
		if !strings.Contains(all, want) {
			t.Errorf("const %q not found in any chunk", want)
		}
	}
}
