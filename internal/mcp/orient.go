package mcp

import (
	"context"

	"github.com/alehatsman/dex/internal/codemap"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// orientResponse handles ask called with an empty question: the session-start
// orientation path (#348 / #316 story 6). It returns the deterministic L0
// overview plus an L1 zoom into the most-central cluster so an agent names the
// right package/cluster before any find() — zero inference, byte-stable, so the
// injected text stays cache-friendly across a session. The bundle is rendered
// through codemap.RenderOrient, the same path `dex orient` uses, so the CLI and
// MCP surfaces agree by construction.
//
// The community parameters mirror `dex map` (min-members 3, k 50, top-k 25) so
// orientation, map(), and the nav-bench routing lane all see the same clusters.
func (s *Server) orientResponse(ctx context.Context, in ContextInput) (*sdk.CallToolResult, ContextOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, ContextOutput{Status: "error", Hint: hint}, nil
	}
	out := ContextOutput{Project: p.Root, Intent: "orient"}

	comm, err := s.GraphCommunities(ctx, CommunitiesInput{MinMembers: 3, K: 50, TopK: 25, ProjectRoot: p.Root})
	if err != nil {
		out.Status = "error"
		out.Hint = err.Error()
		return nil, out, nil
	}
	if comm.Status != "ok" {
		out.Status = comm.Status
		out.Hint = comm.Hint
		if out.Hint == "" {
			out.Hint = "no call graph indexed — run `dex index . --graph=only`, or pass a question to search semantically."
		}
		return nil, out, nil
	}

	clusters := adaptCommunities(comm.Communities)
	out.Status = "ok"
	out.Map = codemap.RenderOrient(clusters, codemap.DefaultL0Budget, codemap.DefaultL1Budget)
	out.NextAction = "Name the cluster matching your task, then find(\"<concept>\", \"<pkg>\") within it — you're already oriented; skip the broad map() call."
	out.Avoid = "Don't fan out grep/Read to discover layout — the map above already routes you to the right package."
	return nil, out, nil
}
