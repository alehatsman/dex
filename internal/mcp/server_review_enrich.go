package mcp

import (
	"context"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/review"
	"github.com/alehatsman/dex/internal/store"
)

// traceResult is the cached caller lane for one symbol.
type traceResult struct {
	callers []CallSite
	count   int
	noGraph bool
}

// collectCallersBySymbol hoists caller bodies out of the hunks into a single map
// keyed by symbol name (#136). It walks the EMITTED files/hunks (so compacted and
// truncated-away symbols are excluded) and pulls each referenced symbol's cached
// callers once. Returns nil when no touched symbol has callers, so the field is
// omitted from JSON.
func collectCallersBySymbol(files []ReviewFile, cache map[string]traceResult) map[string][]CallSite {
	out := map[string][]CallSite{}
	for _, f := range files {
		for _, h := range f.Hunks {
			for _, sym := range h.SymbolsTouched {
				if _, done := out[sym.Name]; done {
					continue
				}
				if tr, ok := cache[sym.Name]; ok && len(tr.callers) > 0 {
					out[sym.Name] = tr.callers
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// reviewFile composes one file's hunks. File-level history legs (tests, doc,
// blame, churn, author) run once; symbol legs run per hunk against the caches.
// newRef, when non-empty, triggers time-travel: hunk→symbol mapping resolves
// against the historical file content at that ref instead of the live index.
func (s *Server) reviewFile(ctx context.Context, st *store.Store, e *retrieve.Enricher, root string,
	fd review.FileDiff, k int, hunkBudget *int,
	callerCache map[string]traceResult, noteCache map[string][]LocatedFact,
	newRef string) ReviewFile {

	rf := ReviewFile{Path: fd.Path, OldPath: fd.OldPath, Status: fd.Status}

	// File-level history — pure filesystem / git, best-effort.
	rf.Tests = e.PairSiblingTests(fd.Path)
	rf.NearestDoc = e.FindNearestDoc(fd.Path)
	meta := map[string]*retrieve.PathMeta{}
	e.EnrichBlame(ctx, []string{fd.Path}, meta)
	if m := meta[fd.Path]; m != nil {
		rf.LastCommit = m.LastCommit
		rf.LastAuthor = m.LastAuthor
	}
	rf.Churn30d = gitChurnCount(ctx, root, fd.Path)
	rf.AuthorHistory = gitAuthorHistory(ctx, root, fd.Path)

	// Proactive gotcha-on-touch (#645/#649): notes whose scope binds this file's
	// path — surfaced because the PR touches it, even if no hunk symbol recalls
	// them. Best-effort.
	if scoped, err := st.KnowledgeByScope(ctx, fd.Path, k); err == nil {
		for _, f := range scoped {
			rf.ScopedNotes = append(rf.ScopedNotes, LocatedFact{
				ID: f.ID, Archetype: f.Archetype, Body: f.Body, Salience: f.Salience, Scope: f.Scope,
			})
		}
	}

	// A deleted file has no current symbols to resolve; emit its hunks with
	// diff + history only (still useful, per the cold-start contract).
	resolvable := fd.Status != "deleted"

	// Time-travel (#644): when newRef is set, pre-fetch the file content at that
	// ref and chunk it once so resolveHunkSymbols can map historical line numbers
	// without touching the live index. Best-effort: on error (new file in the
	// range, binary, or git failure) refChunks stays nil and falls back to ChunkAt.
	var refChunks []chunk.Chunk
	if resolvable && newRef != "" {
		if src, err := gitShowFile(ctx, root, newRef, fd.Path); err == nil {
			refChunks, _ = chunk.Chunks(ctx, fd.Path, src)
		}
	}

	// Track whether this file has any indexed symbols. After reviewMaxHunksNoCode
	// hunks with zero symbols we treat it as a data/non-code file and apply a
	// tighter per-file cap so a single large JSON or generated file can't consume
	// the entire hunkBudget and starve code files in the same diff.
	var fileHunksEmitted, fileSymbolsSeen int

	for _, h := range fd.Hunks {
		if *hunkBudget <= 0 {
			break
		}
		// Non-code file guard: after the probe window, if still no symbols, cap early.
		if fileSymbolsSeen == 0 && fileHunksEmitted >= reviewMaxHunksNoCode {
			break
		}
		*hunkBudget--
		fileHunksEmitted++

		rh := ReviewHunk{
			OldStart: h.OldStart, OldLines: h.OldLines,
			NewStart: h.NewStart, NewLines: h.NewLines, Heading: h.Heading,
		}
		var maxCallers int
		var exported, hadGraph bool
		if resolvable {
			syms := resolveHunkSymbols(ctx, st, fd.Path, h, refChunks)
			rh.SymbolsTouched = syms
			fileSymbolsSeen += len(syms)
			// No symbols at this hunk (comment/whitespace/data). Don't emit
			// "graph not indexed" — there's simply nothing to look up.
			if len(syms) == 0 {
				hadGraph = true
			}
			seenNote := map[int64]bool{}
			for i, sym := range syms {
				if sym.Exported {
					exported = true
				}
				tr := cachedCallers(ctx, s, root, sym.Name, k, callerCache)
				if !tr.noGraph {
					hadGraph = true
				}
				if tr.count > maxCallers {
					maxCallers = tr.count
				}
				// Caller BODIES are hoisted to out.CallersBySymbol (#136); the hunk
				// keeps only the per-symbol count so a reader sees how hot each
				// touched symbol is without the join.
				rh.SymbolsTouched[i].CallerCount = tr.count
				// Dedup notes by ID across symbols: two symbols in the same hunk
				// often share the same note (e.g. a gotcha scoped to the file),
				// which would produce duplicates per hunk with many probes (#701).
				for _, n := range cachedNotes(ctx, s, st, sym.Name, k, noteCache) {
					if !seenNote[n.ID] {
						seenNote[n.ID] = true
						rh.Notes = append(rh.Notes, n)
					}
				}
			}
		}
		rh.RiskTier, rh.RiskReason = hunkRisk(maxCallers, exported, hadGraph)
		rf.Hunks = append(rf.Hunks, rh)
	}
	return rf
}

// resolveHunkSymbols maps a hunk's new-side line range to the enclosing
// declarations via ChunkAt, deduped by name+line and capped.
// refChunks, when non-nil, overrides ChunkAt with an in-memory span lookup
// over historical file content (time-travel, #644).
func resolveHunkSymbols(ctx context.Context, st *store.Store, path string, h review.Hunk, refChunks []chunk.Chunk) []ReviewSymbol {
	seen := map[string]bool{}
	var out []ReviewSymbol
	// Probe at most reviewMaxProbes times, strided evenly across the hunk so a
	// large added file costs a bounded number of lookups.
	lines := h.TouchedLines()
	stride := 1
	if len(lines) > reviewMaxProbes {
		stride = (len(lines) + reviewMaxProbes - 1) / reviewMaxProbes
	}
	for i := 0; i < len(lines); i += stride {
		if len(out) >= reviewMaxSymHunk {
			break
		}
		line := lines[i]
		var name, kind string
		var startLine, endLine int
		if refChunks != nil {
			c, ok := chunkAtLine(refChunks, line)
			if !ok || c.Name == "" {
				continue
			}
			name, kind, startLine, endLine = c.Name, c.Kind, c.StartLine, c.EndLine
		} else {
			hit, err := st.ChunkAt(ctx, path, line)
			if err != nil || hit.Name == "" {
				continue
			}
			name, kind, startLine, endLine = hit.Name, hit.Kind, hit.StartLine, hit.EndLine
		}
		// Dedup by name only: a long function spans multiple indexed chunks
		// with different start_line values, but they're the same symbol.
		// Method names are qualified ((*Foo).Method), so different-receiver
		// methods with the same bare name won't collide (#700).
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, ReviewSymbol{
			Name: name, Kind: kind, Exported: isExportedName(retrieve.BareSymbolName(name)),
			StartLine: startLine, EndLine: endLine,
		})
	}
	return out
}

// cachedCallers returns the caller lane for a symbol, memoised across the review.
func cachedCallers(ctx context.Context, s *Server, root, symbol string, k int, cache map[string]traceResult) traceResult {
	if tr, ok := cache[symbol]; ok {
		return tr
	}
	_, tr, _ := traceVerb(ctx, s, nil, TraceInput{
		Symbol: symbol, Direction: "callers", K: k, ProjectRoot: root,
	})
	res := traceResult{}
	switch tr.Status {
	case "ok":
		res.callers = tr.Hits
		res.count = len(tr.Hits)
	case "no-graph":
		res.noGraph = true
	}
	cache[symbol] = res
	return res
}

// cachedNotes returns related notes for a symbol, memoised across the review.
func cachedNotes(ctx context.Context, s *Server, st *store.Store, symbol string, k int, cache map[string][]LocatedFact) []LocatedFact {
	if n, ok := cache[symbol]; ok {
		return n
	}
	var notes []LocatedFact
	if facts, err := s.recallFacts(ctx, st, symbol, k, false, "", true); err == nil {
		for _, f := range facts {
			notes = append(notes, LocatedFact{ID: f.ID, Archetype: f.Archetype, Body: f.Body, Salience: f.Salience})
		}
	}
	cache[symbol] = notes
	return notes
}
