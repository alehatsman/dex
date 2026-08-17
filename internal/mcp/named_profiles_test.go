package mcp

import (
	"context"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// registeredToolNames drives registerTools directly for a chosen embedder
// availability and DEX_EXPERT state, enumerating the advertised tools over an
// in-memory transport. Post-#149 registerTools no longer takes a weakModel flag:
// the tool set cannot branch on the model profile by construction. The weak-model
// behavior is ask's call-time capability degradation (via process-global
// profiles.Active), exercised elsewhere — not a difference in the advertised set.
func registeredToolNames(t *testing.T, embedAvailable bool) map[string]bool {
	t.Helper()
	ctx := context.Background()
	srv := sdk.NewServer(&sdk.Implementation{Name: "dex", Version: "test"}, nil)
	registerTools(srv, projectScoped{s: stubServer(t), root: t.TempDir()},
		embedAvailable, descriptionModeFromEnv())

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

// The everyday verbs are constant across every profile (#110 step 8, #196):
// query · act · remember appear whether or not an embedder is wired. This is the
// regression guard for the bug where registerTools gated remember behind
// `if !weakModel` — a weak model silently lost durable memory. Post-#149 the gate
// is structurally impossible (registerTools takes no model flag); the verb set
// never changes with deployment, only query's internal capability degrades
// (exercised elsewhere). (Post-#196 the two read verbs ask+look are one: query.)
func TestVerbsConstantAcrossProfiles(t *testing.T) {
	verbs := []string{"query", "act", "remember"}
	profiles := []struct {
		name           string
		embedAvailable bool
	}{
		{"full", true},
		{"bm25-only", false},
	}
	for _, p := range profiles {
		names := registeredToolNames(t, p.embedAvailable)
		for _, v := range verbs {
			if !names[v] {
				t.Errorf("[%s] verb %q missing — the everyday verbs must be constant across profiles", p.name, v)
			}
		}
	}
}

// A profile never adds power lanes on its own: with DEX_EXPERT unset, every profile
// (including the weak-model one that previously suppressed the everyday tools
// entirely) exposes EXACTLY the everyday verbs — no expert tool leaks in.
func TestNonExpertProfilesExposeOnlyEverydayVerbs(t *testing.T) {
	t.Setenv("DEX_EXPERT", "")
	for _, embed := range []bool{false, true} {
		names := registeredToolNames(t, embed)
		if len(names) != 3 {
			t.Errorf("embed=%v: expected exactly 3 verbs (query·act·remember), got %d: %v", embed, len(names), names)
		}
		for _, leaked := range []string{"ask", "look", "search", "trace", "shell", "grep", "read", "notes", "review_diff"} {
			if names[leaked] {
				t.Errorf("embed=%v: tool %q leaked into the non-expert surface", embed, leaked)
			}
		}
	}
}

// DEX_EXPERT is an additive overlay, orthogonal to the profile: setting it exposes
// the power lanes even for a weak local model (an explicit operator opt-in), while
// the everyday verbs remain present.
func TestExpertOverlayIsOrthogonalToProfile(t *testing.T) {
	t.Setenv("DEX_EXPERT", "1")
	for _, embed := range []bool{false, true} {
		names := registeredToolNames(t, embed)
		if !names["remember"] || !names["query"] {
			t.Errorf("embed=%v: everyday verbs must survive the expert overlay", embed)
		}
		if !names["trace"] || !names["shell"] {
			t.Errorf("embed=%v: DEX_EXPERT power lanes must overlay regardless of profile", embed)
		}
	}
}
