package retrieve

import (
	"fmt"
	"strings"
)

// ─── next_action / avoid (prose) ──────────────────────────────────────────

// BuildNextAction returns an imperative sentence the agent can execute
// directly. The issue is explicit that prose outperforms structured
// args for agent compliance. Always concrete — names paths and line
// ranges — never "do more research."
//
// The "weak semantic" fallback fires only when the intent's *primary*
// payload is also empty. For graph-driven intents (package_topology /
// architecture) a populated graph counts as confidence even when the
// semantic-hit scores are low; for editing_context, populated blame
// annotations count likewise. This prevents the misleading
// "rephrase or grep" message on calls that actually returned useful
// structural data.
func BuildNextAction(intent string, reads []SuggestedRead, symbols []SymHit, topSemScore float32, graphEdgeCount, refCount int, hasBlame bool) string {
	if len(reads) == 0 && len(symbols) == 0 && graphEdgeCount == 0 {
		return "Rephrase the question with concrete keywords or fall back to grep."
	}
	// Confidence comes from any of: symbol hits, strong semantic score,
	// or an intent-specific structural payload.
	intentPayloadStrong := false
	switch intent {
	case IntentPackageTopology, IntentArchitecture:
		intentPayloadStrong = graphEdgeCount > 0
	case IntentEditingContext:
		intentPayloadStrong = hasBlame
	}
	weakSemantic := topSemScore > 0 && topSemScore < LowConfidenceScore
	if len(symbols) == 0 && weakSemantic && !intentPayloadStrong {
		return "Top semantic match is weak — rephrase with concrete keywords or fall back to grep."
	}
	switch intent {
	case IntentSymbolLookup:
		// Only claim "the definition" when a symbol actually matched —
		// reads[0] without symbols is a semantic neighbor, not the
		// definition the user asked about.
		if len(symbols) > 0 && len(reads) > 0 {
			// Multiple definitions across distinct paths is a real
			// shape for ambiguous names (`Options` exists in chat,
			// graph, index, store, watch packages). Signal that — singular
			// "the definition" hides matches the agent should know
			// about.
			if distinctSymbolPaths(symbols) > 1 {
				return fmt.Sprintf("%d definitions across files — closest is %s lines %d-%d; consult the full `symbols` array for the rest.",
					distinctSymbolPaths(symbols), reads[0].Path, reads[0].StartLine, reads[0].EndLine)
			}
			return fmt.Sprintf("Read %s lines %d-%d to see the definition.", reads[0].Path, reads[0].StartLine, reads[0].EndLine)
		}
		if len(symbols) == 0 && len(reads) > 0 {
			return fmt.Sprintf("No exact symbol match — the closest semantic neighbor is %s lines %d-%d. Verify there before assuming the identifier exists.",
				reads[0].Path, reads[0].StartLine, reads[0].EndLine)
		}
	case IntentCallers, IntentCallees:
		rel := "callers"
		if intent == IntentCallees {
			rel = "callees"
		}
		// Prefer the precise graph lane when it resolved calls edges.
		// Falls back to the BM25 chunk-search `references` list (populated
		// for non-Go languages where `calls` extraction isn't wired yet).
		if graphEdgeCount > 0 {
			noun := "edge"
			if graphEdgeCount != 1 {
				noun = "edges"
			}
			return fmt.Sprintf("Read the `graph.edges` list — it carries %d %s %s from the static graph; open each `to` node for its body.", graphEdgeCount, rel, noun)
		}
		if refCount > 0 {
			noun := "site"
			if refCount != 1 {
				noun = "sites"
			}
			return fmt.Sprintf("The `references` field lists %d call %s (BM25 chunk search for non-Go targets). Walk them before reaching for grep.", refCount, noun)
		}
		if len(symbols) > 0 {
			return fmt.Sprintf("No %s found via graph or refs — start from %s (%s) and confirm the symbol is actually used.",
				rel, symbols[0].Path, symbols[0].QualifiedName)
		}
	case IntentPackageTopology:
		if graphEdgeCount > 0 {
			return fmt.Sprintf("Read the `graph.edges` list (%d imports) to see package dependencies, then call with intent=symbol_lookup on a specific package to drill in.", graphEdgeCount)
		}
		if len(reads) > 0 {
			return readsSkimDirective(reads)
		}
	case IntentArchitecture:
		if len(reads) > 0 {
			return readsSkimDirective(reads)
		}
	case IntentEditingContext:
		if len(reads) > 0 {
			return fmt.Sprintf("Read %s lines %d-%d before editing — this is the primary site.", reads[0].Path, reads[0].StartLine, reads[0].EndLine)
		}
	}
	// behavior_search and fallback.
	if len(reads) > 0 {
		return fmt.Sprintf("Read %s lines %d-%d to ground your answer.", reads[0].Path, reads[0].StartLine, reads[0].EndLine)
	}
	if len(symbols) > 0 {
		return fmt.Sprintf("Inspect %s in %s.", symbols[0].QualifiedName, symbols[0].Path)
	}
	return ""
}

