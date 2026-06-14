package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// largeFileFixture indexes a project containing a single >800-line Go file with
// many exported functions. It returns the resolved project root, cache dir, and
// the relative file name. Used by the skeleton (#483) and budget (#487)
// regression tests, both of which only misbehaved on large inputs.
func largeFileFixture(t *testing.T) (projRoot, cacheDir, fileName string) {
	t.Helper()
	embedSrv := fakeEmbed(t, 16)
	t.Cleanup(embedSrv.Close)

	var b strings.Builder
	b.WriteString("package big\n\n")
	// 120 funcs * ~7 lines each ≈ 840 lines, comfortably over the 800-line bar.
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&b, "// Handler%03d does work item %d and returns an error on failure.\n", i, i)
		fmt.Fprintf(&b, "func Handler%03d(in int) (int, error) {\n", i)
		fmt.Fprintf(&b, "\tacc := in + %d\n", i)
		b.WriteString("\tfor j := 0; j < 4; j++ {\n")
		b.WriteString("\t\tacc += j\n")
		b.WriteString("\t}\n")
		b.WriteString("\treturn acc, nil\n")
		b.WriteString("}\n\n")
	}

	projDir := t.TempDir()
	fileName = "big.go"
	// go.mod so the graph extractor resolves the package and emits func nodes;
	// SymbolsByFile reads from graph_nodes, and skeleton/handle modes need them.
	writeFile(t, projDir+"/go.mod", "module example.com/big\n\ngo 1.21\n")
	writeFile(t, projDir+"/"+fileName, b.String())
	cacheDir = t.TempDir()
	projRoot = indexProject(t, projDir, cacheDir, embedSrv.URL)

	// index.Run does not build the call graph; do it explicitly so
	// SymbolsByFile returns the file's functions.
	p, err := proj.Resolve(projRoot, cacheDir)
	if err != nil {
		t.Fatalf("resolve project: %v", err)
	}
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := graph.New(p, graph.NewStoreAdapter(st), graph.Options{}).Run(context.Background()); err != nil {
		t.Fatalf("build graph: %v", err)
	}
	return projRoot, cacheDir, fileName
}

// TestSkeletonNeverDegradesToLLMOnLargeFile locks issue #483: `read
// --mode=skeleton` is a deterministic, no-LLM contract. On a large file the
// bounce escalator (#98) used to swap skeleton → LLM `summary`, returning prose
// with a model field. Skeleton must stay deterministic — @Bn body handles, no
// model — regardless of file size or re-reads, even with a healthy chat client.
func TestSkeletonNeverDegradesToLLMOnLargeFile(t *testing.T) {
	projRoot, cacheDir, fileName := largeFileFixture(t)
	ctx := context.Background()

	assertSkeleton := func(t *testing.T, out SummarizeOutput) {
		t.Helper()
		if out.Status != "ok" {
			t.Fatalf("status = %q, want ok", out.Status)
		}
		if out.Model != "" || out.Endpoint != "" {
			t.Errorf("skeleton engaged an LLM (model=%q endpoint=%q); the no-LLM contract is broken", out.Model, out.Endpoint)
		}
		if !strings.Contains(out.Content, "@B") {
			t.Errorf("skeleton output missing @Bn body handles; got:\n%s", out.Content)
		}
	}

	t.Run("single read, no chat wired", func(t *testing.T) {
		s := chatDownServer(t, cacheDir, nil)
		_, out, err := s.summarize(ctx, nil, SummarizeInput{Path: fileName, Mode: "skeleton", ProjectRoot: projRoot})
		if err != nil {
			t.Fatalf("skeleton returned hard error: %v", err)
		}
		assertSkeleton(t, out)
	})

	t.Run("re-read with healthy chat never swaps to LLM", func(t *testing.T) {
		const llmProse = "this is LLM prose that must never be returned for skeleton"
		chatSrv := fakeChat(t, llmProse)
		defer chatSrv.Close()
		s := chatDownServer(t, cacheDir, chat.New(chatSrv.URL, "fake", 5*time.Second))

		// First read records a compressed delivery; the second read within the
		// bounce window triggers escalateOnBounce. Pre-fix this swapped skeleton
		// to an LLM summary (model set, LLM prose). Post-fix the bounce escalates
		// skeleton to the raw `full` view — deterministic, no chat model.
		for i := 0; i < 2; i++ {
			_, out, err := s.summarize(ctx, nil, SummarizeInput{Path: fileName, Mode: "skeleton", ProjectRoot: projRoot})
			if err != nil {
				t.Fatalf("skeleton read %d returned hard error: %v", i, err)
			}
			if out.Status != "ok" {
				t.Fatalf("read %d status = %q, want ok", i, out.Status)
			}
			if out.Model != "" || out.Endpoint != "" {
				t.Errorf("read %d engaged an LLM (model=%q endpoint=%q); skeleton must stay deterministic on bounce", i, out.Model, out.Endpoint)
			}
			if strings.Contains(out.Content, llmProse) {
				t.Errorf("read %d returned LLM prose; skeleton degraded to summary", i)
			}
		}
	})
}

