// Package retrieve is the query-time answer-composition service: given a
// project's store, embedder, and (optional) chat backend, it routes a
// free-text question to an intent, runs the search lanes (semantic +
// symbol + graph), fuses and enriches the hits, and composes the bundle
// the `ask` surface returns. It is the domain layer the MCP transport and
// the CLI both call; it depends on store/graph/graphquery/embed/chat but
// never on the transport — results are returned as neutral types the
// caller maps to its own wire structs.
package retrieve

import (
	"regexp"
	"strings"
)

// ─── intent vocabulary ────────────────────────────────────────────────────

const (
	IntentAuto            = "auto"
	IntentBehaviorSearch  = "behavior_search"
	IntentSymbolLookup    = "symbol_lookup"
	IntentCallers         = "callers"
	IntentCallees         = "callees"
	IntentArchitecture    = "architecture"
	IntentPackageTopology = "package_topology"
	IntentEditingContext  = "editing_context"
	// IntentOrient (#135, spec step 5) is whole-repo orientation: the question's
	// subject is the repository itself ("understand this repo", "overview of the
	// codebase"), not a specific component. The transport answers it from the
	// deterministic orient bundle (L0/L1 map + build/test commands) rather than
	// the semantic-search + synthesis path — same output as an empty question.
	IntentOrient = "orient"
	// IntentAssemble (#687) is an explicit-only mode: ask assembles a
	// budget-bounded working set instead of answering. It is never auto-routed
	// (no keyword/identifier heuristic selects it) — the agent opts in. Its
	// retrieval reuses the default lanes; what differs is that symbol bodies are
	// inlined by submodular keyword coverage and prose synthesis is suppressed.
	IntentAssemble = "assemble"
	// IntentReview (#144, ask-merge slice 5a) routes "review my changes" to the
	// per-hunk review composition instead of the search lanes — the everyday
	// review loop (edit → verify_change → review) reached from the four-verb
	// front door. The result is delta-shaped, so the transport carries it in a
	// discriminated-union field (ContextOutput.Review) rather than the
	// state-shaped lanes. The auto path only targets the working tree; targeted
	// PR/branch/ref review stays on the review_diff tool / `dex review` CLI.
	IntentReview = "review"
)

var validIntents = map[string]struct{}{
	IntentAuto: {}, IntentBehaviorSearch: {}, IntentSymbolLookup: {},
	IntentCallers: {}, IntentCallees: {}, IntentArchitecture: {},
	IntentPackageTopology: {}, IntentEditingContext: {}, IntentAssemble: {},
	IntentOrient: {}, IntentReview: {},
}

