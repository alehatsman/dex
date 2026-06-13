package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: graph_tags ─────────────────────────────────────────────────────

type TagInput struct {
	Tag         string `json:"tag,omitempty" jsonschema:"a markdown #tag (without the leading #) — returns the documents carrying it"`
	Doc         string `json:"doc,omitempty" jsonschema:"a document path — returns the tags that document carries; ignored when tag is set"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int    `json:"k,omitempty" jsonschema:"max items to return (default 100, max 500)"`
}

type TagOutput struct {
	Status  string   `json:"status"`            // "ok" | "no-index" | "no-graph" | "not-found" | "error"
	Hint    string   `json:"hint,omitempty"`    //
	Project string   `json:"project,omitempty"` //
	Query   string   `json:"query,omitempty"`   // the resolved tag or doc
	Result  string   `json:"result,omitempty"`  // "documents" (tag→docs) or "tags" (doc→tags)
	Items   []string `json:"items,omitempty"`   // doc relpaths, or tag names
}

func (s *Server) GraphTags(ctx context.Context, in TagInput) (TagOutput, error) {
	_, out, err := s.graphTags(ctx, nil, in)
	return out, err
}

// graphTags answers the two tag-clustering questions over the doc graph's
// `tagged` edges: `tag` → the documents carrying it; `doc` → the tags it
// carries. Exactly one of tag/doc is used (tag wins if both are set).
func (s *Server) graphTags(ctx context.Context, _ *sdk.CallToolRequest, in TagInput) (*sdk.CallToolResult, TagOutput, error) {
	tag := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(in.Tag), "#"))
	doc := strings.TrimSpace(in.Doc)
	if tag == "" && doc == "" {
		return nil, TagOutput{Status: "error", Hint: "pass `tag` (→ documents) or `doc` (→ tags)"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, TagOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, TagOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, TagOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	view, err := s.cachedLoadGraphView(ctx, st, p.DBPath)
	if err != nil {
		return nil, TagOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, TagOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}

	k := in.K
	if k <= 0 {
		k = 100
	}
	if k > 500 {
		k = 500
	}

	if tag != "" {
		// tag → documents. Find the tag node by name, walk its incoming
		// `tagged` edges, collect the source documents.
		var tagNode *graphquery.Node
		for _, n := range view.NodesByName[tag] {
			if n.Kind == graph.NodeTag {
				nn := n
				tagNode = &nn
				break
			}
		}
		if tagNode == nil {
			return nil, TagOutput{Status: "not-found", Project: p.Root, Query: tag,
				Hint: fmt.Sprintf("no #%s tag in the doc graph.", tag)}, nil
		}
		docs := graphquery.DocsForTag(view, tagNode.ID)
		graphquery.SortByDocCentrality(view, docs)
		if len(docs) > k {
			docs = docs[:k]
		}
		return nil, TagOutput{Status: "ok", Project: p.Root, Query: tag, Result: "documents", Items: docs}, nil
	}

	// doc → tags.
	targets := graphquery.ResolveDocTargets(view, doc)
	if len(targets) == 0 {
		return nil, TagOutput{Status: "not-found", Project: p.Root, Query: doc,
			Hint: fmt.Sprintf("no document node matches %q.", doc)}, nil
	}
	seen := map[string]bool{}
	var tags []string
	for _, t := range targets {
		for _, name := range graphquery.TagsForDoc(view, t.ID) {
			if !seen[name] {
				seen[name] = true
				tags = append(tags, name)
			}
		}
	}
	sort.Strings(tags)
	if len(tags) > k {
		tags = tags[:k]
	}
	return nil, TagOutput{Status: "ok", Project: p.Root, Query: targets[0].QualifiedName, Result: "tags", Items: tags}, nil
}