// TestBudgetDowngradeNeverDumpsRawFile locks issue #487's observable contract:
// budget_tokens too small for the requested view auto-downgrades to a structural
// view (map or smaller), never the raw full file. The bug returned the whole
// ~45KB file because an unrenderable affordable mode fell through to
// summarizeModeRaw.
func TestBudgetDowngradeNeverDumpsRawFile(t *testing.T) {
	projRoot, cacheDir, fileName := largeFileFixture(t)
	ctx := context.Background()
	s := chatDownServer(t, cacheDir, nil)

	_, out, err := s.summarize(ctx, nil, SummarizeInput{
		Path:         fileName,
		Mode:         "skeleton",
		BudgetTokens: 800,
		ProjectRoot:  projRoot,
	})
	if err != nil {
		t.Fatalf("budgeted read returned hard error: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok", out.Status)
	}
	// The raw file is ~5KB and is the only view that carries the dense function
	// bodies. A budget-clamped structural view (map/handle) must not contain them.
	if strings.Contains(out.Content, "for j := 0; j < 4; j++") {
		t.Errorf("budgeted read leaked raw file bodies — fell through to summarizeModeRaw:\n%.500s", out.Content)
	}
	if out.Model != "" || out.Endpoint != "" {
		t.Errorf("budget downgrade engaged an LLM (model=%q endpoint=%q)", out.Model, out.Endpoint)
	}
}

// TestHandleModeIsCompactNotRaw locks the direct #487 fix: `mode == "handle"`
// (the cheapest terminal of the downgrade chain) used to have no dispatch case
// and fell through to summarizeModeRaw, dumping the full file. It must now
// render a compact body-handle stub.
func TestHandleModeIsCompactNotRaw(t *testing.T) {
	projRoot, cacheDir, fileName := largeFileFixture(t)
	ctx := context.Background()
	s := chatDownServer(t, cacheDir, nil)

	_, out, err := s.summarize(ctx, nil, SummarizeInput{Path: fileName, Mode: "handle", ProjectRoot: projRoot})
	if err != nil {
		t.Fatalf("handle read returned hard error: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok", out.Status)
	}
	if strings.Contains(out.Content, "for j := 0; j < 4; j++") {
		t.Fatalf("handle mode dumped the raw file (fell through to summarizeModeRaw):\n%.500s", out.Content)
	}
	if !strings.HasPrefix(out.Content, "HANDLE ") {
		t.Errorf("handle mode did not emit a compact stub; got:\n%.300s", out.Content)
	}
	if out.Model != "" || out.Endpoint != "" {
		t.Errorf("handle mode engaged an LLM (model=%q endpoint=%q)", out.Model, out.Endpoint)
	}
	// Compact: a one-line pointer plus a small sample of handles, far under the
	// raw file. 120 symbols listed in full would be ~4.5KB; the stub must not be.
	if out.Bytes > 800 {
		t.Errorf("handle stub is %d bytes; expected a compact pointer, not the full symbol list", out.Bytes)
	}
}
