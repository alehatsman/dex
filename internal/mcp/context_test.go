package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/codemap"
	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/store"
)

func TestIsBuildOrConfigPath(t *testing.T) {
	tests := map[string]bool{
		"Taskfile.yml":   true,
		"Taskfile.yaml":  true,
		"Dockerfile":     true,
		"Makefile":       true,
		".github/ci.yml": true,
		"config.toml":    true,
		"internal/x.go":  false,
		"README.md":      false,
		"go.mod":         false, // intentionally not demoted
		"package.json":   false,
	}
	for p, want := range tests {
		if got := pathTags(p).has(tagBuild); got != want {
			t.Errorf("pathTags(%q).has(tagBuild) = %v, want %v", p, got, want)
		}
	}
}

func TestIsDocPath(t *testing.T) {
	tests := map[string]bool{
		"README.md":               true,
		"docs/spec.rst":           true,
		"NOTES.txt":               true,
		"docs/page.adoc":          true,
		"site/post.mdx":           true,
		"internal/store/store.go": false,
		"cmd/main.py":             false,
	}
	for p, want := range tests {
		if got := pathTags(p).has(tagDoc); got != want {
			t.Errorf("pathTags(%q).has(tagDoc) = %v, want %v", p, got, want)
		}
	}
}

func TestIsTestPath(t *testing.T) {
	tests := map[string]bool{
		"internal/mcp/context_test.go": true,
		"pkg/foo/bar_test.go":          true,
		"src/Foo.test.ts":              true,
		"src/Foo.test.tsx":             true,
		"src/foo.spec.js":              true,
		"src/foo.spec.jsx":             true,
		"tests/test_foo.py":            true,
		"tests/foo_test.py":            true,
		"spec/foo_spec.rb":             true,
		"src/foo_test.rs":              true,
		"internal/store/store.go":      false,
		"README.md":                    false,
		"cmd/main.py":                  false,
		"src/foo.ts":                   false,
	}
	for p, want := range tests {
		if got := isTestPath(p); got != want {
			t.Errorf("isTestPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestContextRouterInlinesByDefault(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\nfunc Bye() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "where do we greet users",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.SuggestedReads) == 0 {
		t.Fatal("want suggested_reads")
	}
	if out.SuggestedReads[0].Content == "" {
		t.Errorf("suggested_reads[0].Content should be inlined by default; got empty")
	}
}

func TestContextRouterNoInline(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\nfunc Bye() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "where do we greet users",
		ProjectRoot: root,
		NoInline:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.SuggestedReads) == 0 {
		t.Fatal("want suggested_reads")
	}
	for i, r := range out.SuggestedReads {
		if r.Content != "" {
			t.Errorf("suggested_reads[%d].Content should be empty with NoInline=true; got %d bytes", i, len(r.Content))
		}
	}
}

// TestInlineContentSkipsTestSourceForNonEditing verifies that raw test
// source in semantic_hits arrives with Path/Range only (no Content) for
// non-editing intents, while the matching implementation chunk is
// inlined. Test bodies displaced implementation from the shared inline
// budget before this filter — surfaced when "architecture" / behavior
// queries pulled foo_test.go above foo.go and burned ~4 KB on fixture
// boilerplate. editing_context is the exception: sibling tests are
// real context when modifying a file.
//
// Unit-tests inlineContent directly with pre-populated hits so the
// suppressLowScore branch (fake-embed cosines are too low to clear the
// confidence threshold) doesn't mask the filter's behavior.
func TestIsFixturePath(t *testing.T) {
	cases := map[string]bool{
		"internal/graph/testdata/simple/store/store.go": true,
		"pkg/testdata/foo.go":                           true,
		"web/__fixtures__/users.json":                   true,
		"internal/store/store.go":                       false,
		"internal/test_helpers.go":                      false, // not in a testdata dir
		"docs/README.md":                                false,
	}
	for path, want := range cases {
		if got := pathTags(path).has(tagFixture); got != want {
			t.Errorf("pathTags(%q).has(tagFixture) = %v, want %v", path, got, want)
		}
	}
}

func TestRunSymbolLaneDemotesFixtures(t *testing.T) {
	// FindSymbol orders by (path, start_line), so `internal/graph/
	// testdata/simple/store/store.go` lands BEFORE `internal/store/
	// store.go` alphabetically. The symbol lane must demote testdata
	// paths so the prose directive points at real code.
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "real.go"),
		"package main\n\ntype Store struct{}\n")
	writeFile(t, filepath.Join(projDir, "testdata", "fixture.go"),
		"package fixture\n\ntype Store struct{}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "Store",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Symbols) < 2 {
		t.Fatalf("expected 2 symbols (real + testdata); got %d", len(out.Symbols))
	}
	if out.Symbols[0].Path != "real.go" {
		t.Errorf("first symbol should be real.go (not testdata); got %q", out.Symbols[0].Path)
	}
	if !strings.Contains(out.Symbols[1].Path, "testdata") {
		t.Errorf("second symbol should be the testdata fixture; got %q", out.Symbols[1].Path)
	}
}

