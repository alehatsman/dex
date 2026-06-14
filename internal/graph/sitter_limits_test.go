package graph

import (
	"context"
	"strings"
	"testing"
)

// TestExtractSitterSkipsOversizedFiles locks the file-size cap (#443): a file
// larger than maxParseFileSize is skipped before the extractor reads or
// parses it, with a warning, while normal files in the same tree still parse.
func TestExtractSitterSkipsOversizedFiles(t *testing.T) {
	big := strings.Repeat("a = 1\n", (maxParseFileSize/6)+1000) // > maxParseFileSize
	if len(big) <= maxParseFileSize {
		t.Fatalf("test setup: big file %d not over cap %d", len(big), maxParseFileSize)
	}
	root := writeTree(t, map[string]string{
		"small.py": "x = 1\n",
		"huge.py":  big,
	})

	stub := &stubExtractor{name: "pystub", extensions: []string{".py"}}
	reg := NewRegistry()
	reg.Register(func() Extractor { return stub })

	res, err := ExtractSitterWith(context.Background(), root, reg)
	if err != nil {
		t.Fatalf("ExtractSitterWith: %v", err)
	}

	for _, v := range stub.visited {
		if v == "huge.py" {
			t.Errorf("oversized huge.py was parsed; expected skip. visited=%v", stub.visited)
		}
	}
	var sawSmall bool
	for _, v := range stub.visited {
		if v == "small.py" {
			sawSmall = true
		}
	}
	if !sawSmall {
		t.Errorf("small.py should still be parsed; visited=%v", stub.visited)
	}

	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "huge.py") && strings.Contains(w, "exceeds") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected a skip warning naming huge.py; warnings=%v", res.Warnings)
	}
}

// TestExtractSitterDeepNestingDoesNotOverflow feeds a Java method whose body
// nests thousands of levels deep. The query-driven extractor recovers scope
// with iterative ancestor walks (no recursive descent), so extraction must
// complete without a stack overflow and still surface the enclosing
// method (#443).
func TestExtractSitterDeepNestingDoesNotOverflow(t *testing.T) {
	const depth = 6000
	expr := strings.Repeat("(", depth) + "1" + strings.Repeat(")", depth)
	src := "package p;\nclass C {\n  int m() { return " + expr + "; }\n}\n"
	if len(src) > maxParseFileSize {
		t.Fatalf("test setup: src %d exceeds parse cap and would be skipped", len(src))
	}
	root := writeTree(t, map[string]string{"C.java": src})

	res, err := ExtractSitter(context.Background(), root)
	if err != nil {
		t.Fatalf("ExtractSitter on deeply nested input: %v", err)
	}
	var sawMethod bool
	for _, n := range res.Nodes {
		if n.Kind == NodeMethod && n.Name == "m" {
			sawMethod = true
		}
	}
	if !sawMethod {
		t.Errorf("method m should extract despite deep nesting; nodes=%v", nodeQNs(res.Nodes))
	}
}
