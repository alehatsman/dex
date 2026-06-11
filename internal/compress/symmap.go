package compress

import (
	"regexp"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// identRe matches identifiers: starts with a letter, 6+ total chars,
// alphanumeric + underscore only.
var identRe = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9_]{5,}\b`)

// singleTokenRefs is a curated set of characters that each tokenize to exactly
// 1 BPE token in o200k_base (verified by TestRefCharTokenCosts). All are
// non-ASCII, so identRe (pure ASCII) can never match them — refs cannot collide
// with identifier text in Apply.
//
// Greek lowercase (18) + circled digits ①-⑤ (⑥+ cost 2 tokens) = 23 slots.
var singleTokenRefs = []string{
	"α", "β", "γ", "δ", "ε", "ζ", "η", "θ",
	"λ", "μ", "ξ", "π", "σ", "τ", "φ", "χ", "ψ", "ω",
	"①", "②", "③", "④", "⑤",
}

// SymbolMap replaces high-ROI identifiers with short single-token refs to
// reduce token count on verbose source files read in aggressive mode.
type SymbolMap struct {
	// entries sorted longest-first to prevent partial-match conflicts
	// ("handleRequestError" replaced before "handleRequest").
	entries []symEntry
}

type symEntry struct {
	ident string
	ref   string // single Unicode char from singleTokenRefs
}

// BuildSymbolMap scans content for identifiers that appear often enough to
// justify a codebook entry, applies the ROI gate, and returns a SymbolMap
// ready for Apply. Returns an empty (no-op) SymbolMap when savings would not
// justify a legend.
func BuildSymbolMap(content string) SymbolMap {
	counts := countIdentifiers(content)

	type candidate struct {
		ident       string
		occurrences int
		netSavings  int // totalSavings - entryCost, for cap selection
	}
	var candidates []candidate

	for ident, n := range counts {
		if net, ok := symROI(ident, n); ok {
			candidates = append(candidates, candidate{ident, n, net})
		}
	}

	if len(candidates) < 3 {
		// Not enough substitutions to justify emitting a legend.
		return SymbolMap{}
	}

	// Sort by net savings descending: when we hit the slot cap, the highest-ROI
	// identifiers keep their refs and lower-ROI ones are dropped.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].netSavings > candidates[j].netSavings
	})
	if len(candidates) > len(singleTokenRefs) {
		candidates = candidates[:len(singleTokenRefs)]
	}

	// Assign refs, then re-sort longest-ident-first for Apply correctness.
	entries := make([]symEntry, len(candidates))
	for i, c := range candidates {
		entries[i] = symEntry{ident: c.ident, ref: singleTokenRefs[i]}
	}
	sort.Slice(entries, func(i, j int) bool {
		if len(entries[i].ident) != len(entries[j].ident) {
			return len(entries[i].ident) > len(entries[j].ident)
		}
		return entries[i].ident < entries[j].ident
	})

	return SymbolMap{entries: entries}
}

// Empty returns true when the map has no entries (Apply is a no-op).
func (sm SymbolMap) Empty() bool { return len(sm.entries) == 0 }

// excludeAnchors returns a copy of sm without entries whose identifier is
// blocked by the anchor set — those identifiers (and any anchor they are a
// substring of) must survive verbatim under a strict target_model.
func (sm SymbolMap) excludeAnchors(a AnchorSet) SymbolMap {
	if a.Empty() || sm.Empty() {
		return sm
	}
	kept := make([]symEntry, 0, len(sm.entries))
	for _, e := range sm.entries {
		if !a.blocksToken(e.ident) {
			kept = append(kept, e)
		}
	}
	return SymbolMap{entries: kept}
}

// Legend returns the §MAP header to prepend to compressed output.
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

// Apply replaces each registered identifier in content with its ref.
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

// symTokens returns the real BPE token count for a string (default o200k_base),
// minimum 1.
func symTokens(s string) int {
	t := tokens.Count(s)
	if t < 1 {
		return 1
	}
	return t
}

// symROI returns (netSavings, true) when registering ident with a 1-token ref
// saves more tokens across all occurrences than the legend entry costs.
// All refs in singleTokenRefs cost exactly 1 token, so refToks is hard-coded.
func symROI(ident string, occurrences int) (int, bool) {
	if len(ident) < 6 {
		return 0, false
	}
	const refToks = 1
	identToks := symTokens(ident)
	savingPer := identToks - refToks
	if savingPer <= 0 {
		return 0, false
	}
	totalSavings := occurrences * savingPer
	entryCost := identToks + refToks + 2 // ident + ref + "  …=…\n" overhead
	net := totalSavings - entryCost
	return net, net > 0
}
