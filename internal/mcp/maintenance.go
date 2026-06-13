package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// maintenanceInstructions returns the MCP server instructions block for
// maintenance mode. Claude Code receives this at session init and should
// immediately fall back to native tools for the duration of the session.
func maintenanceInstructions(reason string) string {
	msg := "dex is under maintenance — use native tools instead:\n\n" +
		"- Read (not dex read) for file contents\n" +
		"- Bash with grep/rg (not dex find/lookup) for search\n" +
		"- Bash (not dex shell) for shell commands\n" +
		"- Manual cross-reference tracing (not dex trace/impact)\n\n" +
		"Do NOT call any dex MCP tools — they will all return maintenance errors.\n" +
		"Resume normal dex usage once the maintenance window ends."
	if reason != "" {
		msg = "dex is under maintenance (" + reason + ") — use native tools instead:\n\n" +
			"- Read (not dex read) for file contents\n" +
			"- Bash with grep/rg (not dex find/lookup) for search\n" +
			"- Bash (not dex shell) for shell commands\n" +
			"- Manual cross-reference tracing (not dex trace/impact)\n\n" +
			"Do NOT call any dex MCP tools — they will all return maintenance errors.\n" +
			"Resume normal dex usage once the maintenance window ends."
	}
	return msg
}

func maintenanceMsg(reason string) string {
	if reason != "" {
		return "dex maintenance (" + reason + "): use native tools — Read for files, Bash/grep for search"
	}
	return "dex maintenance: use native tools — Read for files, Bash/grep for search"
}

// RunStdioMaintenance runs a stub MCP server on stdio that registers the full
// dex tool surface but returns an immediate maintenance error on every call.
// Use this while the real dex daemon or index is unavailable so agents receive
// instant guidance to fall back to native tools instead of hanging on timeouts.
func RunStdioMaintenance(ctx context.Context, reason string) error {
	srv := sdk.NewServer(&sdk.Implementation{Name: "dex", Version: Version}, &sdk.ServerOptions{
		Instructions: maintenanceInstructions(reason),
	})
	mc := &maintenanceClient{noopSurface: noopSurface{unavailMsg: maintenanceMsg(reason)}}
	// Register the full tool surface so every tool the agent might call is
	// present and returns an immediate error — ensures no tool is silently
	// absent (which would cause the SDK to return "unknown tool").
	registerTools(srv, mc, true, true, false, DescModeFull)
	return srv.Run(ctx, &sdk.StdioTransport{})
}

// maintenanceClient implements toolSurface via noopSurface. Every method
// returns the maintenance error message without network calls or index access.
// When new tools are added to toolSurface, only Server and remoteClient need
// updates; maintenanceClient inherits the noop stub automatically.
type maintenanceClient struct {
	noopSurface
}