// TestInlineCapsFor moved to internal/retrieve (inline_test.go) with the
// caps policy itself — the budget is transport-free.

// ─── integration: contextRouter end-to-end ────────────────────────────────

func TestContextRouterBehaviorSearch(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\nfunc Bye() {}\n")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "where do we greet users",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%s hint=%s", out.Status, out.Hint)
	}
	if out.Intent != retrieve.IntentBehaviorSearch {
		t.Errorf("intent=%s, want behavior_search", out.Intent)
	}
	if len(out.SemanticHits) == 0 {
		t.Fatal("want semantic_hits, got 0")
	}
	if len(out.SuggestedReads) == 0 {
		t.Error("want suggested_reads")
	}
	if out.NextAction == "" {
		t.Error("want non-empty next_action prose")
	}
	// out.Graph is omitted when enrichGraph produced nothing — the
	// JSON tag is `omitempty`. We don't assert anything about it here;
	// the dedicated TestContextRouter*GraphPopulated tests cover the
	// populated path.
}

func TestContextRouterSymbolLookup(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\nfunc Bye() {}\n")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "Greet",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%s hint=%s", out.Status, out.Hint)
	}
	if out.Intent != retrieve.IntentSymbolLookup {
		t.Errorf("intent=%s, want symbol_lookup", out.Intent)
	}
	if len(out.Symbols) == 0 {
		t.Fatal("want symbols")
	}
	if out.Symbols[0].QualifiedName != "Greet" {
		t.Errorf("symbol[0]=%s, want Greet", out.Symbols[0].QualifiedName)
	}
	if !strings.Contains(out.NextAction, "Read") {
		t.Errorf("next_action should be a Read directive: %q", out.NextAction)
	}
	// Without a graph indexed, avoid nudges toward `dex index`.
	// With graph indexed it would say "Do not grep". Either is acceptable
	// here; the symbol_lookup path is exercised either way.
	if !strings.Contains(out.Avoid, "Do not grep") && !strings.Contains(out.Avoid, "dex index") {
		t.Errorf("avoid should mention either don't-grep or index nudge: %q", out.Avoid)
	}
}

func TestContextRouterCallersAvoid(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Search() {}\nfunc UsesSearch() { Search() }\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "callers of Search",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Intent != retrieve.IntentCallers {
		t.Errorf("intent=%s, want callers", out.Intent)
	}
	if out.Avoid == "" {
		t.Errorf("expected non-empty avoid for callers intent")
	}
	if !strings.Contains(out.Avoid, "calls") && !strings.Contains(out.Avoid, "references") {
		t.Errorf("avoid should mention `calls` or `references`: %q", out.Avoid)
	}
	// The fixture indexes a Go file so the call graph is live; references must
	// be populated (Go call graph, not rg or BM25).
	if len(out.References) == 0 {
		t.Errorf("expected references for `Search` usages (Go call graph); got 0")
	}
}

