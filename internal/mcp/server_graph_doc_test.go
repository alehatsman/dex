package mcp

import (
	"testing"

	"github.com/alehatsman/dex/internal/graph"
)

// docVaultView hand-builds a graphView for a small doc vault:
//
//	index.md  --links-->     specs/auth.md   (two sites: lines 3 and 4)
//	index.md  --wikilinks--> specs/auth.md   (line 8)
//	index.md  --wikilinks--> specs/glossary.md
//	notes/ideas.md --wikilinks--> specs/auth.md
//	index.md  --calls-->     specs/auth.md   (a bogus code edge that must
//	                                           never surface via doc tools)
func docVaultView() *graphView {
	doc := func(rel string) graphNode {
		return graphNode{
			ID:   graph.NodeID("", "_markdown", graph.NodeDocument, rel),
			Kind: graph.NodeDocument, Name: baseName(rel), QualifiedName: rel,
			PackagePath: "_markdown", FilePath: rel,
		}
	}
	index := doc("index.md")
	auth := doc("specs/auth.md")
	gloss := doc("specs/glossary.md")
	ideas := doc("notes/ideas.md")

	edge := func(kind graph.EdgeKind, src, dst graphNode, line int) graphEdge {
		return graphEdge{Kind: kind, SrcID: src.ID, DstID: dst.ID, FilePath: src.FilePath, StartLine: line}
	}
	edges := []graphEdge{
		edge(graph.EdgeLinks, index, auth, 3),
		edge(graph.EdgeLinks, index, auth, 4),
		edge(graph.EdgeWikilinks, index, auth, 8),
		edge(graph.EdgeWikilinks, index, gloss, 7),
		edge(graph.EdgeWikilinks, ideas, auth, 3),
		edge(graph.EdgeCalls, index, auth, 99), // must be ignored by doc tools
	}

	v := &graphView{
		nodesByID:        map[string]graphNode{},
		nodesByQualified: map[string][]graphNode{},
		edgesBySrc:       map[string][]graphEdge{},
		edgesByDst:       map[string][]graphEdge{},
	}
	for _, n := range []graphNode{index, auth, gloss, ideas} {
		v.nodesByID[n.ID] = n
		v.nodesByQualified[n.QualifiedName] = append(v.nodesByQualified[n.QualifiedName], n)
	}
	for _, e := range edges {
		v.edgesBySrc[e.SrcID] = append(v.edgesBySrc[e.SrcID], e)
		v.edgesByDst[e.DstID] = append(v.edgesByDst[e.DstID], e)
	}
	return v
}

func baseName(rel string) string {
	for i := len(rel) - 1; i >= 0; i-- {
		if rel[i] == '/' {
			return rel[i+1:]
		}
	}
	return rel
}

func TestCollectDocEdgesOutgoing(t *testing.T) {
	v := docVaultView()
	targets := resolveDocTargets(v, "index.md")
	if len(targets) != 1 {
		t.Fatalf("resolveDocTargets(index.md) = %d targets, want 1", len(targets))
	}
	hits := collectDocEdges(v, targets, false, 50)

	// 4 outgoing doc edges (two auth links on distinct lines + one auth
	// wikilink + one glossary wikilink). The `calls` edge is excluded.
	if len(hits) != 4 {
		t.Fatalf("outgoing hits = %d, want 4: %+v", len(hits), hits)
	}
	for _, h := range hits {
		if h.Kind != "links" && h.Kind != "wikilinks" {
			t.Errorf("doc tool surfaced a non-doc edge kind %q", h.Kind)
		}
	}
	// Sorted by doc, then kind, then line: glossary(wiki) last; auth links
	// (lines 3,4) before auth wikilink (line 8).
	want := []DocLink{
		{Doc: "specs/auth.md", Kind: "links", LinkSiteLine: 3},
		{Doc: "specs/auth.md", Kind: "links", LinkSiteLine: 4},
		{Doc: "specs/auth.md", Kind: "wikilinks", LinkSiteLine: 8},
		{Doc: "specs/glossary.md", Kind: "wikilinks", LinkSiteLine: 7},
	}
	for i, w := range want {
		if hits[i].Doc != w.Doc || hits[i].Kind != w.Kind || hits[i].LinkSiteLine != w.LinkSiteLine {
			t.Errorf("hit[%d] = {%s %s :%d}, want {%s %s :%d}",
				i, hits[i].Doc, hits[i].Kind, hits[i].LinkSiteLine, w.Doc, w.Kind, w.LinkSiteLine)
		}
	}
}

func TestCollectDocEdgesBacklinks(t *testing.T) {
	v := docVaultView()
	targets := resolveDocTargets(v, "specs/auth.md")
	hits := collectDocEdges(v, targets, true, 50)

	// Incoming: index.md links (3,4) + index.md wikilink (8) + ideas.md
	// wikilink (3) = 4. The calls edge is excluded.
	if len(hits) != 4 {
		t.Fatalf("backlinks = %d, want 4: %+v", len(hits), hits)
	}
	var fromIdeas bool
	for _, h := range hits {
		if h.Kind == "calls" {
			t.Errorf("backlinks surfaced a calls edge")
		}
		if h.Doc == "notes/ideas.md" {
			fromIdeas = true
		}
	}
	if !fromIdeas {
		t.Errorf("expected a backlink from notes/ideas.md; got %+v", hits)
	}
}

func TestResolveDocTargets(t *testing.T) {
	v := docVaultView()

	// Exact relpath.
	if got := resolveDocTargets(v, "specs/auth.md"); len(got) != 1 || got[0].QualifiedName != "specs/auth.md" {
		t.Errorf("exact path: got %+v", got)
	}
	// Leading ./ and extension completion.
	if got := resolveDocTargets(v, "./specs/auth"); len(got) != 1 || got[0].QualifiedName != "specs/auth.md" {
		t.Errorf("./path + ext completion: got %+v", got)
	}
	// Unique bare basename.
	if got := resolveDocTargets(v, "glossary"); len(got) != 1 || got[0].QualifiedName != "specs/glossary.md" {
		t.Errorf("bare basename: got %+v", got)
	}
	// Unknown path → no targets.
	if got := resolveDocTargets(v, "does/not/exist.md"); len(got) != 0 {
		t.Errorf("unknown path: got %+v, want none", got)
	}
}
