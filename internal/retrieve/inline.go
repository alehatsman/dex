package retrieve

import (
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/source"
)

// NoiseFloorScore is the per-hit cutoff applied to semantic_hits
// inlining when the top score is already below LowConfidenceScore.
// On a genuine no-signal query (gibberish, very rare phrase) the whole
// pool tends to cluster in the 0.35-0.40 band; inlining all of them
// burns the byte budget on hits the agent will rightly ignore. The
// path+range still ships, just without Content, so the caller can
// follow up with a manual Read if a low-score path turns out to be
// relevant after all.
const NoiseFloorScore = 0.40

// LowConfidenceScore is the cosine-fused top-score threshold below
// which we treat semantic results as noise rather than signal. Picked
// empirically: real matches on this index cluster ≥0.5; nonsense
// queries ("frobnicate the quux gizmo") tend to score ≤0.4 on whatever
// chunk happens to share a token.
const LowConfidenceScore = 0.45

// ─── inline file contents into suggested_reads ────────────────────────────

// InlineCaps are the per-intent budgets for InlineContent. Exploration
// intents (architecture, package_topology) get a denser bundle than
// targeted ones — the caller is forming a mental model, so giving them
// more files / longer slices saves multiple round-trips vs. saving a
// few KB of response.
type InlineCaps struct {
	MaxLinesPerRead int
	MaxBytesPerRead int
	TotalBytesCap   int
}

// InlineCapsFor returns the byte/line budget for an intent. Thin shim over the
// #95d evidence policy (capsDense for architecture/package_topology/assemble,
// capsTargeted otherwise — see policy.go); kept as a function so external call
// sites don't change. The dense budget is an exploration bundle (assemble, #687,
// wants a usable working set in one shot); targeted was bumped 12KB→20KB on
// 2026-05-20 to stop semantic_hits truncating and forcing follow-up Reads.
func InlineCapsFor(intent string) InlineCaps {
	return PolicyFor(intent).InlineCaps
}

// InlineContent fills the Content/Truncated fields on suggested_reads,
// the Content fields on semantic_hits, and (for symbol_lookup intent)
// the Body field on symbols — all from a single per-intent byte
// budget so the caller gets a usable bundle without follow-up Reads.
// Fill order: suggested_reads → symbols → semantic_hits. The first
// two are the curated cut; semantic_hits use the remaining budget.
// A small read cache means a range that appears in multiple lanes
// is loaded once and charged once.
//
// Bounds are enforced at two levels: per-read (lines + bytes) and
// total bytes across all three arrays. Caps scale with intent (see
// InlineCapsFor). Failures (missing file, unreadable, scanner error)
// leave Content/Body empty and the caller still has Path/StartLine
// /EndLine to fall back on a manual Read.
//
// isTest classifies a path as test source — injected by the transport,
// which owns path classification.
func InlineContent(projectRoot, intent string, reads []SuggestedRead, syms []SymbolHit, sem []SemHit, isTest func(string) bool) {
	InlineContentKeyed(projectRoot, intent, reads, syms, sem, nil, isTest, nil)
}

// InlineContentKeyed is InlineContent with the query keywords the assemble
// intent (#687) needs for submodular symbol selection. keywords is ignored for
// every other intent. isNonImpl (also assemble-only) demotes non-implementation
// symbols within the coverage ordering. InlineContent is the keyword-free shim
// for callers that don't assemble.
func InlineContentKeyed(projectRoot, intent string, reads []SuggestedRead, syms []SymbolHit, sem []SemHit, keywords []string, isTest func(string) bool, isNonImpl func(string) bool) {
	in := &inliner{
		projectRoot: projectRoot,
		intent:      intent,
		caps:        InlineCapsFor(intent),
		cache:       map[inlineKey]inlineCached{},
		isTest:      isTest,
	}
	in.budget = in.caps.TotalBytesCap
	in.fillReads(reads)
	in.fillImports(reads)
	switch PolicyFor(intent).BodyFill {
	case BodyFillSymbols:
		in.fillSymbolBodies(syms)
	case BodyFillCoverage:
		// Inline symbol bodies in submodular keyword-coverage order: the
		// remaining byte budget then naturally selects the non-redundant subset
		// that covers the most of the query per byte (#687).
		in.fillSymbolBodiesOrdered(syms, coverageOrder(syms, keywords, isNonImpl))
	}
	in.fillSemanticHits(sem)
}