// distinctSymbolPaths counts the number of unique paths across a
// SymHit slice. Used by BuildNextAction to signal when a single
// identifier resolves to multiple definitions (e.g. `Options` exists
// in chat, graph, index, store, watch packages) so the agent reads
// the full symbols array rather than stopping at the first read.
func distinctSymbolPaths(syms []SymHit) int {
	seen := make(map[string]struct{}, len(syms))
	for _, s := range syms {
		if s.Path == "" {
			continue
		}
		seen[s.Path] = struct{}{}
	}
	return len(seen)
}

// readsSkimDirective renders the multi-file skim hint used by
// architecture / package_topology when the graph isn't the headline.
func readsSkimDirective(reads []SuggestedRead) string {
	parts := make([]string, 0, len(reads))
	for _, r := range reads {
		parts = append(parts, fmt.Sprintf("%s lines %d-%d", r.Path, r.StartLine, r.EndLine))
	}
	return "Skim " + strings.Join(parts, "; ") + " for the structural overview, then re-call with intent=symbol_lookup to drill into specific types, or intent=editing_context for files you want to modify."
}

// BuildAvoid emits a "what not to do" hint. Strong claims when we
// have strong signals (exact symbol found → don't grep); softer
// otherwise. `graphIndexed` is true when the project has a graph
// available. `hasRefs` softens the callers/callees message: when
// either calls-edges or BM25 chunk search populated `references`,
// the agent has the surface it needs, so the message shifts from
// "verify with grep" to "do not re-grep, the list is here."
func BuildAvoid(intent string, semHits []SemHit, symbols []SymHit, graphIndexed, hasRefs bool) string {
	if intent == IntentCallers || intent == IntentCallees {
		if hasRefs {
			return "Do not grep for the identifier — the `references` field already lists call sites. For Go this comes from the static graph; for other languages it's a BM25 chunk search over the bare symbol name (verify edge cases by reading the snippets)."
		}
		return "Do not trust the symbols list as exhaustive — Go `calls` edges are type-resolved, but Python/JS/TS/Rust/Java edges are name-based (tree-sitter) with incomplete recall. Verify with grep on the symbol name."
	}
	if !graphIndexed {
		return "Graph not indexed for this project — results from semantic + symbol lanes only. Run `dex index <project>` to refresh both layers (graph extraction is part of the default index run)."
	}
	// Exploration intents — the user is forming a mental model, so
	// the failure mode to discourage is breadth (enumerating files,
	// re-deriving the topology) rather than depth (reading whole files).
	switch intent {
	case IntentArchitecture:
		return "Do not enumerate the file tree — the graph nodes and suggested reads ARE the structural overview. Start there before broader exploration."
	case IntentPackageTopology:
		return "Do not infer imports by grepping — the graph edges encode them. Use the topology, don't rebuild it."
	}
	if len(symbols) > 0 && len(semHits) > 0 {
		return "Do not grep for the identifier; it is already located. Read the suggested ranges instead of opening whole files."
	}
	if len(symbols) > 0 {
		return "Do not grep for the identifier; it is already located."
	}
	if len(semHits) > 0 {
		return "Do not read entire files; the suggested ranges cover the relevant context."
	}
	return ""
}
