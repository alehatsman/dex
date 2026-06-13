// Package compress provides an offline deterministic benchmark for the
// compress engine: ratio, anchor preservation, extractive fidelity, and
// round-trip correctness for lossless passes. Zero inference — no embed or
// chat calls at runtime.
package compress

// Sample is a single benchmark fixture: a representative block of content, a
// set of anchor tokens that must survive compression verbatim, and a set of
// answer spans whose presence in the output is tested for extractive fidelity.
type Sample struct {
	Name    string   // short label used in the report
	Kind    string   // code | log | diff | prose
	Content string   // raw input text
	Anchors []string // critical tokens: paths, identifiers, line numbers
	Spans   []string // answer spans that must survive as substrings
}

// BuiltinCorpus is a small, embedded set of representative samples across the
// four content modalities dex commonly compresses. Each sample is real-ish but
// short enough to be embedded without an external file.
var BuiltinCorpus = []Sample{
	{
		Name: "go-func",
		Kind: "code",
		Content: `// Package store provides the on-disk index for dex.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrEmbedModelMismatch is returned when the indexed model differs from the
// current embed model, preventing vector space corruption.
var ErrEmbedModelMismatch = errors.New("embed model mismatch")

// Stats holds aggregate index statistics returned by Store.Stats.
type Stats struct {
	ChunkCount   int
	EmbedModel   string
	LastIndexed  time.Time
}

// Search returns the top-k chunks most relevant to the query. When queryVec
// is non-nil it uses hybrid BM25+vector fusion (RRF); otherwise falls back to
// BM25-only.
func (s *Store) Search(ctx context.Context, queryVec []float32, queryText string, k int) ([]Hit, error) {
	if len(queryVec) == 0 {
		return s.scoreBM25(ctx, queryText, k)
	}
	hits, err := s.searchHybrid(ctx, queryVec, queryText, k)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return hits, nil
}

// Stats returns aggregate statistics for the current index.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	row := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(MAX(embed_model),'') FROM chunks")
	if err := row.Scan(&st.ChunkCount, &st.EmbedModel); err != nil {
		return st, fmt.Errorf("stats: %w", err)
	}
	return st, nil
}
`,
		Anchors: []string{
			"ErrEmbedModelMismatch",
			"Store.Search",
			"Store.Stats",
			"internal/store",
			"queryVec",
			"scoreBM25",
			"searchHybrid",
		},
		Spans: []string{
			"ErrEmbedModelMismatch = errors.New",
			"func (s *Store) Search",
			"func (s *Store) Stats",
		},
	},
	{
		Name: "go-test-output",
		Kind: "log",
		Content: `--- FAIL: TestSearch (0.43s)
    store_test.go:142: Search: want 3 hits, got 1
    store_test.go:143: first hit: internal/store/store.go (score=0.82)
    store_test.go:147: expected internal/store/bench_test.go in results
--- FAIL: TestBM25Fallback (0.11s)
    store_test.go:201: BM25Fallback: query "embed model mismatch" should hit ErrEmbedModelMismatch
    store_test.go:202:     got: []Hit{}
--- PASS: TestOpen (0.05s)
--- PASS: TestUpsertMany (0.28s)
--- PASS: TestStats (0.03s)
FAIL
FAIL    github.com/alehatsman/dex/internal/store        0.91s
ok      github.com/alehatsman/dex/internal/compress     0.14s
ok      github.com/alehatsman/dex/internal/tokens       0.07s
`,
		Anchors: []string{
			"TestSearch",
			"store_test.go:142",
			"internal/store/store.go",
			"ErrEmbedModelMismatch",
			"TestBM25Fallback",
		},
		Spans: []string{
			"store_test.go:142: Search: want 3 hits, got 1",
			"FAIL\tgithub.com/alehatsman/dex/internal/store",
		},
	},
	{
		Name: "git-diff",
		Kind: "diff",
		Content: `diff --git a/internal/store/store.go b/internal/store/store.go
index 3a1bc2f..9d4e8c1 100644
--- a/internal/store/store.go
+++ b/internal/store/store.go
@@ -161,6 +161,14 @@ func Open(ctx context.Context, path string) (*Store, error) {
 	return OpenWith(ctx, path, Options{})
 }

+// OpenWith opens the index at path with custom options, creating it if
+// necessary. Callers that need to disable BM25 or tune graph weights should
+// prefer this over Open.
+func OpenWith(ctx context.Context, path string, opts Options) (*Store, error) {
+	s := &Store{opts: opts}
+	if err := s.init(ctx, path); err != nil {
+		return nil, err
+	}
+	return s, nil
+}
+
 // Close releases the underlying database handle.
 func (s *Store) Close() error { return s.db.Close() }
`,
		Anchors: []string{
			"internal/store/store.go",
			"OpenWith",
			"Open",
			"Options",
			"s.init",
		},
		Spans: []string{
			"func OpenWith(ctx context.Context, path string, opts Options) (*Store, error)",
			"func (s *Store) Close() error",
		},
	},
	{
		Name: "markdown-prose",
		Kind: "prose",
		Content: `# dex — semantic search context router

dex is a local MCP server that indexes a repository and serves
find, lookup, and graph_* tools to Claude Code.

## Architecture

The retrieval core runs three lanes in parallel:

1. **Dense vector lane** — embed(query) →  cosine KNN over sqlite-vec.
2. **Lexical lane (BM25)** — FTS5 index over chunk content + context.
3. **Graph lane** — k-hop expansion along call/import edges with hop-decay.

Results from all three lanes are fused with Reciprocal Rank Fusion (RRF)
and optionally re-ranked by a cross-encoder (Qwen3-Reranker-4B).

## Context optimizer

The compress engine runs in the redirect hook (PreToolUse) and the proxy
(ANTHROPIC_BASE_URL pass-through). It reduces tool-result tokens before
they enter the context window, using entropy filtering, structural
stripping, and a symbol-map abbreviation pass.

## Configuration

dex is configured via .dex/config.yml in each indexed project.
The embed endpoint defaults to http://localhost:11434 (ollama-compatible).
`,
		Anchors: []string{
			"find",
			"lookup",
			"graph_*",
			"sqlite-vec",
			"BM25",
			"RRF",
			"Qwen3-Reranker-4B",
			".dex/config.yml",
			"ANTHROPIC_BASE_URL",
		},
		Spans: []string{
			"Results from all three lanes are fused with Reciprocal Rank Fusion (RRF)",
			"dex is configured via .dex/config.yml",
		},
	},
}
