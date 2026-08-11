package mcp

// Anti-accretion lint (#139, spec step 10). Epic #110 exists because the tool
// surface accreted into a frankenstein: tool descriptions that negotiate with
// each other for the agent's attention ("use X instead", "when Y is overkill",
// two tools both claiming to be the "primary entry point"). This lint is the
// standing guard that keeps that from creeping back — a ceiling on the count of
// sibling-negotiation phrases, mirroring the router-accuracy floor: it documents
// the current debt (logging every offender) and fails any change that adds new
// negotiation phrasing. Lowering antiAccretionCeiling is the point; raising it
// needs a reviewed reason, exactly like moving routerAccuracyFloor.

import (
	"regexp"
	"testing"
)

// antiAccretionCeiling is the maximum number of sibling-negotiation offenses
// tolerated across the default tool surface. Set to the measured baseline; drive
// it toward zero as descriptions are cleaned, one tool at a time.
//
// Baseline = 0: the lone offense (ask's description negotiating with brief) was
// retired when brief was folded into ask(assemble) and removed (#141). A future
// change that reintroduces sibling-negotiation phrasing must reword it, not raise
// this floor without a reviewed reason.
const antiAccretionCeiling = 0

// siblingNegotiation matches phrasing that only exists because tools compete:
//   - redirection to a different tool ("use X instead") — anchored on the "use
//     <tok> instead" idiom so it does NOT fire on generic prose like "surface it
//     instead of leaking into chat", which redirects nothing.
//   - self-measurement against a sibling's weight ("X is overkill", "overkill").
//
// Contested primacy ("primary entry point" on >1 tool) is handled separately
// below, since it is a cross-tool count, not a per-description substring.
var siblingNegotiation = regexp.MustCompile("(?i)\\buse\\s+[`\"']?\\w+[`\"']?\\s+instead\\b|\\boverkill\\b")

var primacyClaim = regexp.MustCompile(`(?i)primary\s+entry[- ]?point|primary\s+entrypoint`)

func TestAntiAccretionLint(t *testing.T) {
	// Default profile only: the surface the agent sees by default. Expert/lean
	// are supersets/subsets that would double-count shared tools.
	t.Setenv("DEX_EXPERT", "")
	srv := stubServer(t)
	tools := listToolSchemas(t, srv)

	offenses := 0
	primacyTools := make([]string, 0, 2)
	for _, tool := range tools {
		desc := tool.Description
		for _, m := range siblingNegotiation.FindAllString(desc, -1) {
			offenses++
			t.Logf("sibling-negotiation offense — %s: %q", tool.Name, m)
		}
		if primacyClaim.MatchString(desc) {
			primacyTools = append(primacyTools, tool.Name)
		}
	}

	// One front door is fine; every tool past the first that also claims primacy
	// is negotiating. Count only the excess.
	if excess := len(primacyTools) - 1; excess > 0 {
		offenses += excess
		t.Logf("contested primacy — %d tools claim 'primary entry point': %v", len(primacyTools), primacyTools)
	}

	t.Logf("anti-accretion: %d offense(s) across %d tools (ceiling %d)", offenses, len(tools), antiAccretionCeiling)
	if offenses > antiAccretionCeiling {
		t.Errorf("sibling-negotiation offenses = %d, exceeds ceiling %d — a description is negotiating with a sibling; "+
			"reword it to tell its own story, or raise the ceiling with a reviewed reason", offenses, antiAccretionCeiling)
	}
}
