package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: graph_communities ──────────────────────────────────────────────

type CommunitiesInput struct {
	MinMembers  int    `json:"min_members,omitempty" jsonschema:"min community size to include (default 3)"`
	K           int    `json:"k,omitempty" jsonschema:"max communities to return (default 20, max 50)"`
	TopK        int    `json:"top_k,omitempty" jsonschema:"max members to include per community (default 10)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

type CommunityMember struct {
	QualifiedName   string  `json:"qualified_name"`
	Package         string  `json:"package,omitempty"`
	Kind            string  `json:"kind"`
	Path            string  `json:"path"`
	StartLine       int     `json:"start_line"`
	InDegree        int     `json:"in_degree,omitempty"`
	CrossPkgCallers int     `json:"cross_pkg_callers,omitempty"`
	PageRank        float64 `json:"page_rank,omitempty"`
}

type Community struct {
	ID      int               `json:"id"`
	Size    int               `json:"size"`
	Members []CommunityMember `json:"members"`
}

type CommunitiesOutput struct {
	Status      string      `json:"status"` // "ok" | "no-index" | "no-graph" | "error"
	Hint        string      `json:"hint,omitempty"`
	Project     string      `json:"project,omitempty"`
	Total       int         `json:"total"`
	Truncated   bool        `json:"truncated,omitempty"`
	Communities []Community `json:"communities,omitempty"`
	// Externals is the project's third-party/stdlib dependency paths, sorted —
	// fed to the orientation render's "external dependencies by capability"
	// section (#581). Best-effort; empty when the graph has no import edges.
	Externals []string `json:"externals,omitempty"`
	// Entrypoints is the file paths of the project's main() functions, sorted —
	// fed to the orientation render's "entrypoints" section (#581). Empty for a
	// library with no main.
	Entrypoints []string `json:"entrypoints,omitempty"`
}

func (s *Server) GraphCommunities(ctx context.Context, in CommunitiesInput) (CommunitiesOutput, error) {
	_, out, err := s.graphCommunities(ctx, nil, in)
	return out, err
}

func (s *Server) graphCommunities(ctx context.Context, _ *sdk.CallToolRequest, in CommunitiesInput) (*sdk.CallToolResult, CommunitiesOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, CommunitiesOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, CommunitiesOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}

	minMembers := in.MinMembers
	if minMembers <= 0 {
		minMembers = 3
	}
	k := in.K
	if k <= 0 {
		k = 20
	}
	if k > 50 {
		k = 50
	}
	topK := in.TopK
	if topK <= 0 {
		topK = 10
	}
	if topK > 50 {
		topK = 50
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, CommunitiesOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	communities, err := st.GraphCommunities(ctx, minMembers, k*2) // fetch 2× to allow topK trim
	if err != nil {
		return nil, CommunitiesOutput{Status: "error", Hint: fmt.Sprintf("communities: %v", err)}, nil
	}

	if len(communities) == 0 {
		return nil, CommunitiesOutput{
			Status: "no-graph", Project: p.Root,
			Hint: "no community data — run `dex index . --graph=only` to build the call graph and compute communities.",
		}, nil
	}

	out := CommunitiesOutput{Status: "ok", Project: p.Root, Total: len(communities)}
	// External dependency paths feed the orientation render's capability section
	// (#581). Best-effort — a query failure must not fail the communities call.
	if ext, err := st.ExternalImports(ctx); err == nil {
		out.Externals = ext
	}
	if eps, err := st.MainEntrypoints(ctx); err == nil {
		out.Entrypoints = eps
	}
	if len(communities) > k {
		communities = communities[:k]
		out.Truncated = true
	}
	for _, c := range communities {
		mc := Community{ID: c.ID, Size: len(c.Members)}
		members := c.Members
		if len(members) > topK {
			members = members[:topK]
		}
		for _, m := range members {
			mc.Members = append(mc.Members, CommunityMember{
				QualifiedName:   m.QualifiedName,
				Package:         m.PackagePath,
				Kind:            m.Kind,
				Path:            m.FilePath,
				StartLine:       m.StartLine,
				InDegree:        m.InDegree,
				CrossPkgCallers: m.CrossPkgCallers,
				PageRank:        m.PageRank,
			})
		}
		out.Communities = append(out.Communities, mc)
	}
	return nil, out, nil
}
