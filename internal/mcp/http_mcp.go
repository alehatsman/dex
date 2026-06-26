package mcp

// Native streamable-HTTP MCP transport for `dex serve` — the end state that
// retires the `dex mcp --remote` stdio shim (remote.go). A client attaches
// dex directly over MCP with {"type":"http","url":".../v1/projects/{id}/mcp"};
// no proxy process. The handler is mounted on the same mux as the REST
// surface (http.go) and gated by the same bearer auth.
//
// Each project gets its own *sdk.Server, scoped via a projectScoped wrapper
// that injects the registry root into every tool Input before delegating to
// the shared *Server handlers — the same override the REST handlers apply via
// their bind funcs, so a client cannot reach a different project. Tools are
// registered through the shared registerTools path (server.go), so the
// HTTP-MCP surface is byte-identical to stdio and the remote shim.

import (
	"context"
	"net/http"

	"github.com/alehatsman/dex/internal/profiles"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// projectScoped adapts a *Server to a toolSurface pinned to one project
// root. Every handler stamps the bound root onto its Input's project field
// (ContextInput.Project; every other Input's ProjectRoot; status is global
// and untouched) and then calls the underlying handler. This mirrors the
// per-Input bind funcs in buildHTTPHandler so the HTTP-MCP and REST surfaces
// resolve the project the same way.
type projectScoped struct {
	s    *Server
	root string
}

func (p projectScoped) contextRouter(ctx context.Context, req *sdk.CallToolRequest, in ContextInput) (*sdk.CallToolResult, ContextOutput, error) {
	in.ProjectRoot = p.root
	return p.s.contextRouter(ctx, req, in)
}

func (p projectScoped) locate(ctx context.Context, req *sdk.CallToolRequest, in LocateInput) (*sdk.CallToolResult, LocateOutput, error) {
	in.ProjectRoot = p.root
	return p.s.locate(ctx, req, in)
}

func (p projectScoped) review(ctx context.Context, req *sdk.CallToolRequest, in ReviewInput) (*sdk.CallToolResult, ReviewOutput, error) {
	in.ProjectRoot = p.root
	return p.s.review(ctx, req, in)
}

func (p projectScoped) refactor(ctx context.Context, req *sdk.CallToolRequest, in RefactorInput) (*sdk.CallToolResult, RefactorOutput, error) {
	in.ProjectRoot = p.root
	return p.s.refactor(ctx, req, in)
}

func (p projectScoped) cohort(ctx context.Context, req *sdk.CallToolRequest, in CohortInput) (*sdk.CallToolResult, CohortOutput, error) {
	in.ProjectRoot = p.root
	return p.s.cohort(ctx, req, in)
}

func (p projectScoped) verify(ctx context.Context, req *sdk.CallToolRequest, in VerifyInput) (*sdk.CallToolResult, VerifyOutput, error) {
	in.ProjectRoot = p.root
	return p.s.verify(ctx, req, in)
}

func (p projectScoped) checkpoint(ctx context.Context, req *sdk.CallToolRequest, in CheckpointInput) (*sdk.CallToolResult, CheckpointOutput, error) {
	in.ProjectRoot = p.root
	return p.s.checkpoint(ctx, req, in)
}

func (p projectScoped) search(ctx context.Context, req *sdk.CallToolRequest, in SearchInput) (*sdk.CallToolResult, SearchOutput, error) {
	in.ProjectRoot = p.root
	return p.s.search(ctx, req, in)
}

func (p projectScoped) findSymbol(ctx context.Context, req *sdk.CallToolRequest, in FindSymbolInput) (*sdk.CallToolResult, FindSymbolOutput, error) {
	in.ProjectRoot = p.root
	return p.s.findSymbol(ctx, req, in)
}

func (p projectScoped) related(ctx context.Context, req *sdk.CallToolRequest, in RelatedInput) (*sdk.CallToolResult, RelatedOutput, error) {
	in.ProjectRoot = p.root
	return p.s.related(ctx, req, in)
}
func (p projectScoped) graphDeps(ctx context.Context, req *sdk.CallToolRequest, in GraphDepsInput) (*sdk.CallToolResult, GraphDepsOutput, error) {
	in.ProjectRoot = p.root
	return p.s.graphDeps(ctx, req, in)
}

func (p projectScoped) graphCallers(ctx context.Context, req *sdk.CallToolRequest, in CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error) {
	in.ProjectRoot = p.root
	return p.s.graphCallers(ctx, req, in)
}

func (p projectScoped) graphCallees(ctx context.Context, req *sdk.CallToolRequest, in CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error) {
	in.ProjectRoot = p.root
	return p.s.graphCallees(ctx, req, in)
}

func (p projectScoped) graphImpact(ctx context.Context, req *sdk.CallToolRequest, in ImpactInput) (*sdk.CallToolResult, ImpactOutput, error) {
	in.ProjectRoot = p.root
	return p.s.graphImpact(ctx, req, in)
}

func (p projectScoped) graphLinks(ctx context.Context, req *sdk.CallToolRequest, in DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error) {
	in.ProjectRoot = p.root
	return p.s.graphLinks(ctx, req, in)
}

func (p projectScoped) graphBacklinks(ctx context.Context, req *sdk.CallToolRequest, in DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error) {
	in.ProjectRoot = p.root
	return p.s.graphBacklinks(ctx, req, in)
}

func (p projectScoped) graphTags(ctx context.Context, req *sdk.CallToolRequest, in TagInput) (*sdk.CallToolResult, TagOutput, error) {
	in.ProjectRoot = p.root
	return p.s.graphTags(ctx, req, in)
}

func (p projectScoped) summarize(ctx context.Context, req *sdk.CallToolRequest, in SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error) {
	in.ProjectRoot = p.root
	return p.s.summarize(ctx, req, in)
}
func (p projectScoped) smells(ctx context.Context, req *sdk.CallToolRequest, in SmellsInput) (*sdk.CallToolResult, SmellsOutput, error) {
	in.ProjectRoot = p.root
	return p.s.smells(ctx, req, in)
}

func (p projectScoped) routes(ctx context.Context, req *sdk.CallToolRequest, in RoutesInput) (*sdk.CallToolResult, RoutesOutput, error) {
	in.ProjectRoot = p.root
	return p.s.routes(ctx, req, in)
}

func (p projectScoped) knowledge(ctx context.Context, req *sdk.CallToolRequest, in KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error) {
	in.ProjectRoot = p.root
	return p.s.knowledge(ctx, req, in)
}

func (p projectScoped) session(ctx context.Context, req *sdk.CallToolRequest, in SessionInput) (*sdk.CallToolResult, SessionOutput, error) {
	in.ProjectRoot = p.root
	return p.s.session(ctx, req, in)
}

func (p projectScoped) searchTree(ctx context.Context, req *sdk.CallToolRequest, in SearchTreeInput) (*sdk.CallToolResult, SearchTreeOutput, error) {
	in.ProjectRoot = p.root
	return p.s.searchTree(ctx, req, in)
}

func (p projectScoped) searchGrep(ctx context.Context, req *sdk.CallToolRequest, in SearchGrepInput) (*sdk.CallToolResult, SearchGrepOutput, error) {
	in.ProjectRoot = p.root
	return p.s.searchGrep(ctx, req, in)
}

// shellRun is not project-scoped; the cwd in ShellInput governs the working directory.
func (p projectScoped) shellRun(ctx context.Context, req *sdk.CallToolRequest, in ShellInput) (*sdk.CallToolResult, ShellOutput, error) {
	return p.s.shellRun(ctx, req, in)
}

// status is daemon-global (not project-scoped), so the bound root is ignored
// — matching the REST handleStatus and the stdio index_status tool.
func (p projectScoped) status(ctx context.Context, req *sdk.CallToolRequest, in StatusInput) (*sdk.CallToolResult, StatusOutput, error) {
	return p.s.status(ctx, req, in)
}

func (p projectScoped) budget(ctx context.Context, req *sdk.CallToolRequest, in BudgetInput) (*sdk.CallToolResult, BudgetOutput, error) {
	in.ProjectRoot = p.root
	return p.s.budget(ctx, req, in)
}
func (p projectScoped) graphCycles(ctx context.Context, req *sdk.CallToolRequest, in CyclesInput) (*sdk.CallToolResult, CyclesOutput, error) {
	in.ProjectRoot = p.root
	return p.s.graphCycles(ctx, req, in)
}

func (p projectScoped) graphPath(ctx context.Context, req *sdk.CallToolRequest, in PathInput) (*sdk.CallToolResult, PathOutput, error) {
	in.ProjectRoot = p.root
	return p.s.graphPath(ctx, req, in)
}

func (p projectScoped) graphDiff(ctx context.Context, req *sdk.CallToolRequest, in DiffInput) (*sdk.CallToolResult, DiffOutput, error) {
	in.ProjectRoot = p.root
	return p.s.graphDiff(ctx, req, in)
}

func (p projectScoped) graphCommunities(ctx context.Context, req *sdk.CallToolRequest, in CommunitiesInput) (*sdk.CallToolResult, CommunitiesOutput, error) {
	in.ProjectRoot = p.root
	return p.s.graphCommunities(ctx, req, in)
}

func (p projectScoped) check(ctx context.Context, req *sdk.CallToolRequest, in CheckInput) (*sdk.CallToolResult, CheckOutput, error) {
	in.ProjectRoot = p.root
	return p.s.check(ctx, req, in)
}

// newMCPHandler builds the streamable-HTTP MCP handler mounted at
// /v1/projects/{id}/mcp. One *sdk.Server is prebuilt per registry project (the
// SDK permits reusing a server across sessions) and looked up by the {id} path
// value; an unknown id returns nil, which the SDK serves as 400. nil when the
// registry is empty.
func (s *Server) newMCPHandler(projects map[string]string) http.Handler {
	if len(projects) == 0 {
		return nil
	}

	chatAvailable := s.ChatClient != nil
	embedAvailable := s.EmbedClient != nil

	servers := make(map[string]*sdk.Server, len(projects))
	for id, root := range projects {
		srv := sdk.NewServer(&sdk.Implementation{Name: "dex", Version: Version}, nil)
		registerTools(srv, projectScoped{s: s, root: root}, chatAvailable, embedAvailable, profiles.Active(root).StrictAnchors(), descriptionModeFromEnv())
		servers[id] = srv
	}

	// JSONResponse: dex tools are pure request/response (no server-initiated
	// messages), so replying application/json per POST is simpler and avoids
	// holding an SSE stream open per call. Clients may still open the optional
	// standalone SSE GET; dex just never pushes over it.
	return sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		// Accept the full id or an unambiguous prefix (the boot banner
		// prints a 12-char prefix). Unknown or ambiguous => nil => 400.
		if canonical, ok, _ := resolveRegistryID(r.PathValue("id"), servers); ok {
			return servers[canonical]
		}
		return nil
	}, &sdk.StreamableHTTPOptions{JSONResponse: true})
}
