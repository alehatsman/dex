package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestMaintenanceClientAllMethods verifies that every toolSurface method on
// maintenanceClient returns a non-nil error containing "maintenance" and
// returns immediately (no blocking).
func TestMaintenanceClientAllMethods(t *testing.T) {
	mc := &maintenanceClient{reason: "test"}
	ctx := context.Background()

	assertMaintenance := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: expected maintenance error, got nil", name)
			return
		}
		if !strings.Contains(err.Error(), "maintenance") {
			t.Errorf("%s: error %q does not mention maintenance", name, err)
		}
	}

	_, _, err := mc.contextRouter(ctx, nil, ContextInput{})
	assertMaintenance("contextRouter", err)
	_, _, err = mc.search(ctx, nil, SearchInput{})
	assertMaintenance("search", err)
	_, _, err = mc.findSymbol(ctx, nil, FindSymbolInput{})
	assertMaintenance("findSymbol", err)
	_, _, err = mc.related(ctx, nil, RelatedInput{})
	assertMaintenance("related", err)
	_, _, err = mc.findRelated(ctx, nil, FindRelatedInput{})
	assertMaintenance("findRelated", err)
	_, _, err = mc.graphDeps(ctx, nil, GraphDepsInput{})
	assertMaintenance("graphDeps", err)
	_, _, err = mc.graphCallers(ctx, nil, CallEdgeInput{})
	assertMaintenance("graphCallers", err)
	_, _, err = mc.graphCallees(ctx, nil, CallEdgeInput{})
	assertMaintenance("graphCallees", err)
	_, _, err = mc.graphImpact(ctx, nil, ImpactInput{})
	assertMaintenance("graphImpact", err)
	_, _, err = mc.graphLinks(ctx, nil, DocLinkInput{})
	assertMaintenance("graphLinks", err)
	_, _, err = mc.graphBacklinks(ctx, nil, DocLinkInput{})
	assertMaintenance("graphBacklinks", err)
	_, _, err = mc.graphTags(ctx, nil, TagInput{})
	assertMaintenance("graphTags", err)
	_, _, err = mc.graphCycles(ctx, nil, CyclesInput{})
	assertMaintenance("graphCycles", err)
	_, _, err = mc.graphPath(ctx, nil, PathInput{})
	assertMaintenance("graphPath", err)
	_, _, err = mc.graphDiff(ctx, nil, DiffInput{})
	assertMaintenance("graphDiff", err)
	_, _, err = mc.graphCommunities(ctx, nil, CommunitiesInput{})
	assertMaintenance("graphCommunities", err)
	_, _, err = mc.overview(ctx, nil, OverviewInput{})
	assertMaintenance("overview", err)
	_, _, err = mc.smells(ctx, nil, SmellsInput{})
	assertMaintenance("smells", err)
	_, _, err = mc.routes(ctx, nil, RoutesInput{})
	assertMaintenance("routes", err)
	_, _, err = mc.searchTree(ctx, nil, SearchTreeInput{})
	assertMaintenance("searchTree", err)
	_, _, err = mc.searchGrep(ctx, nil, SearchGrepInput{})
	assertMaintenance("searchGrep", err)
	_, _, err = mc.knowledge(ctx, nil, KnowledgeInput{})
	assertMaintenance("knowledge", err)
	_, _, err = mc.session(ctx, nil, SessionInput{})
	assertMaintenance("session", err)
	_, _, err = mc.compressOutput(ctx, nil, CompressInput{})
	assertMaintenance("compressOutput", err)
	_, _, err = mc.shellRun(ctx, nil, ShellInput{})
	assertMaintenance("shellRun", err)
	_, _, err = mc.status(ctx, nil, StatusInput{})
	assertMaintenance("status", err)
	_, _, err = mc.summarize(ctx, nil, SummarizeInput{})
	assertMaintenance("summarize", err)
	_, _, err = mc.compose(ctx, nil, ComposeInput{})
	assertMaintenance("compose", err)
	_, _, err = mc.specVerify(ctx, nil, SpecVerifyInput{})
	assertMaintenance("specVerify", err)
	_, _, err = mc.agent(ctx, nil, AgentInput{})
	assertMaintenance("agent", err)
	_, _, err = mc.share(ctx, nil, ShareInput{})
	assertMaintenance("share", err)
	_, _, err = mc.ctxPack(ctx, nil, PackInput{})
	assertMaintenance("ctxPack", err)
	_, _, err = mc.nav(ctx, nil, NavInput{})
	assertMaintenance("nav", err)
	_, _, err = mc.feedback(ctx, nil, FeedbackInput{})
	assertMaintenance("feedback", err)
	_, _, err = mc.prefetch(ctx, nil, PrefetchInput{})
	assertMaintenance("prefetch", err)
	_, _, err = mc.workspaceSearch(ctx, nil, WorkspaceSearchInput{})
	assertMaintenance("workspaceSearch", err)
}

func TestMaintenanceInstructions(t *testing.T) {
	plain := maintenanceInstructions("")
	if !strings.Contains(plain, "maintenance") || !strings.Contains(plain, "native tools") {
		t.Errorf("plain instructions missing expected keywords: %q", plain)
	}

	withReason := maintenanceInstructions("upgrading GPU drivers")
	if !strings.Contains(withReason, "upgrading GPU drivers") {
		t.Errorf("reason not embedded in instructions: %q", withReason)
	}
}
