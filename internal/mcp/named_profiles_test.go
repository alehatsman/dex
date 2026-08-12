package mcp

import (
	"context"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// registeredToolNames drives registerTools directly for a chosen profile shape
// (embedAvailable/weakModel) and DEX_EXPERT state, enumerating the advertised
// tools over an in-memory transport. This is the only seam that can force the
// weak-model profile: the production path derives it from profiles.Active, which
// is process-global (sync.Once) and not test-injectable.
func registeredToolNames(t *testing.T, embedAvailable, weakModel bool) map[string]bool {
	t.Helper()
	ctx := context.Background()
	srv := sdk.NewServer(&sdk.Implementation{Name: "dex", Version: "test"}, nil)
	registerTools(srv, projectScoped{s: stubServer(t), root: t.TempDir()},
		false /*chatAvailable*/, embedAvailable, weakModel, descriptionModeFromEnv())

	st, ct := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	names := map[string]bool{}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	return names
}

// The four verbs are constant across every profile (#110 step 8): ask · look · act
// · remember appear whether or not an embedder is wired and whether or not the
// deployment is a weak local model. This is the regression guard for the bug where
// registerTools gated remember behind `if !weakModel` — a weak model silently lost
// durable memory. The verb set never changes with deployment; only ask's internal
// capability degrades (that path is exercised elsewhere).
func TestFourVerbsConstantAcrossProfiles(t *testing.T) {
	verbs := []string{"ask", "look", "act", "remember"}
	profiles := []struct {
		name           string
		embedAvailable bool
		weakModel      bool
	}{
		{"full", true, false},
		{"bm25-only", false, false},
		{"lean/weak", false, true},
		{"weak-with-embedder", true, true},
	}
	for _, p := range profiles {
		names := registeredToolNames(t, p.embedAvailable, p.weakModel)
		for _, v := range verbs {
			if !names[v] {
				t.Errorf("[%s] verb %q missing — the four verbs must be constant across profiles", p.name, v)
			}
		}
	}
}

// A profile never adds power lanes on its own: with DEX_EXPERT unset, every profile
// (including the weak-model one that previously suppressed the everyday tools
// entirely) exposes EXACTLY the four verbs — no expert tool leaks in.
func TestNonExpertProfilesExposeOnlyFourVerbs(t *testing.T) {
	t.Setenv("DEX_EXPERT", "")
	for _, weak := range []bool{false, true} {
		names := registeredToolNames(t, false, weak)
		if len(names) != 4 {
			t.Errorf("weak=%v: expected exactly 4 verbs, got %d: %v", weak, len(names), names)
		}
		for _, leaked := range []string{"search", "trace", "shell", "grep", "read", "notes", "review_diff"} {
			if names[leaked] {
				t.Errorf("weak=%v: expert tool %q leaked into the non-expert surface", weak, leaked)
			}
		}
	}
}

// DEX_EXPERT is an additive overlay, orthogonal to the profile: setting it exposes
// the power lanes even for a weak local model (an explicit operator opt-in), while
// the four verbs remain present.
func TestExpertOverlayIsOrthogonalToProfile(t *testing.T) {
	t.Setenv("DEX_EXPERT", "1")
	for _, weak := range []bool{false, true} {
		names := registeredToolNames(t, true, weak)
		if !names["remember"] || !names["ask"] {
			t.Errorf("weak=%v: four verbs must survive the expert overlay", weak)
		}
		if !names["trace"] || !names["shell"] {
			t.Errorf("weak=%v: DEX_EXPERT power lanes must overlay regardless of profile", weak)
		}
	}
}
