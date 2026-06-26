package chunk

import (
	"context"
	"strings"
	"testing"
)

// assertEveryNonBlankLineCovered locks the lossless-coverage invariant (#655):
// the structural + orphan chunks together must cover every non-blank source
// line. A line that falls through a gap is silently unsearchable (no chunk → no
// embedding → no BM25 row).
func assertEveryNonBlankLineCovered(t *testing.T, relPath, source string) {
	t.Helper()
	chunks, err := Chunks(context.Background(), relPath, []byte(source))
	if err != nil {
		t.Fatalf("Chunks(%s): %v", relPath, err)
	}
	covered := map[int]bool{}
	for _, c := range chunks {
		for ln := c.StartLine; ln <= c.EndLine; ln++ {
			covered[ln] = true
		}
	}
	for i, line := range strings.Split(source, "\n") {
		ln := i + 1 // chunk line numbers are 1-based
		if strings.TrimSpace(line) == "" {
			continue // blank lines need no coverage
		}
		if !covered[ln] {
			t.Errorf("%s line %d not covered by any chunk: %q", relPath, ln, line)
		}
	}
}

func TestChunksCoverEveryNonBlankLineGo(t *testing.T) {
	// A realistic mix: package, doc comments, import block, const, var, a type
	// with a method, a free function, and a trailing comment + var — exercising
	// the structural (func/method/type) ∪ orphan (header/imports/vars/trailer)
	// composition and backfillComments.
	src := `package demo

// Package demo is a coverage fixture.

import (
	"fmt"
	"os"
)

// Version is the build version.
const Version = "1.0"

var global = 42

// Greeter greets by name.
type Greeter struct {
	name string
}

// Hello returns a greeting.
func (g Greeter) Hello() string {
	return "hi " + g.name
}

// Run is the entrypoint helper.
func Run() {
	fmt.Println(os.Args)
	_ = global
}

// tail wires the version through at the end of the file.
var tail = Version
`
	assertEveryNonBlankLineCovered(t, "demo.go", src)
}

func TestChunksCoverEveryNonBlankLinePython(t *testing.T) {
	src := `"""Module docstring."""

import os
import sys

VERSION = "1.0"

GLOBAL = 42


# A standalone comment block before the class.
class Greeter:
    """Greets by name."""

    def __init__(self, name):
        self.name = name

    def hello(self):
        return "hi " + self.name


def run():
    print(os.environ, sys.argv)
    return GLOBAL


TAIL = VERSION
`
	assertEveryNonBlankLineCovered(t, "demo.py", src)
}
