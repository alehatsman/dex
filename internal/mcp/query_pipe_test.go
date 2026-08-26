package mcp

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/tokens"
)

// TestSplitPipe locks the top-level `|` parser, including the one subtlety: a
// `|` inside a leading /regex/ seed is not a separator.
func TestSplitPipe(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"single", []string{"single"}},
		{"a | b | c", []string{"a", "b", "c"}},
		{"a|b", []string{"a", "b"}},
		{"  x  |  y  ", []string{"x", "y"}},
		{"internal/store | callers | impact", []string{"internal/store", "callers", "impact"}},
		{"/a|b/ | callers", []string{"/a|b/", "callers"}},
		{"/foo/ | callers | impact", []string{"/foo/", "callers", "impact"}},
		{"how are edits debounced | callees | assemble:6000", []string{"how are edits debounced", "callees", "assemble:6000"}},
	}
	for _, c := range cases {
		if got := splitPipe(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitPipe(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// TestParseStage covers the op:arg split used for terminals like assemble:6000.
func TestParseStage(t *testing.T) {
	cases := []struct{ in, name, arg string }{
		{"callers", "callers", ""},
		{"assemble:6000", "assemble", "6000"},
		{" Signatures ", "signatures", ""},
		{"assemble:", "assemble", ""},
	}
	for _, c := range cases {
		n, a := parseStage(c.in)
		if n != c.name || a != c.arg {
			t.Errorf("parseStage(%q) = (%q,%q), want (%q,%q)", c.in, n, a, c.name, c.arg)
		}
	}
}

func TestWeakenProvenance(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"exact", "exact", "exact"},
		{"exact", "name-based", "name-based"},
		{"name-based", "semantic", "semantic"},
		{"semantic", "exact", "semantic"},
	}
	for _, c := range cases {
		if got := weakenProvenance(c.a, c.b); got != c.want {
			t.Errorf("weakenProvenance(%q,%q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

// pipeFixture indexes a tiny caller→callee graph and returns a driver.
func pipeFixture(t *testing.T) (context.Context, *Server, string, func(QueryInput) QueryOutput) {
	t.Helper()
	srv := fakeEmbed(t, 16)
	t.Cleanup(srv.Close)
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	// leaf ← mid ← root: a two-hop call chain so callers/callees/impact all bite.
	src := "package main\n\n" +
		"func pipeLeaf() int { return 7 }\n\n" +
		"func pipeMid() int { return pipeLeaf() + 1 }\n\n" +
		"func pipeRoot() int { return pipeMid() + 1 }\n"
	writeFile(t, filepath.Join(projDir, "main.go"), src)
	root := indexProject(t, projDir, cacheDir, srv.URL)
	ctx := context.Background()
	seedCallGraph(t, ctx, root, cacheDir)
	h := newServer(srv.URL, cacheDir)
	call := func(in QueryInput) QueryOutput {
		in.ProjectRoot = root
		_, out, err := queryVerb(ctx, h, &sdk.CallToolRequest{}, in)
		if err != nil {
			t.Fatalf("queryVerb(%q): %v", in.Input, err)
		}
		return out
	}
	return ctx, h, root, call
}

// seedCallGraph writes a synthetic calls chain (pipeRoot → pipeMid → pipeLeaf)
// directly via the store, the way context_test.seedGraph does — ExtractGo needs
// a real go.mod, so tests hand-build the graph. The nodes double as the
// SymbolsByFile source the file→symbol coercion reads.
func seedCallGraph(t *testing.T, ctx context.Context, root, cacheDir string) {
	t.Helper()
	p, err := proj.Resolve(root, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	mk := func(name string, start int) store.GraphNodeRow {
		return store.GraphNodeRow{
			ID: "m::main::func::" + name, Kind: string(graph.NodeFunction),
			Name: name, QualifiedName: name, PackagePath: "main", FilePath: "main.go",
			StartLine: start, EndLine: start + 1, MetadataJSON: []byte("{}"), ContentHash: "n-" + name,
		}
	}
	leaf, mid, rootN := mk("pipeLeaf", 3), mk("pipeMid", 5), mk("pipeRoot", 7)
	if err := st.GraphUpsertNodes(ctx, []store.GraphNodeRow{leaf, mid, rootN}, now); err != nil {
		t.Fatal(err)
	}
	edges := []store.GraphEdgeRow{
		{ID: "e-mid-leaf", Kind: string(graph.EdgeCalls), SrcID: mid.ID, DstID: leaf.ID,
			FilePath: "main.go", StartLine: 5, EndLine: 6, MetadataJSON: []byte("{}"), ContentHash: "c1"},
		{ID: "e-root-mid", Kind: string(graph.EdgeCalls), SrcID: rootN.ID, DstID: mid.ID,
			FilePath: "main.go", StartLine: 7, EndLine: 8, MetadataJSON: []byte("{}"), ContentHash: "c2"},
	}
	if err := st.GraphUpsertEdges(ctx, edges, now); err != nil {
		t.Fatal(err)
	}
}

func refIDs(refs []Ref) []string {
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// TestPipeCorrectnessMatchesManual is the go/no-go correctness gate (spec §1):
// `pipeLeaf | callers` yields the same refs as a manual trace(callers) call.
func TestPipeCorrectnessMatchesManual(t *testing.T) {
	ctx, h, root, call := pipeFixture(t)

	// Manual single lane.
	_, to, err := traceVerb(ctx, h, &sdk.CallToolRequest{}, TraceInput{
		Symbol: "pipeLeaf", Direction: "callers", ProjectRoot: root,
	})
	if err != nil {
		t.Fatalf("manual trace: %v", err)
	}
	manual := refIDs(refsFromTrace(&to))

	// Piped.
	out := call(QueryInput{Input: "pipeLeaf | callers"})
	if out.Status != "ok" {
		t.Fatalf("pipe status = %q, want ok (%+v)", out.Status, out.Trust)
	}
	if out.Route.Detected != "pipe" {
		t.Fatalf("detected = %q, want pipe", out.Route.Detected)
	}
	piped := refIDs(out.Refs)
	if !reflect.DeepEqual(manual, piped) {
		t.Fatalf("piped refs %v != manual refs %v", piped, manual)
	}
	// pipeMid calls pipeLeaf, so it must be among the callers.
	if !hasID(piped, "main.pipeMid") && !containsSuffix(piped, "pipeMid") {
		t.Errorf("expected pipeMid among callers, got %v", piped)
	}
	// stages echoes the seed lane then the transform.
	if len(out.Route.Stages) != 2 || out.Route.Stages[1] != "callers" {
		t.Errorf("stages = %v, want [<seed> callers]", out.Route.Stages)
	}
}

// TestPipeChainThreeStages proves left-to-right composition: seed | callers |
// impact runs impact over the callers set.
func TestPipeChainThreeStages(t *testing.T) {
	_, _, _, call := pipeFixture(t)
	out := call(QueryInput{Input: "pipeLeaf | callers | impact"})
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (caveat=%q)", out.Status, out.Trust.Caveat)
	}
	if out.Result.Trace == nil {
		t.Fatalf("final transform should leave a trace body, got %+v", out.Result)
	}
	if len(out.Route.Stages) != 3 || out.Route.Stages[1] != "callers" || out.Route.Stages[2] != "impact" {
		t.Errorf("stages = %v, want [<seed> callers impact]", out.Route.Stages)
	}
}

// TestPipeFileSeedCoercionWeakensTrust: a file seed coerces file→symbols, so an
// otherwise-exact callees walk reports name-based provenance (weakest link).
func TestPipeFileSeedCoercion(t *testing.T) {
	_, _, _, call := pipeFixture(t)
	out := call(QueryInput{Input: "main.go | callees"})
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (caveat=%q)", out.Status, out.Trust.Caveat)
	}
	if out.Trust.Provenance != "name-based" {
		t.Errorf("provenance = %q, want name-based (coercion is the weak link)", out.Trust.Provenance)
	}
	// callees of the file's functions include the inner calls (pipeLeaf/pipeMid).
	if len(out.Refs) == 0 {
		t.Errorf("file-seeded callees produced no refs")
	}
}

// TestPipeTerminalSignatures: an explicit terminal renders the final set as
// signatures into the read lane.
func TestPipeTerminalSignatures(t *testing.T) {
	_, _, _, call := pipeFixture(t)
	out := call(QueryInput{Input: "pipeLeaf | callers | signatures"})
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok", out.Status)
	}
	if out.Route.Lane != "read" || out.Result.Read == nil {
		t.Fatalf("signatures terminal should populate read lane, got lane=%q result=%+v", out.Route.Lane, out.Result)
	}
	if out.Result.Read.Content == "" {
		t.Errorf("signatures terminal returned empty content")
	}
	last := out.Route.Stages[len(out.Route.Stages)-1]
	if last != "signatures" {
		t.Errorf("last stage = %q, want signatures", last)
	}
}

// TestClampToTokens locks the #218 budget guarantee: the returned prefix never
// exceeds the budget, and an over-budget input is flagged truncated.
func TestClampToTokens(t *testing.T) {
	// Within budget → unchanged, not truncated.
	if got, cut := clampToTokens("a\nb\n", 1000); got != "a\nb\n" || cut {
		t.Errorf("within budget: got %q cut=%v, want unchanged not-cut", got, cut)
	}
	// Over budget → bounded and flagged.
	big := strings.Repeat("xx\n", 100)
	got, cut := clampToTokens(big, 6)
	if !cut {
		t.Errorf("over budget should report truncated")
	}
	if n := tokens.Count(got); n > 6 {
		t.Errorf("clamped output = %d tokens, want <= 6", n)
	}
	if got == "" {
		t.Errorf("clamp should make progress, got empty")
	}
	// Zero budget → empty, truncated.
	if got, cut := clampToTokens("anything", 0); got != "" || !cut {
		t.Errorf("zero budget: got %q cut=%v, want empty+cut", got, cut)
	}
}

// TestPipeTerminalAssembleBudget locks the #218 fix end-to-end: assemble:N caps
// the terminal output at N tokens (the first file is clamped too) and flags it.
func TestPipeTerminalAssembleBudget(t *testing.T) {
	_, _, _, call := pipeFixture(t)
	full := call(QueryInput{Input: "pipeLeaf | callers | signatures"})
	if full.Result.Read == nil || full.Result.Read.Content == "" {
		t.Fatalf("signatures terminal returned no content to bound")
	}
	budget := 6
	out := call(QueryInput{Input: "pipeLeaf | callers | assemble:6"})
	if out.Status != "ok" || out.Result.Read == nil {
		t.Fatalf("assemble terminal: status=%q result=%+v", out.Status, out.Result)
	}
	if n := tokens.Count(out.Result.Read.Content); n > budget {
		t.Errorf("assemble:6 returned %d tokens, want <= %d", n, budget)
	}
	if !out.Result.Read.Truncated || out.Trust.Caveat == "" {
		t.Errorf("a budget-clamped assemble should flag Truncated + carry a caveat")
	}
}

// TestPipeUnknownOp: an unrecognized op is an honest error naming the grammar.
func TestPipeUnknownOp(t *testing.T) {
	_, _, _, call := pipeFixture(t)
	out := call(QueryInput{Input: "pipeLeaf | frobnicate"})
	if out.Status != "error" {
		t.Fatalf("status = %q, want error", out.Status)
	}
	if out.Trust.Caveat == "" {
		t.Errorf("error should carry a caveat explaining the grammar")
	}
}

func hasID(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func containsSuffix(xs []string, suffix string) bool {
	for _, x := range xs {
		if len(x) >= len(suffix) && x[len(x)-len(suffix):] == suffix {
			return true
		}
	}
	return false
}
