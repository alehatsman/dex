package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/embed"
)

// embedBackedTools require a query-time embedder. Under the lean profile
// (DEX_EMBED_ENGINE=none → nil EmbedClient) they must NOT be advertised — the
// capability-derived exposure contract of #283/#290.
var embedBackedTools = []string{
	"search_semantic",
	"search_similar",
	"ctx_overview",
	"search_context",
	"search_workspace",
}

// zeroInferenceTools work with no embedder at all (BM25, exact-symbol, the
// pre-computed graph, and the ask router which degrades to those lanes). They
// must always be advertised, lean or not.
var zeroInferenceTools = []string{
	"search_grep",
	"search_symbol",
	"graph_neighbors",
	"ask",
}

func listToolNames(t *testing.T, srv *Server) map[string]bool {
	t.Helper()
	ctx := context.Background()
	id, _, projects := oneProjectRegistry(t)
	ts := startTestHTTPServer(t, srv, RunHTTPOptions{Projects: projects})
	cs := mcpConnect(t, ctx, ts.URL, "/v1/projects/"+id+"/mcp", ts.Client())
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make(map[string]bool, len(tools.Tools))
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	return names
}

// TestLeanProfileOmitsSemanticTools proves the lean profile: with no embedder
// wired, the embedding-backed tools disappear from the advertised surface while
// the zero-inference lanes (and the degrading `ask` router) remain.
func TestLeanProfileOmitsSemanticTools(t *testing.T) {
	srv := stubServer(t) // stubServer wires no EmbedClient → lean profile
	if srv.EmbedClient != nil {
		t.Fatal("stubServer unexpectedly has an EmbedClient; test assumes lean (nil)")
	}

	names := listToolNames(t, srv)
	for _, n := range embedBackedTools {
		if names[n] {
			t.Errorf("lean profile advertised embedding-backed tool %q; want it omitted", n)
		}
	}
	for _, n := range zeroInferenceTools {
		if !names[n] {
			t.Errorf("lean profile omitted zero-inference tool %q; want it advertised", n)
		}
	}
}

// TestEmbedderAvailableExposesSemanticTools is the positive control: with an
// embedder wired, the embedding-backed tools are advertised.
func TestEmbedderAvailableExposesSemanticTools(t *testing.T) {
	srv := stubServer(t)
	// A non-nil embedder is all the gate checks; the endpoint is never dialed
	// during ListTools, so an unused address is fine.
	srv.EmbedClient = embed.New("http://127.0.0.1:0", "fake", 16, 200*time.Millisecond)

	names := listToolNames(t, srv)
	for _, n := range embedBackedTools {
		if !names[n] {
			t.Errorf("embedder available but embedding-backed tool %q not advertised", n)
		}
	}
}
