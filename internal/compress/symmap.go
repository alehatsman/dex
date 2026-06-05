package compress

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// identRe matches identifiers: starts with a letter, 6+ total chars,
// alphanumeric + underscore only.
var identRe = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9_]{5,}\b`)

// SymbolMap replaces high-ROI identifiers with short αN refs to reduce
// token count on verbose source files read in aggressive mode.
type SymbolMap struct {
	// entries sorted longest-first to prevent partial-match conflicts
	// ("handleRequestError" replaced before "handleRequest").
	entries []symEntry
}

type symEntry struct {
	ident string
	ref   string // α1, α2, …
}

// BuildSymbolMap scans content for identifiers that appear often enough
// to justify a codebook entry, applies the ROI gate, and returns a
// SymbolMap ready for Apply. Returns an empty (no-op) SymbolMap when
// savings would not justify a legend.
func BuildSymbolMap(content string) SymbolMap {
	counts := countIdentifiers(content)

	type candidate struct {
		ident       string
		occurrences int
	}
	var candidates []candidate

	nextID := 1
	for ident, n := range counts {
		if shouldRegisterSym(ident, n, nextID) {
			candidates = append(candidates, candidate{ident, n})
			nextID++
		}
	}

	if len(candidates) < 3 {
		// Not enough substitutions to justify emitting a legend.
		return SymbolMap{}
	}

	// Sort longest-first to avoid partial-match conflicts during Apply.
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i].ident) != len(candidates[j].ident) {
			return len(candidates[i].ident) > len(candidates[j].ident)
		}
		return candidates[i].ident < candidates[j].ident
	})

	entries := make([]symEntry, len(candidates))
	for i, c := range candidates {
		entries[i] = symEntry{ident: c.ident, ref: fmt.Sprintf("α%d", i+1)}
	}
	return SymbolMap{entries: entries}
}

// Empty returns true when the map has no entries (Apply is a no-op).
func (sm SymbolMap) Empty() bool { return len(sm.entries) == 0 }

// Legend returns the αMAP header to prepend to compressed output.
func (sm SymbolMap) Legend() string {
	if sm.Empty() {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("§MAP:")
	for _, e := range sm.entries {
		sb.WriteString("\n  ")
		sb.WriteString(e.ref)
		sb.WriteByte('=')
		sb.WriteString(e.ident)
	}
	sb.WriteByte('\n')
	return sb.String()
}

// Apply replaces each registered identifier in content with its αN ref.
// Entries are processed longest-first so longer identifiers win over
// their shorter prefixes.
func (sm SymbolMap) Apply(content string) string {
	if sm.Empty() {
		return content
	}
	for _, e := range sm.entries {
		content = strings.ReplaceAll(content, e.ident, e.ref)
	}
	return content
}

// ApplyWithLegend returns Legend() + "\n" + Apply(content), or content
// unchanged when the map is empty.
func (sm SymbolMap) ApplyWithLegend(content string) string {
	if sm.Empty() {
		return content
	}
	return sm.Legend() + "\n" + sm.Apply(content)
}

// countIdentifiers returns occurrence counts for all identifiers ≥6 chars
// found in content.
func countIdentifiers(content string) map[string]int {
	counts := make(map[string]int)
	for _, m := range identRe.FindAllString(content, -1) {
		counts[m]++
	}
	return counts
}

// symTokens estimates BPE token count for a string using rune count / 4
// (rounded up), minimum 1. Accurate enough for the ROI gate.
func symTokens(s string) int {
	n := utf8.RuneCountInString(s)
	t := (n + 3) / 4
	if t < 1 {
		return 1
	}
	return t
}

// shouldRegisterSym returns true when registering ident as αnextID saves
// more tokens across all its occurrences than the legend entry costs.
func shouldRegisterSym(ident string, occurrences, nextID int) bool {
	if len(ident) < 6 {
		return false
	}
	identToks := symTokens(ident)
	shortID := fmt.Sprintf("α%d", nextID)
	shortToks := symTokens(shortID)
	savingPer := identToks - shortToks
	if savingPer <= 0 {
		return false
	}
	totalSavings := occurrences * savingPer
	entryCost := identToks + shortToks + 2 // 2 = "  " prefix + "=" + "\n"
	return totalSavings > entryCost
}