func TestContextRouterTruncatedReadFlagsNextAction(t *testing.T) {
	// Inline content has per-read caps (60 lines / 4 KB for targeted
	// intents). When a chunk exceeds those caps, Truncated=true is
	// set on the read — next_action must surface that so the agent
	// knows the inlined Content isn't the full chunk.
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	// Write a Go file with a function long enough to exceed the
	// 60-line targeted cap. Use 100 distinct printable lines so the
	// chunker treats it as one chunk.
	var body strings.Builder
	body.WriteString("package main\n\n// Long is intentionally long to exceed the inline cap.\nfunc Long() {\n")
	for i := 0; i < 100; i++ {
		body.WriteString(fmt.Sprintf("\t_ = %d\n", i))
	}
	body.WriteString("}\n")
	writeFile(t, filepath.Join(projDir, "long.go"), body.String())
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "Long", // exact symbol match → symbol_lookup
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.SuggestedReads) == 0 {
		t.Fatal("expected suggested_reads")
	}
	if !out.SuggestedReads[0].Truncated {
		t.Fatalf("expected reads[0].Truncated; chunk was %d lines", out.SuggestedReads[0].EndLine-out.SuggestedReads[0].StartLine+1)
	}
	if !strings.Contains(out.NextAction, "truncated") {
		t.Errorf("next_action should mention truncation when reads[0].Truncated=true; got %q", out.NextAction)
	}
}

func TestContextRouterSymbolLookupMissNearMissCandidates(t *testing.T) {
	// When the user asks for a specific identifier via symbol_lookup
	// and we find nothing exact, the router should surface substring
	// matches in `hint` — mirroring what search_symbol does — so the
	// agent has real names to retry with instead of guessing.
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Indexer() {}\nfunc IndexableExt() {}\nfunc cmdIndex() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "Index", // bare identifier; no exact match
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Intent != retrieve.IntentSymbolLookup {
		t.Errorf("intent=%s, want symbol_lookup", out.Intent)
	}
	if len(out.Symbols) != 0 {
		t.Errorf("expected 0 symbol matches; got %d", len(out.Symbols))
	}
	if !strings.Contains(out.Hint, "did you mean") {
		t.Errorf("hint should surface candidates; got %q", out.Hint)
	}
	if !strings.Contains(out.Hint, "Indexer") && !strings.Contains(out.Hint, "IndexableExt") && !strings.Contains(out.Hint, "cmdIndex") {
		t.Errorf("hint should name at least one real candidate; got %q", out.Hint)
	}
}

func TestContextRouterNoIndex(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "x.go"), "package x\n")

	s := newServer(srv.URL, cacheDir)
	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "anything",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no-index" {
		t.Errorf("status=%s, want no-index", out.Status)
	}
}

