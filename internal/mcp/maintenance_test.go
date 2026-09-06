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
	mc := &maintenanceClient{noopSurface: noopSurface{unavailMsg: maintenanceMsg("test")}}
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
	_, _, err = mc.related(ctx, nil, RelatedInput{})
	assertMaintenance("related", err)
	_, _, err = mc.graphDeps(ctx, nil, GraphDepsInput{})
	assertMaintenance("graphDeps", err)
	_, _, err = mc.graphCallers(ctx, nil, CallEdgeInput{})
	assertMaintenance("graphCallers", err)
	_, _, err = mc.graphCallees(ctx, nil, CallEdgeInput{})
	assertMaintenance("graphCallees", err)
	_, _, err = mc.graphImpact(ctx, nil, ImpactInput{})
	assertMaintenance("graphImpact", err)
	_, _, err = mc.graphPath(ctx, nil, PathInput{})
	assertMaintenance("graphPath", err)
	_, _, err = mc.graphDiff(ctx, nil, DiffInput{})
	assertMaintenance("graphDiff", err)
	_, _, err = mc.graphCommunities(ctx, nil, CommunitiesInput{})
	assertMaintenance("graphCommunities", err)
	_, _, err = mc.smells(ctx, nil, SmellsInput{})
	assertMaintenance("smells", err)
	_, _, err = mc.routes(ctx, nil, RoutesInput{})
	assertMaintenance("routes", err)
	_, _, err = mc.searchGrep(ctx, nil, SearchGrepInput{})
	assertMaintenance("searchGrep", err)
	_, _, err = mc.status(ctx, nil, StatusInput{})
	assertMaintenance("status", err)
	_, _, err = mc.summarize(ctx, nil, SummarizeInput{})
	assertMaintenance("summarize", err)
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
