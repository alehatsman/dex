package retrieve

import (
	"slices"

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
//   - symbol_lookup, callers, callees, assemble (with symbol hits):
//     prefer symbol-lane definition sites; one read per definition.
//     For assemble, exact name matches anchor the working set and
//     prevent unrelated semantic hits (naming collisions) from
//     misdirecting suggested_reads (#699).
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
	// Exploration intents (architecture/package_topology) take a denser bundle
	// — the caller is forming a mental model, so more files pays off more than a
	// slim one (see the #95d evidence policy in policy.go / InlineCapsFor).
	maxReads := PolicyFor(intent).MaxReads

	seen := map[string]struct{}{}
	out := make([]SuggestedRead, 0, maxReads)

	// Pass 1: symbol definitions for symbol-driven intents and assemble.
	// For assemble, exact symbol matches anchor the working set; semantic hits
	// often collide with unrelated namespaces (e.g. "risk tier" pulling eval
	// files in a codebase where both review and eval use "tier"), so symbol
	// paths must lead suggested_reads when the symbol lane has hits.
	if intent == IntentSymbolLookup || intent == IntentCallers || intent == IntentCallees ||
		(intent == IntentAssemble && len(symbols) > 0) {
		for _, sym := range symbols {
			if sym.Path == "" {
				continue
			}
			if _, ok := seen[sym.Path]; ok {
				continue
			}
			seen[sym.Path] = struct{}{}
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
	slices.SortStableFunc(rs, func(a, b ranked) int {
		if a.crossLane != b.crossLane {
			if a.crossLane {
				return -1 // cross-lane agreement first
			}
			return 1
		}
		if preferCode && a.nonImpl != b.nonImpl {
			if !a.nonImpl {
				return -1 // implementation beats doc/build
			}
			return 1
		}
		if usesCentrality {
			// scoreBucket groups close cosines so PageRank can flip the
			// order. 0.05 wide: 0.55 and 0.59 share bucket 11; 0.55 and
			// 0.60 split into 11/12 and the higher score wins outright.
			bi, bj := int(a.hit.Score*20), int(b.hit.Score*20)
			if bi != bj {
				if bi > bj {
					return -1
				}
				return 1
			}
			if a.pageRank != b.pageRank {
				if a.pageRank > b.pageRank {
					return -1
				}
				return 1
			}
		}
		if a.hit.Score > b.hit.Score {
			return -1
		}
		if a.hit.Score < b.hit.Score {
			return 1
		}
		return 0
	})
	for _, r := range rs {
		if _, ok := seen[r.hit.Path]; ok {
			continue
		}
		seen[r.hit.Path] = struct{}{}
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
