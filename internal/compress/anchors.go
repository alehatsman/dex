package compress

import (
	"regexp"
	"sort"
	"strings"
)

// AnchorSet is the set of tokens a strict (weak target_model) compression pass
// must leave byte-identical in its output — the hard floor that #163's adaptive
// ratio can never override. Weak local models (7-14B) substitute plausible-WRONG
// tokens when an exact path, qualified identifier, type name, or line number is
// mutated, hallucinating APIs and paths far worse than a frontier model. Those
// tokens are therefore never lossy under any aggressiveness level.
//
// Anchor kinds:
//   - file paths, optionally with a :line suffix  (pkg/file.go, pkg/file.go:42)
//   - fully-qualified identifiers                 (pkg.Symbol, std::sync::Arc)
//   - PascalCase type names                       (SymbolMap, HashMap)
type AnchorSet struct {
	// anchors is deduped and sorted longest-first so overlap checks prefer the
	// most specific match (std::sync::Arc before a bare substring of it).
	anchors []string
}

var (
	// reAnchorPath matches a multi-segment file-path-like token
	// (dir/.../file.ext) with an optional :line suffix. The full path —
	// including every leading segment — must be one anchor so no pass mutates a
	// prefix (e.g. symmap rewriting the "internal" segment of internal/x/y.go).
	reAnchorPath = regexp.MustCompile(`\b\w[\w.-]*(?:/[\w.-]+)+\.\w+(?::\d+)?\b`)
	// reAnchorQualified matches a dotted or scope-resolved identifier chain:
	// pkg.Symbol, a.b.C, std::sync::Arc.
	reAnchorQualified = regexp.MustCompile(`\b[A-Za-z_]\w*(?:(?:\.|::)[A-Za-z_]\w*)+\b`)
	// reAnchorType matches a multi-segment PascalCase identifier — the strong
	// type-name signal (SymbolMap, HashMap, AnchorSet). Single-segment caps
	// (Foo, User) are intentionally excluded so compression is not gutted.
	reAnchorType = regexp.MustCompile(`\b[A-Z][a-z0-9]+(?:[A-Z][a-z0-9]*)+\b`)
)

// ExtractAnchors builds the AnchorSet for content. Callers that strip comments
// first (the strict pipeline does) get anchors scoped to retained code — a path
// living only in a stripped comment is not a code anchor.
func ExtractAnchors(content string) AnchorSet {
	seen := make(map[string]struct{})
	add := func(matches []string) {
		for _, m := range matches {
			if len(m) >= 3 {
				seen[m] = struct{}{}
			}
		}
	}
	add(reAnchorPath.FindAllString(content, -1))
	add(reAnchorQualified.FindAllString(content, -1))
	add(reAnchorType.FindAllString(content, -1))

	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return AnchorSet{anchors: out}
}

// Anchors returns the anchor tokens (for tests and reporting).
func (a AnchorSet) Anchors() []string { return a.anchors }

// Empty reports whether the set has no anchors (strict passes are no-ops).
func (a AnchorSet) Empty() bool { return len(a.anchors) == 0 }

// blocksToken reports whether replacing tok would corrupt an anchor: tok is
// itself an anchor, or tok is a substring of some anchor (so swapping it for a
// ref would mutate that anchor in place).
func (a AnchorSet) blocksToken(tok string) bool {
	for _, anc := range a.anchors {
		if tok == anc || strings.Contains(anc, tok) {
			return true
		}
	}
	return false
}

// blocksText reports whether substituting s would overlap an anchor in either
// direction: s contains an anchor, or an anchor contains s. Used to keep
// multi-token rules and n-gram patterns off anchor spans.
func (a AnchorSet) blocksText(s string) bool {
	for _, anc := range a.anchors {
		if strings.Contains(s, anc) || strings.Contains(anc, s) {
			return true
		}
	}
	return false
}

// lineHasAnchor reports whether line contains any anchor — such lines are never
// dropped by the strict entropy pass.
func (a AnchorSet) lineHasAnchor(line string) bool {
	for _, anc := range a.anchors {
		if strings.Contains(line, anc) {
			return true
		}
	}
	return false
}

// Missing returns the anchors absent from out — empty means the verbatim
// guarantee held. The strict pipeline holds this invariant by construction;
// Missing is the assertion tests check across aggressiveness levels.
func (a AnchorSet) Missing(out string) []string {
	var miss []string
	for _, anc := range a.anchors {
		if !strings.Contains(out, anc) {
			miss = append(miss, anc)
		}
	}
	return miss
}

// AggressiveCompressStrict runs the same pipeline as AggressiveCompress but
// guarantees every anchor token is byte-identical in the output. Used for weak
// target_model profiles (see Profile.StrictAnchors). The four anchor-mutating
// passes — entropy line-drop, token reductions, symbol map, n-gram codebook —
// are each held off anchor spans, so the floor holds by construction.
func AggressiveCompressStrict(content, ext string) string {
	stripped := stripComments(strings.Split(content, "\n"), ext)
	anchors := ExtractAnchors(strings.Join(stripped, "\n"))
	if anchors.Empty() {
		return AggressiveCompress(content, ext)
	}

	lines := collapseClosingBracesAggressively(stripped)
	if len(lines) > 200 {
		lines = halveIndentation(lines)
	}
	lines = normalizeBlankLines(lines)
	thresh := thresholdsFor(ext)
	lines = dropLowEntropyLinesStrict(lines, thresh.entropyFilterThreshold(), anchors)
	compressed := applyTokenReductionsExcept(strings.Join(lines, "\n"), ext, anchors)
	compressed = SafeguardRatio(content, compressed)
	sm := BuildSymbolMap(compressed).excludeAnchors(anchors)
	compressed = sm.ApplyWithLegend(compressed)
	ncb := BuildNgramCodebook(compressed).excludeAnchors(anchors)
	return ncb.ApplyWithLegend(compressed)
}

// CompressCode runs aggressive code compression. When strict is true (a weak
// target_model profile), anchor tokens are guaranteed byte-identical; otherwise
// the relaxed pipeline runs unchanged.
func CompressCode(content, ext string, strict bool) string {
	if strict {
		return AggressiveCompressStrict(content, ext)
	}
	return AggressiveCompress(content, ext)
}