// Identifier detection patterns. Conservative — false positives are
// cheap (we just run search_symbol and get nothing) but false negatives
// mean we miss the structural fast path.
var (
	// (*Type).Method or Type.Method — receiver-qualified Go-style names.
	reQualifiedSymbol = regexp.MustCompile(`\(\*?[A-Z][A-Za-z0-9_]*\)\.[A-Za-z_][A-Za-z0-9_]*|\b[A-Z][A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_]*\b`)
	// Bare PascalCase identifier of length ≥ 3 (skip "I", "Go", noise).
	reBarePascal = regexp.MustCompile(`\b[A-Z][A-Za-z0-9_]{2,}\b`)
	// camelCase — lowercase start with an internal uppercase transition
	// (e.g. `inlineContent`, `markDirty`). Required for Go unexported
	// identifiers; the uppercase transition keeps plain English words
	// out (no English word has a mid-word capital).
	reCamel = regexp.MustCompile(`\b[a-z][a-z0-9]*[A-Z][A-Za-z0-9_]*\b`)
	// snake_case_with_underscores — at least one underscore so we don't
	// flag plain words.
	reSnake = regexp.MustCompile(`\b[a-z][a-z0-9_]*_[a-z0-9_]+\b`)

	// Intent keyword regexes for auto routing.
	// #144: code review. Two strong, narrow signals: (1) an imperative "review"
	// at the head of the question ("review my changes", "code review this pr",
	// "please review the diff"), and (2) a "review <possessive> <diff-noun>"
	// object anywhere. Checked FIRST so it wins outright. It deliberately does
	// NOT match "how does the review verb work" (subject "how", object not a
	// diff-noun) — that stays behavior_search. Bare "code" is excluded from the
	// diff-nouns so "review the code" isn't stolen from behavior_search.
	reReview = regexp.MustCompile(`(?:^\s*(?:please\s+|can you\s+|could you\s+)?(?:code\s+)?review\b)|` +
		`\breview (?:my|the|this|these|your) (?:changes?|diffs?|prs?|pull requests?|patch|work|commits?|branch|edits?|changeset)\b`)
	// #135: whole-repo orientation. Narrow by construction — the subject must be
	// the repository itself (an explicit repo|repository|codebase|project|code
	// noun), or a subjectless "orient me" command. This steals only the
	// repo-scoped variants of overview/structure/walkthrough from reArchitecture;
	// "how does the watcher work" / "overview of the graph package" keep their
	// component subject and stay architecture. Checked before reArchitecture.
	reOrient = regexp.MustCompile(`\b(` +
		`orient me|get me oriented|help me get oriented|where (?:do|should) i start|` +
		`(?:understand|navigate|explore|tour|walk me through|walk through|explain|describe|give me (?:an? )?(?:overview|tour) of) (?:this|the|our) (?:repo|repository|codebase|code ?base|project)|` +
		`(?:overview|tour|structure|layout|shape|high[- ]level (?:overview|view)) of (?:this|the|our) (?:repo|repository|codebase|code ?base|project)|` +
		`(?:what is|what's|what does) (?:this|the) (?:repo|repository|codebase|code ?base|project)(?: do| contain| look like)?|` +
		`how (?:is|are) (?:this|the|our) (?:repo|repository|codebase|code ?base|project) (?:structured|organized|organised|laid out|set up|put together)` +
		`)\b`)
	reCallers      = regexp.MustCompile(`\b(callers?|who calls|what calls|called by|usage of|usages of|references? to|where is .* used|where is .* called)\b`)
	reCallees      = regexp.MustCompile(`\b(callees?|what does .* call|calls from|outgoing calls|dependencies of)\b`)
	reArchitecture = regexp.MustCompile(`\b(architecture|how does .* work|overview|big picture|design of|walk me through|how is .* organized)\b`)
	// #118: require the *plural* "packages" (or a topology compound). A bare
	// singular "package" is a common noun ("the graph package", "the store
	// package") and never a topology signal — matching it stole architecture
	// and callers questions whose real subject just happened to end in "package".
	rePackages = regexp.MustCompile(`\b(packages|modules?|topology|dependency graph|import graph|package layout)\b`)
	// `change` / `update` deliberately omitted — they fire on questions
	// like "when X changes" or "update the timestamp on Y" that are
	// really behavior_search, not editing_context.
	reEditing = regexp.MustCompile(`\b(edit|modify|refactor|rename|extend|fix|patch|implement|add)\b`)
)

// ─── intent classification ────────────────────────────────────────────────

// IntentCandidates carries side data the lanes consume: identifiers
// detected in the question that should feed search_symbol.
type IntentCandidates struct {
	Identifiers []string // ranked best-first (qualified before bare)
}

