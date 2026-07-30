package mcp

import "testing"

// TestIsReviewIntent guards the review-intent detector (#83): assessment tasks
// trigger the inline structural pack; ordinary navigate-and-edit tasks do not.
func TestIsReviewIntent(t *testing.T) {
	review := []string{
		"do an architecture review of the indexing pipeline",
		"audit the MCP server for structural problems",
		"find god modules and high coupling in internal/graph",
		"assess tech debt across the retrieve package",
		"where is the code smell / duplication hotspot",
		"REFACTOR the storage layer", // case-insensitive
		"review the coupling between mcp and store",
	}
	for _, task := range review {
		if !isReviewIntent(task) {
			t.Errorf("isReviewIntent(%q) = false, want true", task)
		}
	}

	navigate := []string{
		"add a project_root field to SmellsInput",
		"fix the panic in the tree-sitter parser",
		"who calls graphCommunities",
		"rename the embed engine env var",
		"add a project_root flag to the smells command", // feature name, not a review
		"",
	}
	for _, task := range navigate {
		if isReviewIntent(task) {
			t.Errorf("isReviewIntent(%q) = true, want false", task)
		}
	}
}

// TestDominantPackage picks the most common non-empty package as the cluster label.
func TestDominantPackage(t *testing.T) {
	members := []CommunityMember{
		{Package: "internal/graph"},
		{Package: "internal/graph"},
		{Package: "internal/mcp"},
		{Package: ""},
	}
	if got := dominantPackage(members); got != "internal/graph" {
		t.Errorf("dominantPackage = %q, want internal/graph", got)
	}
	if got := dominantPackage(nil); got != "" {
		t.Errorf("dominantPackage(nil) = %q, want empty", got)
	}
}
