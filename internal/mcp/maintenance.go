package mcp

import (
	"context"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// maintenanceInstructions returns the MCP server instructions block for
// maintenance mode. Claude Code receives this at session init and should
// immediately fall back to native tools for the duration of the session.
func maintenanceInstructions(reason string) string {
	msg := "dex is under maintenance — use native tools instead:\n\n" +
		"- Read (not file_view) for file contents\n" +
		"- Bash with grep/rg (not search_semantic/search_symbol) for search\n" +
		"- Bash (not ctx_shell) for shell commands\n" +
		"- Manual cross-reference tracing (not graph_callers/graph_callees)\n\n" +
		"Do NOT call any dex MCP tools — they will all return maintenance errors.\n" +
		"Resume normal dex usage once the maintenance window ends."
	if reason != "" {
		msg = "dex is under maintenance (" + reason + ") — use native tools instead:\n\n" +
			"- Read (not file_view) for file contents\n" +
			"- Bash with grep/rg (not search_semantic/search_symbol) for search\n" +
			"- Bash (not ctx_shell) for shell commands\n" +
			"- Manual cross-reference tracing (not graph_callers/graph_callees)\n\n" +
			"Do NOT call any dex MCP tools — they will all return maintenance errors.\n" +
			"Resume normal dex usage once the maintenance window ends."
	}
	return msg
}

// RunStdioMaintenance runs a stub MCP server on stdio that registers the full
// dex tool surface but returns an immediate maintenance error on every call.
// Use this while the real dex daemon or index is unavailable so agents receive
// instant guidance to fall back to native tools instead of hanging on timeouts.
func RunStdioMaintenance(ctx context.Context, reason string) error {
	srv := sdk.NewServer(&sdk.Implementation{Name: "dex", Version: Version}, &sdk.ServerOptions{
		Instructions: maintenanceInstructions(reason),
	})
	mc := &maintenanceClient{reason: reason}
	// Register the full power tier so every tool the agent might call is
	// present and returns an immediate error — TierPower ensures no tool is
	// silently absent (which would cause the SDK to return "unknown tool").
	registerTools(srv, mc, TierPower, true, true, DescModeFull)
	return srv.Run(ctx, &sdk.StdioTransport{})
}

// maintenanceClient implements toolSurface. Every method returns immediately
// with an error that explains dex is under maintenance and instructs the agent
// to use native tools. No network calls, no index access, no GPU usage.
type maintenanceClient struct {
	reason string
}

func (mc *maintenanceClient) msg() error {
	if mc.reason != "" {
		return fmt.Errorf("dex maintenance (%s): use native tools — Read for files, Bash/grep for search", mc.reason)
	}
	return fmt.Errorf("dex maintenance: use native tools — Read for files, Bash/grep for search")
}

func (mc *maintenanceClient) contextRouter(_ context.Context, _ *sdk.CallToolRequest, _ ContextInput) (*sdk.CallToolResult, ContextOutput, error) {
	return nil, ContextOutput{}, mc.msg()
}
func (mc *maintenanceClient) search(_ context.Context, _ *sdk.CallToolRequest, _ SearchInput) (*sdk.CallToolResult, SearchOutput, error) {
	return nil, SearchOutput{}, mc.msg()
}
func (mc *maintenanceClient) findSymbol(_ context.Context, _ *sdk.CallToolRequest, _ FindSymbolInput) (*sdk.CallToolResult, FindSymbolOutput, error) {
	return nil, FindSymbolOutput{}, mc.msg()
}
func (mc *maintenanceClient) related(_ context.Context, _ *sdk.CallToolRequest, _ RelatedInput) (*sdk.CallToolResult, RelatedOutput, error) {
	return nil, RelatedOutput{}, mc.msg()
}
func (mc *maintenanceClient) findRelated(_ context.Context, _ *sdk.CallToolRequest, _ FindRelatedInput) (*sdk.CallToolResult, FindRelatedOutput, error) {
	return nil, FindRelatedOutput{}, mc.msg()
}
func (mc *maintenanceClient) graphDeps(_ context.Context, _ *sdk.CallToolRequest, _ GraphDepsInput) (*sdk.CallToolResult, GraphDepsOutput, error) {
	return nil, GraphDepsOutput{}, mc.msg()
}
func (mc *maintenanceClient) graphCallers(_ context.Context, _ *sdk.CallToolRequest, _ CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error) {
	return nil, CallEdgeOutput{}, mc.msg()
}
func (mc *maintenanceClient) graphCallees(_ context.Context, _ *sdk.CallToolRequest, _ CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error) {
	return nil, CallEdgeOutput{}, mc.msg()
}
func (mc *maintenanceClient) graphImpact(_ context.Context, _ *sdk.CallToolRequest, _ ImpactInput) (*sdk.CallToolResult, ImpactOutput, error) {
	return nil, ImpactOutput{}, mc.msg()
}
func (mc *maintenanceClient) graphLinks(_ context.Context, _ *sdk.CallToolRequest, _ DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error) {
	return nil, DocLinkOutput{}, mc.msg()
}
func (mc *maintenanceClient) graphBacklinks(_ context.Context, _ *sdk.CallToolRequest, _ DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error) {
	return nil, DocLinkOutput{}, mc.msg()
}
func (mc *maintenanceClient) graphTags(_ context.Context, _ *sdk.CallToolRequest, _ TagInput) (*sdk.CallToolResult, TagOutput, error) {
	return nil, TagOutput{}, mc.msg()
}
func (mc *maintenanceClient) graphCycles(_ context.Context, _ *sdk.CallToolRequest, _ CyclesInput) (*sdk.CallToolResult, CyclesOutput, error) {
	return nil, CyclesOutput{}, mc.msg()
}
func (mc *maintenanceClient) graphPath(_ context.Context, _ *sdk.CallToolRequest, _ PathInput) (*sdk.CallToolResult, PathOutput, error) {
	return nil, PathOutput{}, mc.msg()
}
func (mc *maintenanceClient) graphDiff(_ context.Context, _ *sdk.CallToolRequest, _ DiffInput) (*sdk.CallToolResult, DiffOutput, error) {
	return nil, DiffOutput{}, mc.msg()
}
func (mc *maintenanceClient) graphCommunities(_ context.Context, _ *sdk.CallToolRequest, _ CommunitiesInput) (*sdk.CallToolResult, CommunitiesOutput, error) {
	return nil, CommunitiesOutput{}, mc.msg()
}
func (mc *maintenanceClient) overview(_ context.Context, _ *sdk.CallToolRequest, _ OverviewInput) (*sdk.CallToolResult, OverviewOutput, error) {
	return nil, OverviewOutput{}, mc.msg()
}
func (mc *maintenanceClient) smells(_ context.Context, _ *sdk.CallToolRequest, _ SmellsInput) (*sdk.CallToolResult, SmellsOutput, error) {
	return nil, SmellsOutput{}, mc.msg()
}
func (mc *maintenanceClient) routes(_ context.Context, _ *sdk.CallToolRequest, _ RoutesInput) (*sdk.CallToolResult, RoutesOutput, error) {
	return nil, RoutesOutput{}, mc.msg()
}
func (mc *maintenanceClient) searchTree(_ context.Context, _ *sdk.CallToolRequest, _ SearchTreeInput) (*sdk.CallToolResult, SearchTreeOutput, error) {
	return nil, SearchTreeOutput{}, mc.msg()
}
func (mc *maintenanceClient) searchGrep(_ context.Context, _ *sdk.CallToolRequest, _ SearchGrepInput) (*sdk.CallToolResult, SearchGrepOutput, error) {
	return nil, SearchGrepOutput{}, mc.msg()
}
func (mc *maintenanceClient) knowledge(_ context.Context, _ *sdk.CallToolRequest, _ KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error) {
	return nil, KnowledgeOutput{}, mc.msg()
}
func (mc *maintenanceClient) session(_ context.Context, _ *sdk.CallToolRequest, _ SessionInput) (*sdk.CallToolResult, SessionOutput, error) {
	return nil, SessionOutput{}, mc.msg()
}
func (mc *maintenanceClient) compressOutput(_ context.Context, _ *sdk.CallToolRequest, _ CompressInput) (*sdk.CallToolResult, CompressOutput, error) {
	return nil, CompressOutput{}, mc.msg()
}
func (mc *maintenanceClient) shellRun(_ context.Context, _ *sdk.CallToolRequest, _ ShellInput) (*sdk.CallToolResult, ShellOutput, error) {
	return nil, ShellOutput{}, mc.msg()
}
func (mc *maintenanceClient) status(_ context.Context, _ *sdk.CallToolRequest, _ StatusInput) (*sdk.CallToolResult, StatusOutput, error) {
	return nil, StatusOutput{}, mc.msg()
}
func (mc *maintenanceClient) summarize(_ context.Context, _ *sdk.CallToolRequest, _ SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error) {
	return nil, SummarizeOutput{}, mc.msg()
}
func (mc *maintenanceClient) compose(_ context.Context, _ *sdk.CallToolRequest, _ ComposeInput) (*sdk.CallToolResult, ComposeOutput, error) {
	return nil, ComposeOutput{}, mc.msg()
}
func (mc *maintenanceClient) specVerify(_ context.Context, _ *sdk.CallToolRequest, _ SpecVerifyInput) (*sdk.CallToolResult, SpecVerifyOutput, error) {
	return nil, SpecVerifyOutput{}, mc.msg()
}
func (mc *maintenanceClient) agent(_ context.Context, _ *sdk.CallToolRequest, _ AgentInput) (*sdk.CallToolResult, AgentOutput, error) {
	return nil, AgentOutput{}, mc.msg()
}
func (mc *maintenanceClient) share(_ context.Context, _ *sdk.CallToolRequest, _ ShareInput) (*sdk.CallToolResult, ShareOutput, error) {
	return nil, ShareOutput{}, mc.msg()
}
func (mc *maintenanceClient) ctxPack(_ context.Context, _ *sdk.CallToolRequest, _ PackInput) (*sdk.CallToolResult, PackOutput, error) {
	return nil, PackOutput{}, mc.msg()
}
func (mc *maintenanceClient) nav(_ context.Context, _ *sdk.CallToolRequest, _ NavInput) (*sdk.CallToolResult, NavOutput, error) {
	return nil, NavOutput{}, mc.msg()
}
func (mc *maintenanceClient) feedback(_ context.Context, _ *sdk.CallToolRequest, _ FeedbackInput) (*sdk.CallToolResult, FeedbackOutput, error) {
	return nil, FeedbackOutput{}, mc.msg()
}
func (mc *maintenanceClient) prefetch(_ context.Context, _ *sdk.CallToolRequest, _ PrefetchInput) (*sdk.CallToolResult, PrefetchOutput, error) {
	return nil, PrefetchOutput{}, mc.msg()
}
func (mc *maintenanceClient) workspaceSearch(_ context.Context, _ *sdk.CallToolRequest, _ WorkspaceSearchInput) (*sdk.CallToolResult, WorkspaceSearchOutput, error) {
	return nil, WorkspaceSearchOutput{}, mc.msg()
}
