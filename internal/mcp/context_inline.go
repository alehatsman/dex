package mcp

import (
	"path/filepath"

	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/source"
)

// ─── inline file contents into suggested_reads ────────────────────────────

// inlineCaps are the per-intent budgets for inlineSuggestedReads.
// Exploration intents (architecture, package_topology) get a denser
// bundle than targeted ones — the caller is forming a mental model,
// so giving them more files / longer slices saves multiple round-trips
// vs. saving a few KB of response.
type inlineCaps struct {
	maxLinesPerRead int
	maxBytesPerRead int
	totalBytesCap   int
}

func inlineCapsFor(intent string) inlineCaps {
	switch intent {
	case retrieve.IntentArchitecture, retrieve.IntentPackageTopology:
		return inlineCaps{maxLinesPerRead: 120, maxBytesPerRead: 8 * 1024, totalBytesCap: 40 * 1024}
	default:
		// Targeted intents (behavior_search / symbol_lookup / callers /
		// callees / editing_context). Bumped from 12 KB → 20 KB on
		// 2026-05-20: the smaller budget often forced semantic_hits to
		// truncate, pushing the agent toward follow-up Reads. 20 KB
		// covers ~10 chunk-sized hits with their content intact while
		// still being a tight bundle.
		return inlineCaps{maxLinesPerRead: 60, maxBytesPerRead: 4 * 1024, totalBytesCap: 20 * 1024}
	}
}

// inlineContent fills the Content/Truncated fields on suggested_reads,
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
// inlineCapsFor). Failures (missing file, unreadable, scanner error)
// leave Content/Body empty and the caller still has Path/StartLine
// /EndLine to fall back on a manual Read.
func inlineContent(projectRoot, intent string, reads []SuggestedRead, syms []SymbolHit, sem []SemHit) {
	in := &inliner{
		projectRoot: projectRoot,
		intent:      intent,
		caps:        inlineCapsFor(intent),
		cache:       map[inlineKey]inlineCached{},
	}
	in.budget = in.caps.totalBytesCap
	in.fillReads(reads)
	in.fillImports(reads)
	if intent == retrieve.IntentSymbolLookup {
		in.fillSymbolBodies(syms)
	}
	in.fillSemanticHits(sem)
}

// inliner carries the shared budget + read cache across inlineContent's
// four passes (suggested_reads bodies, imports, symbol bodies, semantic
// hits). Splitting the passes off the entrypoint keeps each one small
// enough to read without paging — the entrypoint itself is now a thin
// orchestrator.
type inliner struct {
	projectRoot string
	intent      string
	caps        inlineCaps
	budget      int
	cache       map[inlineKey]inlineCached
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
	k := inlineKey{path, start, end, in.caps.maxLinesPerRead, in.caps.maxBytesPerRead}
	if c, ok := in.cache[k]; ok {
		return c.content, c.truncated, false
	}
	if in.budget <= 0 {
		return "", false, false
	}
	perBytes := min(in.caps.maxBytesPerRead, in.budget)
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(in.projectRoot, abs)
	}
	content, truncated, err := source.ReadLineRange(abs, start, end, in.caps.maxLinesPerRead, perBytes)
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
// double-debit the budget and shrink it below totalBytesCap. Keying the
// charge the same way fetch keys disk reads collapses the two into one.
func (in *inliner) chargePrePopulated(path string, start, end int, content string, truncated bool) {
	k := inlineKey{path, start, end, in.caps.maxLinesPerRead, in.caps.maxBytesPerRead}
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
// for retrieve.IntentSymbolLookup: "what does X do" is the canonical case
// where the agent reads the body next, so inlining eliminates an
// otherwise certain follow-up Read.
func (in *inliner) fillSymbolBodies(syms []SymbolHit) {
	for i := range syms {
		if in.budget <= 0 {
			return
		}
		s := &syms[i]
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
	// matches the decision buildNextAction already makes via maxSemanticScore.
	topScore := maxSemanticScore(sem)
	suppressLowScore := topScore > 0 && topScore < lowConfidenceScore
	for i := range sem {
		if in.budget <= 0 {
			return
		}
		if sem[i].Content != "" {
			in.chargePrePopulated(sem[i].Path, sem[i].StartLine, sem[i].EndLine, sem[i].Content, false)
			continue
		}
		if suppressLowScore && sem[i].Score < noiseFloorScore {
			continue
		}
		if in.intent != retrieve.IntentEditingContext && isTestPath(sem[i].Path) {
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