// An empty question is no longer an error — it routes to the session-start
// orientation path (#348 / #316 story 6). On an unindexed project it must
// degrade gracefully: intent "orient" with a hint that points at indexing,
// never the old "question is empty" error.
func TestContextRouterEmptyQuestionOrients(t *testing.T) {
	s := newServer("http://127.0.0.1:0", t.TempDir())
	_, out, err := s.ContextRouter(context.Background(), ContextInput{ProjectRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Intent != "orient" {
		t.Errorf("intent=%q, want orient", out.Intent)
	}
	if out.Status == "ok" {
		t.Errorf("status=ok on an unindexed project, want a graceful degrade status; out=%+v", out)
	}
	if !strings.Contains(out.Hint, "index") {
		t.Errorf("hint should guide toward indexing, got %q", out.Hint)
	}
	if strings.Contains(out.Hint, "question is empty") {
		t.Errorf("empty question must orient, not error: %q", out.Hint)
	}
}

// On an indexed project with a community graph, an empty question returns the
// deterministic L0+L1 orientation bundle in out.Map (#348). Proves the router
// reaches codemap.RenderOrient end-to-end and names the indexed package.
func TestContextRouterEmptyQuestionRendersMap(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\ntype Store struct{}\nfunc (s *Store) Search() {}\nfunc (s *Store) Open() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	ctx := context.Background()
	seedGraph(t, ctx, root, cacheDir)

	// Assign all three nodes to one community with PageRank so GraphCommunities
	// (min-members 3) returns a cluster the orient bundle can zoom into.
	p, err := proj.Resolve(root, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GraphSetCentrality(ctx, []store.GraphCentralityRow{
		{ID: "m::p::type::Store", PageRank: 0.5, InDegree: 2, CommunityID: 1},
		{ID: "m::p::method::(*Store).Search", PageRank: 0.3, InDegree: 1, CommunityID: 1},
		{ID: "m::p::method::(*Store).Open", PageRank: 0.2, InDegree: 1, CommunityID: 1},
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	s := newServer(srv.URL, cacheDir)
	_, out, err := s.ContextRouter(ctx, ContextInput{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if out.Intent != "orient" {
		t.Fatalf("intent=%q, want orient", out.Intent)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%s hint=%s", out.Status, out.Hint)
	}
	if out.Map == "" {
		t.Fatal("out.Map should hold the L0+L1 orientation bundle, got empty")
	}
	if !strings.Contains(out.Map, "p") {
		t.Errorf("orient bundle should name the indexed package; got:\n%s", out.Map)
	}
	// The map must equal a direct render of the same clusters — single-sourced
	// through codemap.RenderOrient, never a divergent reimplementation.
	comm, err := s.GraphCommunities(ctx, CommunitiesInput{MinMembers: 3, K: 50, TopK: 25, ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if want := codemap.RenderOrient(AdaptCommunities(comm.Communities), codemap.OrientExtras{Entrypoints: comm.Entrypoints, Commands: ExtractProjectCommands(comm.Project), ImportEdges: CodemapImportEdges(comm.ImportEdges), Externals: comm.Externals, Scale: CodemapScale(comm.Scale)}, codemap.DefaultL0Budget, codemap.DefaultL1Budget); out.Map != want {
		t.Errorf("out.Map diverges from codemap.RenderOrient:\ngot:\n%s\nwant:\n%s", out.Map, want)
	}
}

func TestCompactID(t *testing.T) {
	cases := []struct {
		name string
		n    graphquery.Node
		want string
	}{
		{"method", graphquery.Node{Kind: graph.NodeMethod, Name: "ContextRouter", QualifiedName: "(*Server).ContextRouter", PackagePath: "github.com/foo/bar/internal/mcp"}, "mcp.(*Server).ContextRouter"},
		{"type", graphquery.Node{Kind: graph.NodeStruct, Name: "Server", QualifiedName: "Server", PackagePath: "github.com/foo/bar/internal/mcp"}, "mcp.Server"},
		{"package", graphquery.Node{Kind: graph.NodePackage, QualifiedName: "github.com/foo/bar/internal/mcp", PackagePath: "github.com/foo/bar/internal/mcp"}, "mcp"},
		{"import", graphquery.Node{Kind: graph.NodeImport, Name: "sync", QualifiedName: "sync", PackagePath: "github.com/foo/bar/internal/mcp"}, "sync"},
		{"field", graphquery.Node{Kind: graph.NodeField, Name: "ChatClient", QualifiedName: "Server.ChatClient", PackagePath: "github.com/foo/bar/internal/mcp"}, "mcp.Server.ChatClient"},
		{"stdlib pkg path", graphquery.Node{Kind: graph.NodeFunction, Name: "Println", QualifiedName: "Println", PackagePath: "fmt"}, "fmt.Println"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retrieve.CompactID(tc.n); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// seedGraph writes a synthetic graph for `root` directly via the
// store's upsert methods. Avoids invoking ExtractGo (which needs a
// real go.mod + GOPATH-resolvable imports) so we can test the router's
// graph integration on a one-file fixture.
func seedGraph(t *testing.T, ctx context.Context, root, cacheDir string) {
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
	typeID := "m::p::type::Store"
	methodID := "m::p::method::(*Store).Search"
	siblingID := "m::p::method::(*Store).Open"
	nodes := []store.GraphNodeRow{
		{ID: typeID, Kind: string(graph.NodeType), Name: "Store", QualifiedName: "Store",
			PackagePath: "p", FilePath: "main.go", StartLine: 1, EndLine: 5,
			MetadataJSON: []byte("{}"), ContentHash: "n1"},
		{ID: methodID, Kind: string(graph.NodeMethod), Name: "Search", QualifiedName: "(*Store).Search",
			PackagePath: "p", FilePath: "main.go", StartLine: 10, EndLine: 20,
			MetadataJSON: []byte("{}"), ContentHash: "n2"},
		{ID: siblingID, Kind: string(graph.NodeMethod), Name: "Open", QualifiedName: "(*Store).Open",
			PackagePath: "p", FilePath: "main.go", StartLine: 30, EndLine: 40,
			MetadataJSON: []byte("{}"), ContentHash: "n3"},
	}
	if err := st.GraphUpsertNodes(ctx, nodes, now); err != nil {
		t.Fatal(err)
	}
	edges := []store.GraphEdgeRow{
		{ID: "e1", Kind: string(graph.EdgeHasMethod), SrcID: typeID, DstID: methodID,
			FilePath: "main.go", StartLine: 10, EndLine: 20,
			MetadataJSON: []byte("{}"), ContentHash: "h1"},
		{ID: "e2", Kind: string(graph.EdgeHasMethod), SrcID: typeID, DstID: siblingID,
			FilePath: "main.go", StartLine: 30, EndLine: 40,
			MetadataJSON: []byte("{}"), ContentHash: "h2"},
	}
	if err := st.GraphUpsertEdges(ctx, edges, now); err != nil {
		t.Fatal(err)
	}
}

func TestContextRouterGraphSymbolLookup(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\ntype Store struct{}\nfunc (s *Store) Search() {}\nfunc (s *Store) Open() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	ctx := context.Background()
	seedGraph(t, ctx, root, cacheDir)

	s := newServer(srv.URL, cacheDir)
	_, out, err := s.ContextRouter(ctx, ContextInput{
		Question:    "Search",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%s hint=%s", out.Status, out.Hint)
	}
	if out.Graph == nil || len(out.Graph.Nodes) == 0 {
		t.Fatalf("graph.nodes should be populated; got %+v", out.Graph)
	}

	var names, ids []string
	for _, n := range out.Graph.Nodes {
		names = append(names, n.QualifiedName)
		ids = append(ids, n.ID)
	}
	joinedNames := strings.Join(names, ",")
	for _, want := range []string{"Store", "(*Store).Search", "(*Store).Open"} {
		if !strings.Contains(joinedNames, want) {
			t.Errorf("graph.nodes should include %q; got %s", want, joinedNames)
		}
	}
	// IDs must be the compact form (`<pkg-tail>.<qualified-name>`),
	// never the legacy `<module>::<pkg>::<kind>::<qname>`.
	joinedIDs := strings.Join(ids, ",")
	if strings.Contains(joinedIDs, "::") {
		t.Errorf("graph.nodes[].id should be compact, not module-qualified; got %s", joinedIDs)
	}
	for _, want := range []string{"p.(*Store).Search", "p.(*Store).Open", "p.Store"} {
		if !strings.Contains(joinedIDs, want) {
			t.Errorf("graph.nodes[].id should include %q; got %s", want, joinedIDs)
		}
	}
	// Edges must reference the same compact IDs.
	for _, e := range out.Graph.Edges {
		if strings.Contains(e.From, "::") || strings.Contains(e.To, "::") {
			t.Errorf("edge id should be compact; got from=%q to=%q", e.From, e.To)
		}
	}
	if !strings.Contains(out.Avoid, "Do not grep") {
		t.Errorf("avoid should say don't grep when graph is indexed: %q", out.Avoid)
	}
}

func TestContextRouterKBudget(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "a.go"), "package x\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(projDir, "b.go"), "package x\n\nfunc B() {}\n")
	writeFile(t, filepath.Join(projDir, "c.go"), "package x\n\nfunc C() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "function",
		ProjectRoot: root,
		K:           2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%s hint=%s", out.Status, out.Hint)
	}
	if len(out.SemanticHits) > 2 {
		t.Errorf("k=2 should cap semantic_hits; got %d", len(out.SemanticHits))
	}
}
