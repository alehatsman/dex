package chunk

import (
	"context"
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
