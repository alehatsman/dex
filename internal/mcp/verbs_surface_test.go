package mcp

import (
	"context"
	"reflect"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file locks the surface-level guarantees the four-verb spec
// (specs/tool-surface.md, "Validation") makes about the exact verbs. They are
// cheap invariants, but they are the guard rail the riskier deepening steps
// (session spine, ask-merge) lean on: if a later change quietly drops `trust`
// from an envelope or lets an exact verb claim a non-exact provenance, one of
// these fails before it ships.

// exactVerbOutputs is the set of exact-verb (look/act/remember) envelope types.
// ask is deliberately excluded: it is the one verb allowed to infer, and it
// carries a richer trust envelope (confidence + semantic/name-based provenance),
// so it does not share this shape.
func exactVerbOutputs() map[string]reflect.Type {
	return map[string]reflect.Type{
		"look":     reflect.TypeOf(LookOutput{}),
		"act":      reflect.TypeOf(ActOutput{}),
		"remember": reflect.TypeOf(RememberOutput{}),
	}
}

// TestEnvelopeUniformity — spec Validation "Envelope uniformity": every exact
// verb returns the same top-level shape (status/trust/next), so the agent learns
// the envelope once. A new verb that skips any of these fields fails here.
func TestEnvelopeUniformity(t *testing.T) {
	want := map[string]reflect.Type{
		"Status": reflect.TypeOf(""),
		"Trust":  reflect.TypeOf(EnvTrust{}),
		"Next":   reflect.TypeOf([]NextStep(nil)),
	}
	for verb, typ := range exactVerbOutputs() {
		for name, wantType := range want {
			f, ok := typ.FieldByName(name)
			if !ok {
				t.Errorf("%s output %s is missing the %q envelope field", verb, typ.Name(), name)
				continue
			}
			if f.Type != wantType {
				t.Errorf("%s output %s.%s is %s, want %s", verb, typ.Name(), name, f.Type, wantType)
			}
		}
	}
}

// TestOneFuzzyVerbInvariant — spec Validation "One-fuzzy-verb invariant": only
// `ask` may return a non-exact provenance. Every exact-verb envelope path,
// including error paths, stamps provenance "exact". Driven behaviorally against a
// bare stub (no index) so it also proves the early-return/error paths — the
// easiest place for an envelope to forget its trust stamp.
func TestOneFuzzyVerbInvariant(t *testing.T) {
	h := stubServer(t)
	ctx := context.Background()
	req := &sdk.CallToolRequest{}

	check := func(verb, provenance string) {
		if provenance != "exact" {
			t.Errorf("%s returned provenance %q, want exact — only ask may infer", verb, provenance)
		}
	}

	// act: empty command → error path.
	if _, out, _ := actVerb(ctx, h, req, ActInput{}); true {
		check("act(empty)", out.Trust.Provenance)
	}
	// remember: recall against no index.
	if _, out, _ := rememberVerb(ctx, h, req, RememberInput{Query: "anything"}); true {
		check("remember(recall)", out.Trust.Provenance)
	}
	// look: every classified lane, error paths included (no index under the stub).
	for _, target := range []string{"internal/x.go", "/needle/", "someSymbol", "x.go:12"} {
		_, out, _ := lookVerb(ctx, h, req, LookInput{Target: target})
		check("look("+target+")", out.Trust.Provenance)
	}
	// look: caller error (empty target) still stamps exact.
	if _, out, _ := lookVerb(ctx, h, req, LookInput{}); true {
		check("look(empty)", out.Trust.Provenance)
	}
}
