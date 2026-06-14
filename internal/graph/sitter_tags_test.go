package graph

import (
	"context"
	"testing"
)

// extractTags copies testdata/<fixture>, runs it through a single tags
// extractor, and returns the resulting graph. It is the post-#498
// replacement for the walker-vs-tags parity helpers: the walkers are gone,
// so the precision oracle is now the committed corpus trace baseline
// (benchmark/trace/baseline-corpus.json); these unit tests only guard that
// each tags extractor still produces a well-formed graph on a small fixture.
func extractTags(t *testing.T, fixture string, factory ExtractorFactory) *ExtractResult {
	t.Helper()
	root := copyFixture(t, fixture)
	reg := NewRegistry()
	reg.Register(factory)
	res, err := ExtractSitterWith(context.Background(), root, reg)
	if err != nil {
		t.Fatalf("extract %s: %v", fixture, err)
	}
	return res
}

// TestTagsExtractorsSmoke runs every language's query-driven extractor over
// its simple fixture and asserts a well-formed graph: a package node, a file
// node, at least one function node, edges, and correct sitter_lang
// provenance on every edge.
func TestTagsExtractorsSmoke(t *testing.T) {
	cases := []struct {
		lang    string
		fixture string
		factory ExtractorFactory
	}{
		{"python", "python_simple", newPythonTagsExtractor},
		{"typescript", "ts_simple", newTSTagsExtractor},
		{"javascript", "js_simple", newJSTagsExtractor},
		{"rust", "rust_simple", newRustTagsExtractor},
		{"java", "java_simple", newJavaTagsExtractor},
	}
	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			res := extractTags(t, tc.fixture, tc.factory)

			var pkgs, files, funcs int
			for _, n := range res.Nodes {
				switch n.Kind {
				case NodePackage:
					pkgs++
				case NodeFile:
					files++
				case NodeFunction, NodeMethod:
					funcs++
				}
			}
			if pkgs == 0 || files == 0 || funcs == 0 {
				t.Fatalf("%s: malformed graph: packages=%d files=%d funcs=%d",
					tc.lang, pkgs, files, funcs)
			}
			if len(res.Edges) == 0 {
				t.Fatalf("%s: no edges produced", tc.lang)
			}
			for _, e := range res.Edges {
				if got, _ := e.Metadata["sitter_lang"].(string); got != tc.lang {
					t.Fatalf("%s: edge has sitter_lang=%q, want %q", tc.lang, got, tc.lang)
				}
			}
		})
	}
}
