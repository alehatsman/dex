package retrieve

import (
	"fmt"
	"testing"
)

// Router-accuracy harness (#110-9). ResolveIntent is the pivot the four-verb
// cutover rests on: collapsing ask/look/act/remember over the eight intents is
// only safe if auto-routing stays accurate. This is the gate — a labelled
// goal→intent corpus measured against a ratchet threshold. Do not collapse to
// four verbs on faith.
//
// Cases are labelled by TRUE human intent, not by what the router currently
// returns. Genuine misroutes therefore show up as accuracy < 100% — that is the
// signal the gate exists to protect. The threshold is a ratchet set just below
// the measured baseline: it fails CI when a change *regresses* routing, without
// pretending the heuristic router is perfect.
//
// Measured baseline as of #118: 40/44 = 90.9%. #118 tightened rePackages so a
// bare singular "package" (a common noun) no longer shadows architecture —
// "overview of the graph package" now routes to architecture. The remaining
// four misroutes are honest vocabulary gaps the gate documents rather than
// hides: callers-vocab ("references PruneIndex", "depends on the store
// package"), callees-vocab ("functions Assemble invokes"), and arch-vocab
// ("explain how ask builds a response"). Floor is set at 0.88 (~1-case slack)
// so single-case noise doesn't fail CI but a genuine regression does.
//
// When you improve the router, raise routerAccuracyFloor to the new measured
// value. When you add a case, keep the label honest and re-measure.
const routerAccuracyFloor = 0.88

// routerCorpus spans the seven auto-routable intents (IntentAssemble is
// explicit-only — never auto-routed — so it is intentionally absent).
var routerCorpus = []struct {
	q    string
	want string
}{
	// ── callers: who calls / uses / references X ──
	{"who calls ResolveIntent", IntentCallers},
	{"find all callers of (*Store).Search", IntentCallers},
	{"what calls into the embedder", IntentCallers},
	{"where is markDirty used", IntentCallers},
	{"usages of ExtractIdentifiers", IntentCallers},
	{"what references PruneIndex", IntentCallers},                           // "references" needs "references to" — MISS (behavior)
	{"show me everything that depends on the store package", IntentCallers}, // no keyword — MISS

	// ── callees: what X calls / its outgoing edges ──
	{"what does Assemble call", IntentCallees},
	{"outgoing calls from ResolveIntent", IntentCallees},
	{"dependencies of the indexer", IntentCallees},
	{"list the functions Assemble invokes", IntentCallees}, // no "does .* call" — MISS (behavior)
	{"what does the router call downstream", IntentCallees},

	// ── architecture: how it works / overview / design ──
	{"how does indexing work", IntentArchitecture},
	{"give me an overview of the graph package", IntentArchitecture},
	{"walk me through the query pipeline", IntentArchitecture},
	{"design of the embedding cache", IntentArchitecture},
	{"big picture of how ask composes a response", IntentArchitecture},
	{"how is the store organized", IntentArchitecture},
	{"explain how ask builds a response", IntentArchitecture}, // "explain" not a keyword — MISS (behavior)

	// ── package_topology: packages / imports / layout ──
	{"show the package topology", IntentPackageTopology},
	{"what's the import graph", IntentPackageTopology},
	{"package layout of internal", IntentPackageTopology},
	{"which packages depend on graphquery", IntentPackageTopology},
	{"draw the dependency graph", IntentPackageTopology},

	// ── editing_context: edit / fix / refactor / add / implement ──
	{"fix the rerank pool overflow", IntentEditingContext},
	{"refactor the intent router", IntentEditingContext},
	{"rename ResolveIntent to RouteIntent", IntentEditingContext},
	{"add a timeout to the embedder", IntentEditingContext},
	{"implement the trust envelope", IntentEditingContext},
	{"patch the stale check", IntentEditingContext},
	{"extend the corpus with new cases", IntentEditingContext},

	// ── symbol_lookup: short, identifier-dominated ──
	{"(*Store).Search", IntentSymbolLookup},
	{"ResolveIntent", IntentSymbolLookup},
	{"inlineContent", IntentSymbolLookup},
	{"PruneIndex", IntentSymbolLookup},
	{"EnrichGraph", IntentSymbolLookup},

	// ── behavior_search: default; how/where/what/when without a structural cue ──
	{"where do we open the SQLite store", IntentBehaviorSearch},
	{"where does the cache invalidate when chunks change", IntentBehaviorSearch},
	{"what triggers a reindex", IntentBehaviorSearch},
	{"how are stale files detected", IntentBehaviorSearch},
	{"when does the debouncer flush", IntentBehaviorSearch},
	{"what happens when an embedding request times out", IntentBehaviorSearch},
	{"why is the trust envelope nil on empty results", IntentBehaviorSearch},
	{"where are graph edges persisted", IntentBehaviorSearch},
}

func TestRouterAccuracy(t *testing.T) {
	type miss struct{ q, want, got string }
	var misses []miss
	for _, c := range routerCorpus {
		got, _ := ResolveIntent(c.q, "auto")
		if got != c.want {
			misses = append(misses, miss{c.q, c.want, got})
		}
	}

	total := len(routerCorpus)
	correct := total - len(misses)
	acc := float64(correct) / float64(total)
	t.Logf("router accuracy: %d/%d = %.1f%% (floor %.0f%%)", correct, total, acc*100, routerAccuracyFloor*100)
	for _, m := range misses {
		t.Logf("  misroute: %-55q want=%-18s got=%s", m.q, m.want, m.got)
	}

	if acc < routerAccuracyFloor {
		t.Errorf("router accuracy %.1f%% regressed below floor %.1f%% — %d/%d misrouted:\n%s",
			acc*100, routerAccuracyFloor*100, len(misses), total, formatMisses(misses))
	}
}

func formatMisses[T any](misses []T) string {
	s := ""
	for _, m := range misses {
		s += fmt.Sprintf("  %+v\n", m)
	}
	return s
}
