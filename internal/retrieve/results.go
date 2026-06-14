package retrieve

import (
	"sort"

	"github.com/alehatsman/dex/internal/graphquery"
)

// SuggestedRead is one file range the caller should open in full, in
// neutral (transport-free) form. The transport maps it to the wire
// SuggestedRead, stamping the handle and seen-turn dedup at the edge.
type SuggestedRead struct {
	Path      string
	StartLine int
	EndLine   int
	Reason    string
	// Content / Truncated / Imports are the inline overlay, populated by
	// InlineContent.
	Content   string
	Truncated bool
	Imports   string
}

// isReadableRange reports whether a semantic hit names a concrete file
// range (vs. a rollup like package_summary / repo_summary whose "path"
// is a directory with zero line bounds). Only concrete ranges belong in
// suggested_reads — rollups stay in semantic_hits as informational
// context but would produce bogus "lines 0-0" directives downstream.
func isReadableRange(h SemHit) bool {
	switch h.Kind {
	case "package_summary", "repo_summary":
		return false
	}
	return true
}

// PickSuggestedReads merges the top results from both lanes into a
// short, deduplicated list of file ranges the caller should open in
// full. Strategy by intent:
//
//   - symbol_lookup, callers, callees: prefer symbol-lane definition
//     sites; one read per definition.
//   - architecture, package_topology: top 2-3 semantic hits across
//     distinct files, widened to surrounding chunk extents. When `view`
//     is populated, PageRank breaks ties within 0.05-wide score buckets
//     so a structural hub (high in/out degree on the calls graph) wins
//     against a near-tied tuning doc.
//   - behavior_search, editing_context: top 2 semantic hits, prefer
//     paths that also appear in the symbol lane (cross-lane agreement
//     bumps confidence).
//
// isNonImpl classifies a path as non-implementation (docs, build/CI
// config) — injected by the transport, which owns path classification.
func PickSuggestedReads(intent string, semHits []SemHit, symbols []SymHit, symbolPaths map[string]struct{}, view *graphquery.View, isNonImpl func(string) bool) []SuggestedRead {
	maxReads := 2
	switch intent {
	case IntentArchitecture, IntentPackageTopology:
		// Exploration intents — the caller is forming a mental model,
		// so a denser bundle (more files, more lines, see
		// InlineCapsFor) pays off more than a slim one.
		maxReads = 5
	case IntentSymbolLookup, IntentCallers, IntentCallees:
		maxReads = 2
	}

	seen := map[string]bool{}
	out := make([]SuggestedRead, 0, maxReads)

	// Pass 1: symbol definitions for symbol-driven intents.
	if intent == IntentSymbolLookup || intent == IntentCallers || intent == IntentCallees {
		for _, sym := range symbols {
			if sym.Path == "" || seen[sym.Path] {
				continue
			}
			seen[sym.Path] = true
			out = append(out, SuggestedRead{
				Path:      sym.Path,
				StartLine: sym.StartLine,
				EndLine:   sym.EndLine,
				Reason:    "definition of " + sym.QualifiedName,
			})
			if len(out) >= maxReads {
				return out
			}
		}
	}

	// Pass 2: semantic hits, biased toward cross-lane agreement.
	// For code-oriented intents we also demote non-implementation paths
	// (docs and build/CI config) as a tiebreaker, so a README or
	// Taskfile.yml doesn't beat the .go file that implements the
	// feature when scores are close. Architecture and behavior_search
	// are exceptions — for the former the README often IS the right
	// read; for the latter a spec/behavior doc may be the best answer
	// (e.g. specs/watch.md for "how does watch work?").
	preferCode := intent != IntentArchitecture && intent != IntentBehaviorSearch
	// usesCentrality intents bucket their scores into 0.05-wide bins and
	// break ties by PageRank — so a structural hub beats a near-tied
	// non-hub. Limited to architecture/package_topology where "which
	// file holds the system together" matters more than a small cosine
	// edge. Other intents keep the strict score ordering.
	usesCentrality := intent == IntentArchitecture || intent == IntentPackageTopology
	type ranked struct {
		hit       SemHit
		crossLane bool
		nonImpl   bool
		pageRank  float64
	}
	rs := make([]ranked, 0, len(semHits))
	for _, h := range semHits {
		// Skip rollup hits (package_summary / repo_summary) — their
		// "path" is a directory and StartLine/EndLine are 0, so they
		// produce bogus "lines 0-0" directives downstream. They still
		// live in semantic_hits as informational context; they just
		// don't belong in suggested_reads.
		if !isReadableRange(h) {
			continue
		}
		_, cross := symbolPaths[h.Path]
		r := ranked{hit: h, crossLane: cross, nonImpl: isNonImpl(h.Path)}
		if usesCentrality {
			r.pageRank = graphquery.ChunkPageRank(view, h.Path, h.StartLine)
		}
		rs = append(rs, r)
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].crossLane != rs[j].crossLane {
			return rs[i].crossLane // cross-lane agreement first
		}
		if preferCode && rs[i].nonImpl != rs[j].nonImpl {
			return !rs[i].nonImpl // implementation beats doc/build
		}
		if usesCentrality {
			// scoreBucket groups close cosines so PageRank can flip the
			// order. 0.05 wide: 0.55 and 0.59 share bucket 11; 0.55 and
			// 0.60 split into 11/12 and the higher score wins outright.
			bi, bj := int(rs[i].hit.Score*20), int(rs[j].hit.Score*20)
			if bi != bj {
				return bi > bj
			}
			if rs[i].pageRank != rs[j].pageRank {
				return rs[i].pageRank > rs[j].pageRank
			}
		}
		return rs[i].hit.Score > rs[j].hit.Score
	})
	for _, r := range rs {
		if seen[r.hit.Path] {
			continue
		}
		seen[r.hit.Path] = true
		reason := "top semantic match"
		if r.crossLane {
			reason = "semantic match + symbol agreement"
		}
		out = append(out, SuggestedRead{
			Path:      r.hit.Path,
			StartLine: r.hit.StartLine,
			EndLine:   r.hit.EndLine,
			Reason:    reason,
			Content:   r.hit.Content,
		})
		if len(out) >= maxReads {
			return out
		}
	}
	return out
}
