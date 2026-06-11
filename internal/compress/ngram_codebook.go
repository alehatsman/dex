package compress

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// NgramCodebook replaces recurring whitespace-token bigrams and trigrams with
// ©N refs, extending compression to sub-line repeated patterns not covered by
// SymbolMap (whole identifiers) or Codebook (whole lines).
//
// Applied after SymbolMap to avoid colliding with α/β refs, and to operate on
// already-reduced content. The ©N ref namespace is distinct from §N (Codebook)
// and αN (SymbolMap).
type NgramCodebook struct {
	entries []ngramEntry
}

type ngramEntry struct {
	pattern []string
	ref     string
	re      *regexp.Regexp
}

const ngramRefMax = 20

func ngramRef(i int) string { return fmt.Sprintf("©%d", i) }

// BuildNgramCodebook scans content for recurring bigrams and trigrams of
// whitespace-split tokens, applies a ROI gate (net token savings > 0), and
// returns a NgramCodebook. Trigrams are preferred over their constituent
// bigrams when both qualify. Returns an empty (no-op) codebook when fewer
// than 2 patterns qualify.
func BuildNgramCodebook(content string) NgramCodebook {
	bgFreq := make(map[string]int)
	tgFreq := make(map[string]int)

	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		n := len(fields)
		for i := 0; i+2 < n; i++ {
			tgFreq[fields[i]+"\x00"+fields[i+1]+"\x00"+fields[i+2]]++
		}
		for i := 0; i+1 < n; i++ {
			bgFreq[fields[i]+"\x00"+fields[i+1]]++
		}
	}

	// refToks: BPE cost of one ©N ref (e.g. "©0").
	refToks := symTokens("©0")

	type candidate struct {
		pattern []string
		net     int
	}
	var candidates []candidate

	// Track which token pairs are already covered by a qualifying trigram so
	// the constituent bigram is not double-counted.
	covered := make(map[string]bool)

	for key, count := range tgFreq {
		parts := strings.SplitN(key, "\x00", 3)
		patStr := strings.Join(parts, " ")
		patToks := symTokens(patStr)
		savingPer := patToks - refToks
		if savingPer <= 0 {
			continue
		}
		legendCost := patToks + refToks + 3
		net := count*savingPer - legendCost
		if net <= 0 {
			continue
		}
		candidates = append(candidates, candidate{parts, net})
		covered[parts[0]+"\x00"+parts[1]] = true
		covered[parts[1]+"\x00"+parts[2]] = true
	}

	for key, count := range bgFreq {
		if covered[key] {
			continue
		}
		parts := strings.SplitN(key, "\x00", 2)
		patStr := strings.Join(parts, " ")
		patToks := symTokens(patStr)
		savingPer := patToks - refToks
		if savingPer <= 0 {
			continue
		}
		legendCost := patToks + refToks + 3
		net := count*savingPer - legendCost
		if net <= 0 {
			continue
		}
		candidates = append(candidates, candidate{parts, net})
	}

	if len(candidates) < 2 {
		return NgramCodebook{}
	}

	// Sort by net savings descending; cap at ngramRefMax.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].net > candidates[j].net
	})
	if len(candidates) > ngramRefMax {
		candidates = candidates[:ngramRefMax]
	}

	// Sort longer patterns first so trigrams are applied before bigrams,
	// preventing partial-match replacement conflicts.
	sort.SliceStable(candidates, func(i, j int) bool {
		return len(candidates[i].pattern) > len(candidates[j].pattern)
	})

	entries := make([]ngramEntry, len(candidates))
	for i, c := range candidates {
		quoted := make([]string, len(c.pattern))
		for j, tok := range c.pattern {
			quoted[j] = regexp.QuoteMeta(tok)
		}
		entries[i] = ngramEntry{
			pattern: c.pattern,
			ref:     ngramRef(i),
			re:      regexp.MustCompile(strings.Join(quoted, `\s+`)),
		}
	}
	return NgramCodebook{entries: entries}
}

// Empty returns true when the codebook has no entries (Apply is a no-op).
func (cb NgramCodebook) Empty() bool { return len(cb.entries) == 0 }

// excludeAnchors returns a copy of cb without entries whose pattern overlaps an
// anchor span. An n-gram replaces a whitespace-separated token sequence with a
// ©N ref; if that sequence contains an anchor token, applying it would delete
// the anchor — so the entry is dropped under a strict target_model.
func (cb NgramCodebook) excludeAnchors(a AnchorSet) NgramCodebook {
	if a.Empty() || cb.Empty() {
		return cb
	}
	kept := make([]ngramEntry, 0, len(cb.entries))
	for _, e := range cb.entries {
		if !a.blocksText(strings.Join(e.pattern, " ")) {
			kept = append(kept, e)
		}
	}
	return NgramCodebook{entries: kept}
}

// Legend returns the ©MAP header to prepend to compressed output.
func (cb NgramCodebook) Legend() string {
	if cb.Empty() {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("©MAP:")
	for _, e := range cb.entries {
		sb.WriteString("\n  ")
		sb.WriteString(e.ref)
		sb.WriteByte('=')
		sb.WriteString(strings.Join(e.pattern, " "))
	}
	sb.WriteByte('\n')
	return sb.String()
}

// Apply replaces each registered n-gram pattern in content with its ©N ref.
// Entries are processed longer-pattern-first.
func (cb NgramCodebook) Apply(content string) string {
	if cb.Empty() {
		return content
	}
	for _, e := range cb.entries {
		content = e.re.ReplaceAllString(content, e.ref)
	}
	return content
}

// ApplyWithLegend returns Legend() + "\n" + Apply(content), or content when empty.
func (cb NgramCodebook) ApplyWithLegend(content string) string {
	if cb.Empty() {
		return content
	}
	return cb.Legend() + "\n" + cb.Apply(content)
}
