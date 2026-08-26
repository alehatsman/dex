package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/slo"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/throttle"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: search_semantic ────────────────────────────────────────────────

type SearchInput struct {
	Query       string   `json:"query" jsonschema:"natural-language or code query"`
	ProjectRoot string   `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
	K           int      `json:"k,omitempty" jsonschema:"number of results to return (default 8, max 30)"`
	Exclude     []string `json:"exclude,omitempty" jsonschema:"paths to skip: a plain entry is a directory prefix ('vendor/', 'internal/legacy/'); an entry with glob metacharacters (*?[) is matched as a glob against the full path and the basename ('*_test.go', 'testdata/**')"`
	Languages   []string `json:"languages,omitempty" jsonschema:"restrict results to these languages (e.g. ['go','typescript']); accepts language names or raw extensions (.rs, .go)"`
	PathGlob    string   `json:"path_glob,omitempty" jsonschema:"glob pattern matched against relative file path (e.g. 'internal/**', '**/test*')"`
}

type SearchHit struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	// SortScore is the authoritative key the hits are ordered by — compare
	// THIS across hits, not Score. It folds the full pipeline (local rerank,
	// cross-encoder, RRF fusion) into one monotonic value, so it is the only
	// field guaranteed non-increasing down the list. Score/bm25/rrf/rerank
	// below are per-lane diagnostics that need not be monotonic on their own.
	SortScore float32 `json:"sort_score"`
	// Score is the cosine similarity in [-1, 1]. Always populated. A
	// per-lane diagnostic — do NOT sort by it; use SortScore.
	Score float32 `json:"score"`
	// BM25Score is the lexical (FTS5) score when the hit surfaced
	// through the BM25 leg of hybrid search. Larger = better. Zero
	// for semantic-only hits.
	BM25Score float32 `json:"bm25_score,omitempty"`
	// RRFScore is the fused rank used for ordering when hybrid search
	// is active. Zero when search ran semantic-only.
	RRFScore float32 `json:"rrf_score,omitempty"`
	// RerankScore is the cross-encoder relevance score in [0, 1] when
	// rerank ran. Zero when no reranker was wired or it failed open.
	RerankScore float32 `json:"rerank_score,omitempty"`
	// Lanes names the retrieval lanes that surfaced this hit — any of
	// "vector", "bm25", "symbol", "graph" (#707). A hit several independent
	// lanes agreed on is higher-confidence than a single-lane one, so prefer
	// reading multi-lane hits first instead of trusting top-K position
	// uniformly. Pure provenance — it never reorders results.
	Lanes []string `json:"lanes,omitempty"`
	// Role is a compact tag describing how the symbol sits in the call
	// graph — e.g. "central:47/9pkg" (47 callers from 9 packages),
	// "leaf" (no callees), "exported-unused" (exported but no callers).
	// Empty when the symbol has no graph node (non-Go file, top-level
	// const, etc.) or sits in the unremarkable middle. See formatRole.
	Role    string `json:"role,omitempty"`
	Content string `json:"content"`
	// Handle is the opaque expansion handle for this hit's range (#344).
	// Echo it into read(handle=…) instead of constructing a path:line.
	// Empty for pseudo-hits that have no concrete file range (start_line 0).
	Handle string `json:"handle,omitempty"`
}

type SearchOutput struct {
	Status   string      `json:"status"`             // "ok" | "no-index" | "embedding-service-unreachable" | "error"
	Hint     string      `json:"hint,omitempty"`     // human-readable suggestion for the model
	Endpoint string      `json:"endpoint,omitempty"` // when unreachable
	Project  string      `json:"project,omitempty"`  // resolved project root
	Stale    bool        `json:"stale,omitempty"`    // last_indexed older than 24h, or a rebuild is in progress
	Indexing bool        `json:"indexing,omitempty"` // a re-index is underway; hits are partial (#531)
	Hits     []SearchHit `json:"hits"`
}

// resolveProject canonicalizes projectRoot (falling back to cwd) and
// resolves it to a Project. On failure it returns a non-empty hint that
// callers can surface as a Status:"error" response.
//
// Side effect: on successful resolution under stdio mode, ensures a
// per-project watcher goroutine is running (no-op if AutoWatch is
// disabled or one is already spawned).
func (s *Server) search(ctx context.Context, _ *sdk.CallToolRequest, in SearchInput) (*sdk.CallToolResult, SearchOutput, error) {
	out := SearchOutput{Hits: []SearchHit{}}
	if strings.TrimSpace(in.Query) == "" {
		return nil, SearchOutput{Status: "error", Hint: "query is empty — pass a natural-language description or code fragment"}, nil
	}
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, SearchOutput{Status: "error", Hint: hint}, nil
	}
	out.Project = p.Root

	// Repetition guard: at 7+ identical searches skip re-running the expensive
	// embedding+search pipeline and return just the hint. At 4–6, continue but
	// annotate. Must run before the embedding call so the skip is meaningful.
	if throttleHint, earlyReturn := s.searchThrottleHint(in.Query, p.Root); earlyReturn {
		return nil, SearchOutput{Status: "ok", Project: p.Root, Hint: throttleHint, Hits: []SearchHit{}}, nil
	} else if throttleHint != "" {
		out.Hint = throttleHint
	}

	if _, err := os.Stat(p.DBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			out.Status = "no-index"
			out.Hint = fmt.Sprintf("no index for %s — run `dex index %s` first, then retry. Fall back to grep / Glob in the meantime.", p.Root, p.Root)
			return nil, out, nil
		}
		out.Status = "error"
		out.Hint = err.Error()
		return nil, out, nil
	}

	k, candidateK, kHint := clampSearchK(in, p.Root)

	em := s.EmbedClient
	vecs, err := em.Embed(ctx, []string{in.Query})
	if err != nil {
		if errors.Is(err, embed.ErrUnreachable) {
			out.Status = "embedding-service-unreachable"
			out.Endpoint = em.Endpoint()
			out.Hint = "the local embedding service is offline — fall back to grep / Glob / ripgrep for this query."
			return nil, out, nil
		}
		out.Status = "error"
		out.Hint = fmt.Sprintf("embed: %v", err)
		return nil, out, nil
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		out.Status = "error"
		out.Hint = fmt.Sprintf("open index: %v", err)
		return nil, out, nil
	}

	stats, err := st.Stats(ctx)
	if err == nil && !stats.LastIndex.IsZero() && time.Since(stats.LastIndex) > 24*time.Hour {
		out.Stale = true
		out.Hint = fmt.Sprintf("index is %s old — results may be stale; run `dex index %s` to refresh.",
			time.Since(stats.LastIndex).Round(time.Hour), p.Root)
	}
	// An active rebuild trumps age: the index is being rewritten right now,
	// so any hits are partial. Mark stale and surface the more urgent note.
	if indexing, note := indexingNotice(ctx, st); indexing {
		out.Stale = true
		out.Indexing = true
		out.Hint = note
	}

	// Session task feeds the ECS rerank inside the assembler and the activity
	// nudge below.
	var sessionTask string
	if ss, ok, err := st.SessionGet(ctx); err == nil && ok {
		sessionTask = ss.Task
	}

	// Ranking core (#111): the search-verb assembler owns the fuse → symbol →
	// spreading-activation → rerank → ECS → multi-scale sequence. The transport
	// injects the TF-IDF multi-scale filter (it holds the multi-scale index
	// cache) and keeps embedding, language/path_glob filtering, loop-detect,
	// SLO and the wire projection.
	asm := retrieve.SearchAssembler{Service: s.Retrieve}
	hits, err := asm.Assemble(ctx, st, retrieve.SearchRequest{
		Query:       in.Query,
		Vec:         vecs[0],
		CandidateK:  candidateK,
		SessionTask: sessionTask,
		MultiScale: func(h []store.Hit) []store.Hit {
			return s.applyMultiScaleFilter(ctx, st, p.DBPath, in.Query, h)
		},
	})
	if err != nil {
		out.Status = "error"
		out.Hint = err.Error()
		return nil, out, nil
	}

	s.activityRecord(p.Root, 1)

	// Apply language and path_glob filters post-ranking, then trim to k.
	exts := langToExtensions(in.Languages)
	preFilterHits := hits
	hits = filterHits(hits, exts, in.PathGlob, k)
	// A filter that drops every ranked hit yields an empty result that looks
	// identical to "the query matched nothing" — a typo'd language or glob is
	// then indistinguishable from a real miss. Flag it (issue #512).
	filteredToEmpty := len(hits) == 0 && len(preFilterHits) > 0 && (len(exts) > 0 || in.PathGlob != "")

	// Loop detection: block/reduce/hint before building the response.
	ldLevel, ldHint := s.ld().Check("find", throttle.ArgsKey(in.Query), true)
	if ldLevel == throttle.Block {
		return nil, SearchOutput{Status: "loop-blocked", Project: p.Root, Hint: ldHint}, nil
	}

	out.Status = "ok"
	if out.Hint == "" {
		out.Hint = s.activityNudge(p.Root, sessionTask)
	}
	if ldHint != "" && out.Hint == "" {
		out.Hint = ldHint
	}
	searchBuildHits(hits, in, ldLevel, ldHint, &out)

	// When a language/path_glob filter is what emptied the result, the
	// diagnostic takes precedence over softer nudges: it tells the caller
	// the query did match, the filter just rejected everything (issue #512).
	if filteredToEmpty && len(out.Hits) == 0 {
		out.Hint = filterMissHint(in.Languages, exts, in.PathGlob, preFilterHits)
	}

	// Stamp expansion handles on the final hit set (#344) — after truncation
	// so we don't mint handles for hits we drop.
	stampSearchHandles(out.Hits)

	// SLO monitoring: record this tool call and check thresholds.
	tr := s.sloFor(p.Root)
	tr.RecordToolCall()
	out.Hint = appendSLOAnnotation(out.Hint, tr)
	// Surface any k override last so it leads the hint and survives the
	// branch-specific hint assignments above (#543).
	out.Hint = prependHint(out.Hint, kHint)
	return nil, out, nil
}

// searchBuildHits appends filtered SearchHits into out and applies loop-detect
// reduction when ldLevel == throttle.Reduce.
func searchBuildHits(hits []store.Hit, in SearchInput, ldLevel throttle.Level, ldHint string, out *SearchOutput) {
	for _, h := range hits {
		if excluded(h.Path, in.Exclude) {
			continue
		}
		out.Hits = append(out.Hits, SearchHit{
			Path:        h.Path,
			Kind:        h.Kind,
			StartLine:   h.StartLine,
			EndLine:     h.EndLine,
			SortScore:   h.DisplayScore(),
			Score:       h.Score,
			BM25Score:   h.BM25Score,
			RRFScore:    h.RRFScore,
			RerankScore: h.RerankScore,
			Lanes:       h.Lanes.Names(),
			Role:        formatRole(h.Name, h.InDegree, h.OutDegree, h.CrossPkgCallers, h.Betweenness),
			Content:     h.Content,
		})
	}
	if ldLevel == throttle.Reduce && len(out.Hits) > 5 {
		out.Hits = out.Hits[:5]
		out.Hint = ldHint + " [reduced: showing top 5]"
	}
}

// appendSLOAnnotation appends any SLO annotation to hint (space-joined).
func appendSLOAnnotation(hint string, tr *slo.Tracker) string {
	if ann := sloAnnotation(tr.Check()); ann != "" {
		if hint == "" {
			return ann
		}
		return hint + " " + ann
	}
	return hint
}

// prependHint puts lead in front of an existing hint (space-joined), returning
// whichever is non-empty when one side is blank.
func prependHint(existing, lead string) string {
	switch {
	case lead == "":
		return existing
	case existing == "":
		return lead
	default:
		return lead + " " + existing
	}
}

// Symbol-lane RRF fusion moved to internal/retrieve.FuseWithSymbols (#480);
// the symbol leg + dedup moved to internal/retrieve.SearchAssembler (#111).