// coverageOrder ranks symbol indices by submodular max-coverage of the query
// keywords over each symbol's name + signature. Cost is the symbol's line span
// (a fetch-free proxy for body size); when isNonImpl reports a symbol's path as
// non-implementation (test/fixture, docs, build config) its cost is inflated so
// a real implementation with equal coverage is picked first and leads the
// assembled set (#723). With no keywords it falls back to natural order so
// assemble still inlines bodies. Budget 0 = order all covering symbols; the
// byte cap downstream does the real cut.
func coverageOrder(syms []SymbolHit, keywords []string, isNonImpl func(string) bool) []int {
	if len(keywords) == 0 {
		order := make([]int, len(syms))
		for i := range syms {
			order[i] = i
		}
		return order
	}
	lkw := make([]string, len(keywords))
	for i, k := range keywords {
		lkw[i] = strings.ToLower(k)
	}
	items := make([]Coverable, len(syms))
	for i := range syms {
		hay := strings.ToLower(syms[i].QualifiedName + " " + syms[i].Name + " " + syms[i].Signature)
		var keys []string
		for _, k := range lkw {
			if k != "" && strings.Contains(hay, k) {
				keys = append(keys, k)
			}
		}
		cost := syms[i].EndLine - syms[i].StartLine + 1
		if cost < 1 {
			cost = 1
		}
		if isNonImpl != nil && isNonImpl(syms[i].Path) {
			// Demote non-implementation symbols (tests/fixtures, docs,
			// build config) so a real implementation with equal coverage
			// is picked first.
			cost *= 4
		}
		items[i] = Coverable{Keys: keys, Cost: cost}
	}
	return SelectMaxCoverage(items, 0)
}

// inliner carries the shared budget + read cache across InlineContent's
// four passes (suggested_reads bodies, imports, symbol bodies, semantic
// hits). Splitting the passes off the entrypoint keeps each one small
// enough to read without paging — the entrypoint itself is now a thin
// orchestrator.
type inliner struct {
	projectRoot string
	intent      string
	caps        InlineCaps
	budget      int
	cache       map[inlineKey]inlineCached
	isTest      func(string) bool
}

type inlineKey struct {
	path           string
	start, end     int
	maxLines, maxB int
}

type inlineCached struct {
	content   string
	truncated bool
}

// fetch returns (content, truncated, charged). charged=true means the
// call drew from the budget (cache miss); the caller must decrement.
func (in *inliner) fetch(path string, start, end int) (string, bool, bool) {
	// Key on the per-read cap, NOT the running budget. The budget is
	// decremented between passes, so including it would give the same
	// (path,range) a different key on a later lane, miss the cache, and
	// re-read + re-charge the file — breaking the "loaded once, charged
	// once" guarantee. The read itself is still clamped to the remaining
	// budget below.
	k := inlineKey{path, start, end, in.caps.MaxLinesPerRead, in.caps.MaxBytesPerRead}
	if c, ok := in.cache[k]; ok {
		return c.content, c.truncated, false
	}
	if in.budget <= 0 {
		return "", false, false
	}
	perBytes := min(in.caps.MaxBytesPerRead, in.budget)
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(in.projectRoot, abs)
	}
	content, truncated, err := source.ReadLineRange(abs, start, end, in.caps.MaxLinesPerRead, perBytes)
	if err != nil {
		return "", false, false
	}
	in.cache[k] = inlineCached{content, truncated}
	return content, truncated, true
}

// chargePrePopulated debits the budget for content that was already
// inlined upstream (summary kinds), exactly once across all lanes. A
// summary SemHit promoted into suggested_reads carries the same Content
// in both lanes; charging it in fillReads AND fillSemanticHits would
// double-debit the budget and shrink it below TotalBytesCap. Keying the
// charge the same way fetch keys disk reads collapses the two into one.
func (in *inliner) chargePrePopulated(path string, start, end int, content string, truncated bool) {
	k := inlineKey{path, start, end, in.caps.MaxLinesPerRead, in.caps.MaxBytesPerRead}
	if _, seen := in.cache[k]; seen {
		return
	}
	in.cache[k] = inlineCached{content, truncated}
	in.budget -= len(content)
}

// fillReads populates Content/Truncated on every SuggestedRead.
// Entries that already carry Content (summary kinds) skip disk I/O
// but still charge the budget so the total response size stays bounded.
func (in *inliner) fillReads(reads []SuggestedRead) {
	for i := range reads {
		if in.budget <= 0 {
			return
		}
		if reads[i].Content != "" {
			in.chargePrePopulated(reads[i].Path, reads[i].StartLine, reads[i].EndLine, reads[i].Content, reads[i].Truncated)
			continue
		}
		content, truncated, charged := in.fetch(reads[i].Path, reads[i].StartLine, reads[i].EndLine)
		if content == "" && !truncated {
			continue
		}
		reads[i].Content = content
		reads[i].Truncated = truncated
		if charged {
			in.budget -= len(content)
		}
	}
}

