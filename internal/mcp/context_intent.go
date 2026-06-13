package mcp

import "strings"

// ─── intent classification ────────────────────────────────────────────────

// intentCandidates carries side data the lanes consume: identifiers
// detected in the question that should feed search_symbol.
type intentCandidates struct {
	identifiers []string // ranked best-first (qualified before bare)
}

// resolveIntent picks an intent and surfaces side data (detected
// identifiers). Priority:
//
//  1. Explicit Intent field (issue spec) when valid and not "auto".
//  2. Keyword regex on Question.
//  3. Identifier-shaped tokens → symbol_lookup.
//  4. Default: behavior_search.
func resolveIntent(in ContextInput) (string, intentCandidates) {
	cand := intentCandidates{identifiers: extractIdentifiers(in.Question)}

	explicit := strings.ToLower(strings.TrimSpace(in.Intent))
	if explicit != "" && explicit != IntentAuto {
		if _, ok := validIntents[explicit]; ok {
			return explicit, cand
		}
		// Invalid override falls through to auto routing.
	}

	q := strings.ToLower(in.Question)
	switch {
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

	if len(cand.identifiers) > 0 && looksLikeBareIdentifierQuery(in.Question) {
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

func extractIdentifiers(q string) []string {
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
// single-token fallback in extractIdentifiers to avoid passing
// punctuation/whitespace to search_symbol.
func isAllIdentChars(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isIdentChar(s[i]) {
			return false
		}
	}
	return true
}
