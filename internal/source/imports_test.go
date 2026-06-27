package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractGoImports_BlockForm(t *testing.T) {
	lines := []string{
		`package main`,
		``,
		`import (`,
		`	"fmt"`,
		`	"os"`,
		`)`,
		``,
		`func main() {}`,
	}
	got := ExtractGoImports(lines)
	if !strings.Contains(got, `"fmt"`) || !strings.Contains(got, `"os"`) {
		t.Errorf("missing imports in block form output: %q", got)
	}
	if strings.Contains(got, "func main") {
		t.Errorf("function body leaked into import output: %q", got)
	}
}

func TestExtractGoImports_SingleLine(t *testing.T) {
	lines := []string{
		`package main`,
		``,
		`import "fmt"`,
		`import "os"`,
		``,
		`func main() {}`,
	}
	got := ExtractGoImports(lines)
	if !strings.Contains(got, `import "fmt"`) || !strings.Contains(got, `import "os"`) {
		t.Errorf("missing single-line imports: %q", got)
	}
	if strings.Contains(got, "func main") {
		t.Errorf("function body leaked: %q", got)
	}
}

func TestExtractGoImports_NoImports(t *testing.T) {
	lines := []string{`package main`, ``, `func main() {}`}
	got := ExtractGoImports(lines)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractJSImports_ESModule(t *testing.T) {
	lines := []string{
		`import React from "react";`,
		`import { useState, useEffect } from "react";`,
		``,
		`export default function App() {}`,
	}
	got := ExtractJSImports(lines)
	if !strings.Contains(got, `import React`) || !strings.Contains(got, `import { useState`) {
		t.Errorf("missing ES module imports: %q", got)
	}
	if strings.Contains(got, "export default") {
		t.Errorf("export leaked into import output: %q", got)
	}
}

func TestExtractJSImports_MultilineNamed(t *testing.T) {
	lines := []string{
		`import {`,
		`  alpha,`,
		`  beta,`,
		`} from "module";`,
		``,
		`const x = 1;`,
	}
	got := ExtractJSImports(lines)
	if !strings.Contains(got, "alpha") || !strings.Contains(got, `"module"`) {
		t.Errorf("multiline named import not captured: %q", got)
	}
	if strings.Contains(got, "const x") {
		t.Errorf("const leaked into import output: %q", got)
	}
}

func TestExtractJSImports_Require(t *testing.T) {
	lines := []string{
		`const fs = require("fs");`,
		`const path = require('path');`,
		``,
		`module.exports = {};`,
	}
	got := ExtractJSImports(lines)
	if !strings.Contains(got, `require("fs")`) || !strings.Contains(got, `require('path')`) {
		t.Errorf("require not captured: %q", got)
	}
}

func TestExtractJSImports_NoImports(t *testing.T) {
	lines := []string{`const x = 1;`, `console.log(x);`}
	got := ExtractJSImports(lines)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestReadLineRange_Basic(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "readrange*.txt")
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{"line1", "line2", "line3", "line4", "line5"}
	for _, l := range lines {
		f.WriteString(l + "\n")
	}
	f.Close()

	got, truncated, err := ReadLineRange(f.Name(), 2, 4, 100, 10000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if truncated {
		t.Error("expected truncated=false")
	}
	if !strings.Contains(got, "line2") || !strings.Contains(got, "line4") {
		t.Errorf("expected lines 2-4, got %q", got)
	}
	if strings.Contains(got, "line1") || strings.Contains(got, "line5") {
		t.Errorf("out-of-range lines in output: %q", got)
	}
}

func TestReadLineRange_MaxLinesCap(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "readrange*.txt")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		f.WriteString("line\n")
	}
	f.Close()

	_, truncated, err := ReadLineRange(f.Name(), 1, 10, 3, 10000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !truncated {
		t.Error("expected truncated=true when maxLines fires")
	}
}

func TestReadLineRange_MissingFile(t *testing.T) {
	_, _, err := ReadLineRange(filepath.Join(t.TempDir(), "no-such-file.txt"), 1, 5, 100, 10000)
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestExtractImports_GoFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "foo.go")
	content := "package foo\n\nimport (\n\t\"fmt\"\n)\n\nfunc Foo() {}\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ExtractImports(p)
	if !strings.Contains(got, `"fmt"`) {
		t.Errorf("ExtractImports: expected fmt import, got %q", got)
	}
}

func TestExtractImports_UnknownExtension(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(p, []byte("just text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ExtractImports(p)
	if got != "" {
		t.Errorf("expected empty for unknown extension, got %q", got)
	}
}