// ResolveIntent picks an intent and surfaces side data (detected
// identifiers). Priority:
//
//  1. Explicit intent override when valid and not "auto".
//  2. Keyword regex on the question.
//  3. Identifier-shaped tokens → symbol_lookup.
//  4. Default: behavior_search.
func ResolveIntent(question, intent string) (string, IntentCandidates) {
	cand := IntentCandidates{Identifiers: ExtractIdentifiers(question)}

	explicit := strings.ToLower(strings.TrimSpace(intent))
	if explicit != "" && explicit != IntentAuto {
		if _, ok := validIntents[explicit]; ok {
			return explicit, cand
		}
		// Invalid override falls through to auto routing.
	}

	q := strings.ToLower(question)
	switch {
	case reReview.MatchString(q):
		return IntentReview, cand
	case reOrient.MatchString(q):
		return IntentOrient, cand
	case reCallers.MatchString(q):
		return IntentCallers, cand
	case reCallees.MatchString(q):
		return IntentCallees, cand
	case rePackages.MatchString(q):
		return IntentPackageTopology, cand
	case reArchitecture.MatchString(q):
		return IntentArchitecture, cand
	case reEditing.MatchString(q):
		return IntentEditingContext, cand
	}

	if len(cand.Identifiers) > 0 && looksLikeBareIdentifierQuery(question) {
		return IntentSymbolLookup, cand
	}
	return IntentBehaviorSearch, cand
}

// looksLikeBareIdentifierQuery returns true when the question is short
// enough and identifier-dominated that the user likely wants a symbol
// lookup rather than a behavior search. Heuristic, but keeps
// "(*Store).Search" from being routed to behavior_search.
func looksLikeBareIdentifierQuery(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" {
		return false
	}
	words := strings.Fields(q)
	// 1-3 words AND at least one identifier-shaped token.
	return len(words) <= 3
}

// ExtractIdentifiers pulls identifier-shaped tokens out of a question,
// ranked best-first (qualified symbols before bare Pascal/camel/snake),
// for the symbol lane to look up.
func ExtractIdentifiers(q string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	// Pass 1: qualified symbols. Track their byte spans so we can skip
	// bare-Pascal matches that fall inside (e.g. "Store" and "Search"
	// inside "(*Store).Search" are noise once the qualified form is
	// recorded).
	type span struct{ lo, hi int }
	var taken []span
	for _, idx := range reQualifiedSymbol.FindAllStringIndex(q, -1) {
		add(q[idx[0]:idx[1]])
		taken = append(taken, span{idx[0], idx[1]})
	}
	inside := func(lo, hi int) bool {
		for _, sp := range taken {
			if lo >= sp.lo && hi <= sp.hi {
				return true
			}
		}
		return false
	}

	for _, idx := range reBarePascal.FindAllStringIndex(q, -1) {
		if inside(idx[0], idx[1]) {
			continue
		}
		add(q[idx[0]:idx[1]])
	}
	for _, idx := range reCamel.FindAllStringIndex(q, -1) {
		if inside(idx[0], idx[1]) {
			continue
		}
		add(q[idx[0]:idx[1]])
	}
	for _, idx := range reSnake.FindAllStringIndex(q, -1) {
		if inside(idx[0], idx[1]) {
			continue
		}
		add(q[idx[0]:idx[1]])
	}

	// Fallback for single-word lowercase queries (e.g. `rerank`,
	// `index`, `embed`). None of the regexes above pick these up —
	// they require camelCase, PascalCase, or underscore shape — but
	// they're a perfectly valid form for Go's unexported identifiers
	// and short package names. When the question is literally one
	// short token and we have nothing yet, treat the token as the
	// identifier to look up. Guarded by length and content so a single
	// English word like "fix" or "bug" doesn't dominate.
	if len(out) == 0 {
		trimmed := strings.TrimSpace(q)
		if len(trimmed) >= 3 && len(trimmed) <= 32 && isAllIdentChars(trimmed) {
			out = append(out, trimmed)
		}
	}
	return out
}

// isAllIdentChars reports whether every byte in s is a valid Go
// identifier character (letter, digit, or underscore). Used by the
// single-token fallback in ExtractIdentifiers to avoid passing
// punctuation/whitespace to search_symbol.
func isAllIdentChars(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isIdentChar(s[i]) {
			return false
		}
	}
	return true
}

// isIdentChar reports whether b is a valid Go identifier byte.
func isIdentChar(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}
