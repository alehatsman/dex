package mcp

import (
	"context"
	"testing"
)

// powerTools are the granular lanes gated behind DEX_EXPERT.
// They must be absent from the default surface and present once expert is on.
var powerTools = []string{
	"deps",
	"clusters", "routes", "smells", "status", "session",
	"plan_rename", "rehearse_patch", "check",
	"repo_map",
}

// defaultVerbs are the zero-inference verbs that headline the default surface;
// they don't need an embedder or chat model, so a lean stubServer advertises
// them regardless of DEX_EXPERT. notes joined this lane in #548 — persistent
// memory needs no models and is useless if the agent can't write it.
// ask is default when no embed (lean); repo_map moved to expert.
var defaultVerbs = []string{"trace", "ask", "grep", "notes", "act", "remember"}

func TestExpertGatingHidesPowerToolsByDefault(t *testing.T) {
	t.Setenv("DEX_EXPERT", "") // explicit: default surface, power tier off
	names := listToolNames(t, stubServer(t))

	for _, v := range defaultVerbs {
		if !names[v] {
			t.Errorf("default surface omitted verb %q; want it advertised", v)
		}
	}
	for _, p := range powerTools {
		if names[p] {
			t.Errorf("default surface advertised power tool %q; want it gated behind DEX_EXPERT", p)
		}
	}
}

func TestExpertGatingExposesPowerToolsWhenEnabled(t *testing.T) {
	t.Setenv("DEX_EXPERT", "1")
	names := listToolNames(t, stubServer(t))

	for _, p := range powerTools {
		if !names[p] {
			t.Errorf("DEX_EXPERT=1 but power tool %q not advertised", p)
		}
	}
	// The default verbs stay put — the expert tier is additive, not a swap.
	for _, v := range defaultVerbs {
		if !names[v] {
			t.Errorf("DEX_EXPERT=1 dropped default verb %q", v)
		}
	}
}

func TestExpertEnabledParsing(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"no":    false,
		"off":   false,
		"OFF":   false,
		"1":     true,
		"true":  true,
		"yes":   true,
		"on":    true,
		"x":     true,
	}
	for v, want := range cases {
		t.Setenv("DEX_EXPERT", v)
		if got := expertEnabled(); got != want {
			t.Errorf("expertEnabled() with DEX_EXPERT=%q = %v, want %v", v, got, want)
		}
	}
}

// TestTraceVerbDispatch exercises the routing in traceVerb: direction defaulting,
// the path destination guard, and that each direction reaches its handler and
// stamps the direction back. A lean stubServer (no index) makes the underlying
// handlers return a clean status rather than data — enough to prove wiring.
func TestTraceVerbDispatch(t *testing.T) {
	srv := stubServer(t)
	ctx := context.Background()

	t.Run("defaults to callers", func(t *testing.T) {
		_, out, err := traceVerb(ctx, srv, nil, TraceInput{Symbol: "Foo"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Direction != "callers" {
			t.Errorf("empty direction = %q, want defaulted to callers", out.Direction)
		}
	})

	t.Run("callees routes", func(t *testing.T) {
		_, out, _ := traceVerb(ctx, srv, nil, TraceInput{Symbol: "Foo", Direction: "callees"})
		if out.Direction != "callees" {
			t.Errorf("direction = %q, want callees", out.Direction)
		}
	})

	t.Run("path requires to", func(t *testing.T) {
		_, out, _ := traceVerb(ctx, srv, nil, TraceInput{Symbol: "Foo", Direction: "path"})
		if out.Status != "error" {
			t.Errorf("path without `to` = status %q, want error", out.Status)
		}
	})

	t.Run("path routes with to", func(t *testing.T) {
		_, out, _ := traceVerb(ctx, srv, nil, TraceInput{Symbol: "A", Direction: "path", To: "B"})
		if out.Direction != "path" {
			t.Errorf("direction = %q, want path", out.Direction)
		}
		if out.Status == "" {
			t.Error("path routed but Status empty; handler not reached")
		}
	})

	t.Run("impact routes", func(t *testing.T) {
		_, out, _ := traceVerb(ctx, srv, nil, TraceInput{Symbol: "Foo", Direction: "impact"})
		if out.Direction != "impact" {
			t.Errorf("direction = %q, want impact", out.Direction)
		}
		if out.Status == "" {
			t.Error("impact routed but Status empty; handler not reached")
		}
	})

	t.Run("unknown direction errors without touching the graph", func(t *testing.T) {
		_, out, _ := traceVerb(ctx, srv, nil, TraceInput{Symbol: "Foo", Direction: "sideways"})
		if out.Status != "error" {
			t.Errorf("bad direction = status %q, want error", out.Status)
		}
	})
}