// fillImports adds the import block to the first SuggestedRead per
// file. Skips reads starting near the top (already covered by their
// own Content) and files with no recognised extractor. Cheap per byte
// (<500 B typical), high agent value: surfaces dependencies without a
// follow-up Read.
func (in *inliner) fillImports(reads []SuggestedRead) {
	const maxImportsBytes = 1536
	importsDone := make(map[string]bool, len(reads))
	for i := range reads {
		if in.budget <= 0 {
			return
		}
		p := reads[i].Path
		if importsDone[p] {
			continue
		}
		importsDone[p] = true
		if reads[i].StartLine > 0 && reads[i].StartLine <= 5 {
			continue // top-of-file read already includes the imports
		}
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(in.projectRoot, abs)
		}
		imps := source.ExtractImports(abs)
		if imps == "" {
			continue
		}
		if len(imps) > maxImportsBytes {
			imps = imps[:maxImportsBytes] + "\n// … imports truncated"
		}
		reads[i].Imports = imps
		in.budget -= len(imps)
	}
}

// fillSymbolBodies fills Body on every matched symbol. Only invoked
// for IntentSymbolLookup: "what does X do" is the canonical case where
// the agent reads the body next, so inlining eliminates an otherwise
// certain follow-up Read.
func (in *inliner) fillSymbolBodies(syms []SymbolHit) {
	order := make([]int, len(syms))
	for i := range syms {
		order[i] = i
	}
	in.fillSymbolBodiesOrdered(syms, order)
}

// fillSymbolBodiesOrdered fills Body on symbols in the given index order,
// stopping when the shared budget is spent. The order is natural rank for
// symbol_lookup and submodular coverage order for assemble (#687); the
// per-symbol fetch and budget accounting are identical either way.
func (in *inliner) fillSymbolBodiesOrdered(syms []SymbolHit, order []int) {
	for _, idx := range order {
		if in.budget <= 0 {
			return
		}
		if idx < 0 || idx >= len(syms) {
			continue
		}
		s := &syms[idx]
		if s.Path == "" || s.StartLine <= 0 || s.EndLine < s.StartLine {
			continue
		}
		content, truncated, charged := in.fetch(s.Path, s.StartLine, s.EndLine)
		if content == "" && !truncated {
			continue
		}
		s.Body = content
		s.Truncated = truncated
		if charged {
			in.budget -= len(content)
		}
	}
}

// fillSemanticHits fills Content on each semantic hit. Skips three
// classes that would burn budget for no agent value:
//   - low-score hits when the top score is below the confidence
//     threshold (whole pool is likely noise)
//   - raw test source for non-editing intents (displaces real code
//     from the shared pool at ~4 KB per hit)
//   - hits with pre-populated Content (summary kinds); charges the
//     budget but skips refetch.
func (in *inliner) fillSemanticHits(sem []SemHit) {
	// Use the max score across the pool, not sem[0]: semantic_hits are
	// not strictly score-sorted (summary merging and rerank reordering
	// permute them), so [0] can be a low-score entry — using it would
	// spuriously trip, or fail to trip, the weak-match suppression. This
	// matches the decision buildNextAction already makes via the pool max.
	topScore := maxScore(sem)
	suppressLowScore := topScore > 0 && topScore < LowConfidenceScore
	for i := range sem {
		if in.budget <= 0 {
			return
		}
		if sem[i].Content != "" {
			in.chargePrePopulated(sem[i].Path, sem[i].StartLine, sem[i].EndLine, sem[i].Content, false)
			continue
		}
		if suppressLowScore && sem[i].Score < NoiseFloorScore {
			continue
		}
		if in.intent != IntentEditingContext && in.isTest(sem[i].Path) {
			continue
		}
		content, truncated, charged := in.fetch(sem[i].Path, sem[i].StartLine, sem[i].EndLine)
		if content == "" && !truncated {
			continue
		}
		sem[i].Content = content
		sem[i].Truncated = truncated
		if charged {
			in.budget -= len(content)
		}
	}
}

// maxScore returns the highest Score across the pool. Used by the
// weak-match suppression in fillSemanticHits; the pool is not strictly
// score-sorted, so a scan beats indexing [0].
func maxScore(sem []SemHit) float32 {
	var top float32
	for i := range sem {
		if sem[i].Score > top {
			top = sem[i].Score
		}
	}
	return top
}

// CountInlinedBytes sums len(Content) / len(Body) across the three
// output lanes. Imports are excluded — they are accounted against the
// budget at fill time but not reported here, matching the prior
// transport-side accounting.
func CountInlinedBytes(reads []SuggestedRead, syms []SymbolHit, sem []SemHit) int {
	n := 0
	for i := range reads {
		n += len(reads[i].Content)
	}
	for i := range syms {
		n += len(syms[i].Body)
	}
	for i := range sem {
		n += len(sem[i].Content)
	}
	return n
}
