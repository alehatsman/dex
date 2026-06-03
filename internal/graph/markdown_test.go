package graph

import (
	"context"
	"strings"
	"testing"
)

func TestExtractMarkdownVault(t *testing.T) {
	root := copyFixture(t, "md_vault")
	res, err := ExtractMarkdown(context.Background(), root)
	if err != nil {
		t.Fatalf("ExtractMarkdown: %v", err)
	}

	// One document node per markdown file.
	wantDocs := []string{
		"index.md",
		"specs/auth.md",
		"specs/glossary.md",
		"specs/dup.md",
		"notes/ideas.md",
		"notes/dup.md",
	}
	for _, qn := range wantDocs {
		if findNode(res.Nodes, NodeDocument, qn) == nil {
			t.Errorf("missing document node %s; docs=%v", qn, nodesOfKind(res.Nodes, NodeDocument))
		}
	}

	doc := func(qn string) string { return NodeID("", mdPkg, NodeDocument, qn) }

	// Inline links, including the anchored variant (#flow) and the
	// relative-up resolution (specs/auth.md → ../index.md).
	wantLinks := [][2]string{
		{"index.md", "specs/auth.md"},
		{"index.md", "notes/ideas.md"},
		{"specs/auth.md", "index.md"},
	}
	for _, e := range wantLinks {
		if findEdge(res.Edges, EdgeLinks, doc(e[0]), doc(e[1])) == nil {
			t.Errorf("missing links edge %s → %s", e[0], e[1])
		}
	}

	// Wikilinks: bare, aliased, and the reverse pair (backlink-by-direction).
	wantWiki := [][2]string{
		{"index.md", "specs/glossary.md"},   // [[glossary]]
		{"index.md", "specs/auth.md"},       // [[auth|Authentication]]
		{"specs/auth.md", "notes/ideas.md"}, // [[ideas]]
		{"notes/ideas.md", "specs/auth.md"}, // [[auth]] — reverse direction = backlink
	}
	for _, e := range wantWiki {
		if findEdge(res.Edges, EdgeWikilinks, doc(e[0]), doc(e[1])) == nil {
			t.Errorf("missing wikilinks edge %s → %s", e[0], e[1])
		}
	}

	// Fenced code block link [fake](specs/glossary.md) must NOT emit a
	// links edge (only the [[glossary]] wikilink reaches glossary).
	if findEdge(res.Edges, EdgeLinks, doc("index.md"), doc("specs/glossary.md")) != nil {
		t.Errorf("fenced-code link to specs/glossary.md leaked a links edge")
	}

	// Images, external URLs, and embeds (![[diagram]]) emit nothing.
	if findNode(res.Nodes, NodeDocument, "assets/logo.png") != nil {
		t.Errorf("image target leaked a document node")
	}
	for _, e := range res.Edges {
		if strings.Contains(e.DstID, "logo.png") || strings.Contains(e.DstID, "diagram") {
			t.Errorf("unexpected edge to image/embed: %+v", e)
		}
	}

	// Broken link and ambiguous wikilink are surfaced as warnings, not edges.
	if findEdge(res.Edges, EdgeLinks, doc("index.md"), doc("specs/missing.md")) != nil {
		t.Errorf("broken link specs/missing.md emitted an edge; should warn + skip")
	}
	var sawBroken, sawAmbiguous bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "broken link to specs/missing.md") {
			sawBroken = true
		}
		if strings.Contains(w, "ambiguous wikilink [[dup]]") {
			sawAmbiguous = true
		}
	}
	if !sawBroken {
		t.Errorf("expected a broken-link warning; warnings=%v", res.Warnings)
	}
	if !sawAmbiguous {
		t.Errorf("expected an ambiguous-wikilink warning; warnings=%v", res.Warnings)
	}
}

func TestExtractMarkdownNoMarkdown(t *testing.T) {
	root := copyFixture(t, "simple") // Go fixture, no .md files
	res, err := ExtractMarkdown(context.Background(), root)
	if err != nil {
		t.Fatalf("ExtractMarkdown: %v", err)
	}
	if len(res.Nodes) != 0 || len(res.Edges) != 0 {
		t.Errorf("expected empty result on a markdown-free tree; got nodes=%d edges=%d",
			len(res.Nodes), len(res.Edges))
	}
}
