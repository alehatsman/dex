package mcp

import (
	"strings"
	"testing"
)

// This file is the registration + budget half of the S5 cutover gate (#195, spec
// specs/two-verb-surface.md). The router harness (router_accuracy_test.go) proves
// inputs route to the right lane; these tests prove the *surface* actually
// collapsed: exactly one tool headlines it, every deleted verb is unreachable at
// both tiers, and the system prompt shrank accordingly. A clean break with no
// alias step means the old names must be GONE, not merely demoted.

// removedVerbs are the tools deleted across S2–S4 (#196/#197/#198) and the #205
// record cut. None may register at either the default or the DEX_EXPERT tier — a
// clean break, no aliases, no deprecation window (spec §"no backward compat"):
//   - ask, look      → merged into query (S2, #196)
//   - act, shell, verify_change, checkpoint → advisory-only cut (S3, #197)
//   - remember        → renamed to record (S4, #198)
//   - notes, session, budget → dropped from the MCP surface (S4, #198);
//     notes' admin tail is CLI-only, session dedup stays internal.
//   - record          → removed with the L3 knowledge subsystem (#205); dex is
//     retrieval over the codebase, not agent memory.
var removedVerbs = []string{
	"ask", "look",
	"act", "shell", "verify_change", "checkpoint",
	"remember", "record",
	"notes", "session", "budget",
}

// TestCutoverEverydaySurfaceIsExactlyOneTool asserts the default surface is
// EXACTLY {query} — no more, no less. The pre-existing expert-gating tests only
// check that query is present and the power lanes absent; this pins the set so a
// stray everyday-tier registration can't slip in.
func TestCutoverEverydaySurfaceIsExactlyOneTool(t *testing.T) {
	t.Setenv("DEX_EXPERT", "") // default surface, power tier off
	names := listToolNames(t, stubServer(t))

	want := map[string]bool{"query": true}
	for n := range names {
		if !want[n] {
			t.Errorf("default surface advertised unexpected tool %q; want exactly {query}", n)
		}
	}
	for n := range want {
		if !names[n] {
			t.Errorf("default surface missing verb %q", n)
		}
	}
	if len(names) != len(want) {
		t.Errorf("default surface has %d tools %v; want exactly 1 {query}", len(names), toolNameSet(names))
	}
}

// TestCutoverRemovedVerbsUnreachable proves the deleted verbs are gone at BOTH
// tiers — the default surface AND the DEX_EXPERT power tier. The clean break
// (#196–#198) deleted the registrations outright; a reachable old name at either
// tier would mean a stale alias survived.
func TestCutoverRemovedVerbsUnreachable(t *testing.T) {
	for _, expert := range []string{"", "1"} {
		tier := "default"
		if expert != "" {
			tier = "expert"
		}
		t.Run(tier, func(t *testing.T) {
			t.Setenv("DEX_EXPERT", expert)
			names := listToolNames(t, stubServer(t))
			for _, v := range removedVerbs {
				if names[v] {
					t.Errorf("%s tier still advertises removed verb %q; the S2–S4 cut left an alias", tier, v)
				}
			}
		})
	}
}

// TestServerInstructionsBudget asserts the single-verb system prompt is well under
// the four-verb ceiling (~800 tokens, spec §"S5 validation"). ServerInstructions
// is the whole per-session prompt cost; the collapse should have shrunk it, so
// the gate ratchets it below a budget that stays comfortably under four verbs'.
// A ~4-chars/token estimate is coarse but stable — good enough to catch prompt
// bloat creeping back in.
func TestServerInstructionsBudget(t *testing.T) {
	const (
		fourVerbCeiling = 800 // the four-verb surface's prompt budget (spec)
		budget          = 600 // single-verb ceiling, comfortably under four verbs'
	)
	instr := ServerInstructions()
	tokens := (len(instr) + 3) / 4 // ceil(len/4)
	t.Logf("ServerInstructions: %d chars ≈ %d tokens (budget %d, four-verb ceiling %d)",
		len(instr), tokens, budget, fourVerbCeiling)

	if tokens > budget {
		t.Errorf("system prompt ≈ %d tokens, over the two-verb budget of %d", tokens, budget)
	}
	// The prompt must still name query — a budget met by dropping guidance
	// would be a false economy.
	for _, v := range []string{"query"} {
		if !strings.Contains(instr, v) {
			t.Errorf("ServerInstructions no longer mentions %q", v)
		}
	}
	// And must not resurrect a removed verb as a headline instruction.
	for _, dead := range []string{"act(command)", "remember(fact)", "ask(", "look("} {
		if strings.Contains(instr, dead) {
			t.Errorf("ServerInstructions still instructs the removed surface %q", dead)
		}
	}
}

func toolNameSet(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
