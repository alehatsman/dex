package mcp

import (
	"context"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// noopSurface is a default, all-stubs implementation of toolSurface. Types
// that only handle a subset of the surface embed noopSurface so that adding a
// new tool to the interface only requires updating Server (the real
// implementation) rather than every implementor.
//
// unavailMsg is returned as the error text for every stub; set it at
// construction time to produce context-appropriate messages.
type noopSurface struct {
	unavailMsg string
}

func (n *noopSurface) err() error { return errors.New(n.unavailMsg) }

func (n *noopSurface) contextRouter(_ context.Context, _ *sdk.CallToolRequest, _ ContextInput) (*sdk.CallToolResult, ContextOutput, error) {
	return nil, ContextOutput{}, n.err()
}
func (n *noopSurface) search(_ context.Context, _ *sdk.CallToolRequest, _ SearchInput) (*sdk.CallToolResult, SearchOutput, error) {
	return nil, SearchOutput{}, n.err()
}
func (n *noopSurface) findSymbol(_ context.Context, _ *sdk.CallToolRequest, _ FindSymbolInput) (*sdk.CallToolResult, FindSymbolOutput, error) {
	return nil, FindSymbolOutput{}, n.err()
}
func (n *noopSurface) related(_ context.Context, _ *sdk.CallToolRequest, _ RelatedInput) (*sdk.CallToolResult, RelatedOutput, error) {
	return nil, RelatedOutput{}, n.err()
}
func (n *noopSurface) findRelated(_ context.Context, _ *sdk.CallToolRequest, _ FindRelatedInput) (*sdk.CallToolResult, FindRelatedOutput, error) {
	return nil, FindRelatedOutput{}, n.err()
}
func (n *noopSurface) graphDeps(_ context.Context, _ *sdk.CallToolRequest, _ GraphDepsInput) (*sdk.CallToolResult, GraphDepsOutput, error) {
	return nil, GraphDepsOutput{}, n.err()
}
func (n *noopSurface) graphCallers(_ context.Context, _ *sdk.CallToolRequest, _ CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error) {
	return nil, CallEdgeOutput{}, n.err()
}
func (n *noopSurface) graphCallees(_ context.Context, _ *sdk.CallToolRequest, _ CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error) {
	return nil, CallEdgeOutput{}, n.err()
}
func (n *noopSurface) graphImpact(_ context.Context, _ *sdk.CallToolRequest, _ ImpactInput) (*sdk.CallToolResult, ImpactOutput, error) {
	return nil, ImpactOutput{}, n.err()
}
func (n *noopSurface) graphLinks(_ context.Context, _ *sdk.CallToolRequest, _ DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error) {
	return nil, DocLinkOutput{}, n.err()
}
func (n *noopSurface) graphBacklinks(_ context.Context, _ *sdk.CallToolRequest, _ DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error) {
	return nil, DocLinkOutput{}, n.err()
}
func (n *noopSurface) graphTags(_ context.Context, _ *sdk.CallToolRequest, _ TagInput) (*sdk.CallToolResult, TagOutput, error) {
	return nil, TagOutput{}, n.err()
}
func (n *noopSurface) graphCycles(_ context.Context, _ *sdk.CallToolRequest, _ CyclesInput) (*sdk.CallToolResult, CyclesOutput, error) {
	return nil, CyclesOutput{}, n.err()
}
func (n *noopSurface) graphPath(_ context.Context, _ *sdk.CallToolRequest, _ PathInput) (*sdk.CallToolResult, PathOutput, error) {
	return nil, PathOutput{}, n.err()
}
func (n *noopSurface) graphDiff(_ context.Context, _ *sdk.CallToolRequest, _ DiffInput) (*sdk.CallToolResult, DiffOutput, error) {
	return nil, DiffOutput{}, n.err()
}
func (n *noopSurface) graphCommunities(_ context.Context, _ *sdk.CallToolRequest, _ CommunitiesInput) (*sdk.CallToolResult, CommunitiesOutput, error) {
	return nil, CommunitiesOutput{}, n.err()
}
func (n *noopSurface) overview(_ context.Context, _ *sdk.CallToolRequest, _ OverviewInput) (*sdk.CallToolResult, OverviewOutput, error) {
	return nil, OverviewOutput{}, n.err()
}
func (n *noopSurface) smells(_ context.Context, _ *sdk.CallToolRequest, _ SmellsInput) (*sdk.CallToolResult, SmellsOutput, error) {
	return nil, SmellsOutput{}, n.err()
}
func (n *noopSurface) routes(_ context.Context, _ *sdk.CallToolRequest, _ RoutesInput) (*sdk.CallToolResult, RoutesOutput, error) {
	return nil, RoutesOutput{}, n.err()
}
func (n *noopSurface) searchTree(_ context.Context, _ *sdk.CallToolRequest, _ SearchTreeInput) (*sdk.CallToolResult, SearchTreeOutput, error) {
	return nil, SearchTreeOutput{}, n.err()
}
func (n *noopSurface) searchGrep(_ context.Context, _ *sdk.CallToolRequest, _ SearchGrepInput) (*sdk.CallToolResult, SearchGrepOutput, error) {
	return nil, SearchGrepOutput{}, n.err()
}
func (n *noopSurface) knowledge(_ context.Context, _ *sdk.CallToolRequest, _ KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error) {
	return nil, KnowledgeOutput{}, n.err()
}
func (n *noopSurface) session(_ context.Context, _ *sdk.CallToolRequest, _ SessionInput) (*sdk.CallToolResult, SessionOutput, error) {
	return nil, SessionOutput{}, n.err()
}
func (n *noopSurface) compressOutput(_ context.Context, _ *sdk.CallToolRequest, _ CompressInput) (*sdk.CallToolResult, CompressOutput, error) {
	return nil, CompressOutput{}, n.err()
}
func (n *noopSurface) shellRun(_ context.Context, _ *sdk.CallToolRequest, _ ShellInput) (*sdk.CallToolResult, ShellOutput, error) {
	return nil, ShellOutput{}, n.err()
}
func (n *noopSurface) status(_ context.Context, _ *sdk.CallToolRequest, _ StatusInput) (*sdk.CallToolResult, StatusOutput, error) {
	return nil, StatusOutput{}, n.err()
}
func (n *noopSurface) summarize(_ context.Context, _ *sdk.CallToolRequest, _ SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error) {
	return nil, SummarizeOutput{}, n.err()
}
func (n *noopSurface) compose(_ context.Context, _ *sdk.CallToolRequest, _ ComposeInput) (*sdk.CallToolResult, ComposeOutput, error) {
	return nil, ComposeOutput{}, n.err()
}
func (n *noopSurface) specVerify(_ context.Context, _ *sdk.CallToolRequest, _ SpecVerifyInput) (*sdk.CallToolResult, SpecVerifyOutput, error) {
	return nil, SpecVerifyOutput{}, n.err()
}
func (n *noopSurface) agent(_ context.Context, _ *sdk.CallToolRequest, _ AgentInput) (*sdk.CallToolResult, AgentOutput, error) {
	return nil, AgentOutput{}, n.err()
}
func (n *noopSurface) share(_ context.Context, _ *sdk.CallToolRequest, _ ShareInput) (*sdk.CallToolResult, ShareOutput, error) {
	return nil, ShareOutput{}, n.err()
}
func (n *noopSurface) ctxPack(_ context.Context, _ *sdk.CallToolRequest, _ PackInput) (*sdk.CallToolResult, PackOutput, error) {
	return nil, PackOutput{}, n.err()
}
func (n *noopSurface) nav(_ context.Context, _ *sdk.CallToolRequest, _ NavInput) (*sdk.CallToolResult, NavOutput, error) {
	return nil, NavOutput{}, n.err()
}
func (n *noopSurface) feedback(_ context.Context, _ *sdk.CallToolRequest, _ FeedbackInput) (*sdk.CallToolResult, FeedbackOutput, error) {
	return nil, FeedbackOutput{}, n.err()
}
func (n *noopSurface) prefetch(_ context.Context, _ *sdk.CallToolRequest, _ PrefetchInput) (*sdk.CallToolResult, PrefetchOutput, error) {
	return nil, PrefetchOutput{}, n.err()
}
func (n *noopSurface) workspaceSearch(_ context.Context, _ *sdk.CallToolRequest, _ WorkspaceSearchInput) (*sdk.CallToolResult, WorkspaceSearchOutput, error) {
	return nil, WorkspaceSearchOutput{}, n.err()
}
