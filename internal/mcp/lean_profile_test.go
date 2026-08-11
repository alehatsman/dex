package mcp

import (
	"context"
	"testing"
)

// Note: since #142 no embedder-backed tool lives on the everyday surface —
// `search` moved to the DEX_EXPERT power lane (everyday concept-search is covered
// by ask(behavior_search), which degrades to BM25). The embedder-derived exposure
// contract (#283/#290) is now tested at the expert tier in TestClonesSimilarGating
// (clones/similar/search appear iff an embedder is wired AND DEX_EXPERT is set).

// zeroInferenceTools work with no embedder at all (the ask router degrades to
// BM25+symbol lanes; look routes to grep/read over the pre-computed index; act
// runs shell). They are the default verb surface and must always be advertised,
// lean or not. After the 5c collapse (#145) the raw grep primitive moved to
// expert — its lean-mode coverage is now via look("/regex/"). (repo_map is
// expert-only; exact-symbol lookup has no standalone tool since #685.)
var zeroInferenceTools = []string{
	"ask",
	"look",     // exact-fetch verb (symbol→trace, path:line→locate, path→read, /re/→grep); no embedder needed
	"act",      // run verb (wraps shell); no embedder needed
	"remember", // memory verb (write/recall/supersede), no embedder needed; notes' admin tail is expert (#147)
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

// TestLeanProfileKeepsZeroInferenceTools proves the everyday surface degrades
// gracefully: with no embedder wired, the zero-inference lanes and the (BM25-
// degrading) `ask` router remain advertised. Since #142 no embedder-backed tool
// lives on the everyday surface, so there is nothing to assert *omitted* here —
// the embedder-derived exposure contract now lives in TestClonesSimilarGating.
func TestLeanProfileKeepsZeroInferenceTools(t *testing.T) {
	srv := stubServer(t) // stubServer wires no EmbedClient → lean profile
	if srv.EmbedClient != nil {
		t.Fatal("stubServer unexpectedly has an EmbedClient; test assumes lean (nil)")
	}

	names := listToolNames(t, srv)
	for _, n := range zeroInferenceTools {
		if !names[n] {
			t.Errorf("lean profile omitted zero-inference tool %q; want it advertised", n)
		}
	}
}
