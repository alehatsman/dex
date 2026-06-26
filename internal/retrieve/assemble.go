package retrieve

import "strings"

// AssembleKeywords builds the submodular coverage keys for an assemble
// query (#723, lever B). ExtractIdentifiers only yields code-shaped tokens,
// so a prose or multi-concern question ("how does pruning interact with CCR
// recovery") arrived with few or no keys and SelectMaxCoverage degraded to
// natural order — the symptom that made assemble return specs and CLI
// plumbing instead of the implementation set.
//
// It unions three sources, all lowercased to match coverageOrder's hay
// (QualifiedName + Name + Signature, lowercased):
//
//   - the detected identifiers, split into sub-word stems so a qualified
//     form like "(*Store).Search" yields clean keys ["store","search"];
//   - sub-word stems of the anchor symbol names, so a symbol sharing a
//     stem with an anchor ("PruneIndex" → "prune") clusters into the set;
//   - stopword-stripped content words of the question (taskKeywords).
//
// Keys are deduped preserving that priority order. Tokens shorter than 3
// bytes are dropped — they Contains-hit too much hay to discriminate.
func AssembleKeywords(identifiers []string, question string, anchorNames []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(identifiers)+len(anchorNames)+8)
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if len(s) < 3 {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, id := range identifiers {
		for _, tok := range splitIdentWords(id) {
			add(tok)
		}
	}
	for _, name := range anchorNames {
		for _, tok := range splitIdentWords(name) {
			add(tok)
		}
	}
	for _, w := range taskKeywords(question) {
		add(w)
	}
	return out
}

// splitIdentWords breaks an identifier or symbol name into lowercased
// sub-word stems, splitting on every non-alphanumeric run and on camelCase
// humps: "(*Store).parseConfig" → ["store","parse","config"],
// "prune_older_than" → ["prune","older","than"]. The caller applies the
// length floor.
func splitIdentWords(s string) []string {
	var out []string
	emit := func(chunk string) {
		runes := []rune(chunk)
		start := 0
		for i := 1; i < len(runes); i++ {
			prev, cur := runes[i-1], runes[i]
			if prev >= 'a' && prev <= 'z' && cur >= 'A' && cur <= 'Z' { // hump
				out = append(out, strings.ToLower(string(runes[start:i])))
				start = i
			}
		}
		out = append(out, strings.ToLower(string(runes[start:])))
	}
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			emit(b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}
