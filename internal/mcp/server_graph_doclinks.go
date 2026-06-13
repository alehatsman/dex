package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/graphquery"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tools: graph_links / graph_backlinks ─────────────────────────────────

type DocLinkInput struct {
	Doc         string `json:"doc" jsonschema:"path to a markdown document relative to the project root (e.g. 'docs/spec.md'); a bare basename like 'spec' is also accepted when unambiguous"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int    `json:"k,omitempty" jsonschema:"max hits to return (default 50, max 200)"`
}

// DocLink is one endpoint of a doc-graph edge: the document on the other
// end, plus the file:line of the link expression that produced the edge.
type DocLink struct {
	Doc          string `json:"doc"`  // relpath of the peer document (its parent doc when the peer is a heading)
	Name         string `json:"name"` // basename of the peer document, or the heading text
	Kind         string `json:"kind"` // "links" | "wikilinks" | "transcludes"
	LinkSitePath string `json:"link_site_path,omitempty"`
	LinkSiteLine int    `json:"link_site_line,omitempty"`
	// TargetAnchor is the heading slug the reference points at, when the
	// edge targets a section rather than a whole document. For backlinks
	// it names the section of the queried doc that was linked.
	TargetAnchor string `json:"target_anchor,omitempty"`
}

// DocTarget is a resolved interpretation of the input `doc`. Returned
// even with no links so the caller can confirm the resolution.
type DocTarget struct {
	Doc  string `json:"doc"`
	Name string `json:"name"`
}

type DocLinkOutput struct {
	Status  string      `json:"status"` // "ok" | "no-index" | "no-graph" | "not-found" | "error"
	Hint    string      `json:"hint,omitempty"`
	Project string      `json:"project,omitempty"`
	Targets []DocTarget `json:"targets,omitempty"`
	Hits    []DocLink   `json:"hits"`
}

// GraphLinks, GraphBacklinks are exported wrappers so the CLI can reuse
// the handlers, mirroring GraphCallers/GraphCallees.
func (s *Server) GraphLinks(ctx context.Context, in DocLinkInput) (DocLinkOutput, error) {
	_, out, err := s.graphLinks(ctx, nil, in)
	return out, err
}

func (s *Server) GraphBacklinks(ctx context.Context, in DocLinkInput) (DocLinkOutput, error) {
	_, out, err := s.graphBacklinks(ctx, nil, in)
	return out, err
}

func (s *Server) graphLinks(ctx context.Context, _ *sdk.CallToolRequest, in DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error) {
	return s.docEdges(ctx, in, false)
}

func (s *Server) graphBacklinks(ctx context.Context, _ *sdk.CallToolRequest, in DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error) {
	return s.docEdges(ctx, in, true)
}

// docEdges is the shared body for the doc-graph verbs. backlinks=false
// (graph_links) walks edgesBySrc — documents this doc points to;
// backlinks=true (graph_backlinks) walks edgesByDst — documents that
// point here. Both keep only `links`/`wikilinks` edges, so it never
// surfaces code (`calls`/`imports`) edges even on a mixed node id.
func (s *Server) docEdges(ctx context.Context, in DocLinkInput, backlinks bool) (*sdk.CallToolResult, DocLinkOutput, error) {
	if strings.TrimSpace(in.Doc) == "" {
		return nil, DocLinkOutput{Status: "error", Hint: "doc is empty"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, DocLinkOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, DocLinkOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, DocLinkOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	view, err := s.cachedLoadGraphView(ctx, st, p.DBPath)
	if err != nil {
		return nil, DocLinkOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, DocLinkOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}

	targets := graphquery.ResolveDocTargets(view, in.Doc)
	if len(targets) == 0 {
		// Distinguish "no docs at all" (needs reindex with this release)
		// from "that path isn't a known doc" (likely a typo).
		hint := fmt.Sprintf("no document node matches %q — pass a path relative to the project root, e.g. 'docs/spec.md'.", in.Doc)
		if len(view.DocNodes()) == 0 {
			hint = "graph has no document nodes — reindex with this release (`dex index . --graph=only`) to extract the markdown doc graph."
		}
		return nil, DocLinkOutput{Status: "not-found", Project: p.Root, Hint: hint}, nil
	}

	k := in.K
	if k <= 0 {
		k = 50
	}
	if k > 200 {
		k = 200
	}

	out := DocLinkOutput{Status: "ok", Project: p.Root}
	for _, t := range targets {
		out.Targets = append(out.Targets, DocTarget{Doc: t.QualifiedName, Name: t.Name})
	}
	out.Hits = docLinksFrom(graphquery.CollectDocEdges(view, targets, backlinks, k))
	return nil, out, nil
}
