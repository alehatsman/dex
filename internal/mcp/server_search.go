package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: search_semantic ────────────────────────────────────────────────

type SearchInput struct {
	Query       string   `json:"query" jsonschema:"natural-language or code query"`
	ProjectRoot string   `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int      `json:"k,omitempty" jsonschema:"number of results to return (default 8, max 30)"`
	Exclude     []string `json:"exclude,omitempty" jsonschema:"path prefixes to skip (e.g. ['vendor/', 'internal/legacy/'])"`
	Languages   []string `json:"languages,omitempty" jsonschema:"restrict results to these languages (e.g. ['go','typescript']); accepts language names or raw extensions (.rs, .go)"`
	PathGlob    string   `json:"path_glob,omitempty" jsonschema:"glob pattern matched against relative file path (e.g. 'internal/**', '**/test*')"`
}

type SearchHit struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	// Score is the cosine similarity in [-1, 1]. Always populated.
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
	Stale    bool        `json:"stale,omitempty"`    // last_indexed older than 24h
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
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, SearchOutput{Status: "error", Hint: hint}, nil
	}
	out.Project = p.Root

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

	k, candidateK := clampSearchK(in, p.Root)

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

	hits, err := st.SearchFused(ctx, vecs[0], in.Query, candidateK)
	if err != nil {
		out.Status = "error"
		out.Hint = fmt.Sprintf("search: %v", err)
		return nil, out, nil
	}

	hits = s.applyMultiScaleFilter(ctx, st, p.DBPath, in.Query, hits)

	// Symbol leg: extract identifier tokens from the query, look them up
	// by exact name, and RRF-fuse with the semantic results. Runs in the
	// same request with no extra embedding round-trip — FindSymbol is a
	// pure SQL index scan.
	idents := retrieve.ExtractIdentifiers(in.Query)
	if len(idents) > 0 {
		symPool := candidateK * 3
		if symPool < 15 {
			symPool = 15
		}
		if symHits := collectSymbolHits(ctx, st, idents, symPool); len(symHits) > 0 {
			hits = fuseWithSymbols(hits, symHits, k)
		}
	}

	// Graph-proximity lane: spreading activation from session-recent files and
	// the current semantic hits. Silently skips when no session exists or the
	// graph hasn't been built — never fails the search.
	var sessionTask string
	if ss, ok, err := st.SessionGet(ctx); err == nil && ok {
		sessionTask = ss.Task
	}
	hits = st.FuseSpreadingActivation(ctx, hits, vecs[0], candidateK)

	hits, err = st.RerankFused(ctx, in.Query, hits, candidateK)
	if err != nil {
		out.Status = "error"
		out.Hint = fmt.Sprintf("rerank: %v", err)
		return nil, out, nil
	}
	hits = ecsRerank(hits, extractTaskKWs(sessionTask))
	s.activityRecord(p.Root, 1)

	// Apply language and path_glob filters post-ranking, then trim to k.
	exts := langToExtensions(in.Languages)
	hits = filterHits(hits, exts, in.PathGlob, k)

	// Loop detection: block/reduce/hint before building the response.
	ldLevel, ldHint := s.ld().Check("find", argsKey(in.Query), true)
	if ldLevel == ThrottleBlock {
		return nil, SearchOutput{Status: "loop-blocked", Project: p.Root, Hint: ldHint}, nil
	}

	out.Status = "ok"
	if hint := s.searchThrottleHint(in.Query, p.Root); hint != "" {
		out.Hint = hint
	}
	if out.Hint == "" {
		out.Hint = s.activityNudge(p.Root, sessionTask)
	}
	if ldHint != "" && out.Hint == "" {
		out.Hint = ldHint
	}
	for _, h := range hits {
		if excluded(h.Path, in.Exclude) {
			continue
		}
		out.Hits = append(out.Hits, SearchHit{
			Path:        h.Path,
			Kind:        h.Kind,
			StartLine:   h.StartLine,
			EndLine:     h.EndLine,
			Score:       h.Score,
			BM25Score:   h.BM25Score,
			RRFScore:    h.RRFScore,
			RerankScore: h.RerankScore,
			Role:        formatRole(h.Name, h.InDegree, h.OutDegree, h.CrossPkgCallers, h.Betweenness),
			Content:     h.Content,
		})
	}
	if ldLevel == ThrottleReduce && len(out.Hits) > 5 {
		out.Hits = out.Hits[:5]
		out.Hint = ldHint + " [reduced: showing top 5]"
	}

	// Stamp expansion handles on the final hit set (#344) — after truncation
	// so we don't mint handles for hits we drop.
	stampSearchHandles(out.Hits)

	// SLO monitoring: record this tool call and check thresholds.
	tr := s.sloFor(p.Root)
	tr.RecordToolCall()
	if ann := sloAnnotation(tr.Check()); ann != "" {
		if out.Hint == "" {
			out.Hint = ann
		} else {
			out.Hint += " " + ann
		}
	}
	return nil, out, nil
}

// collectSymbolHits runs FindSymbol for each identifier and returns a
// deduplicated hit list (keyed by path+start_line), in the order they
// surfaced across all identifier queries.
func collectSymbolHits(ctx context.Context, st *store.Store, idents []string, pool int) []store.Hit {
	seen := map[string]struct{}{}
	var out []store.Hit
	for _, id := range idents {
		bare := id
		if i := strings.LastIndex(bare, "."); i >= 0 {
			bare = bare[i+1:]
		}
		hits, err := st.FindSymbol(ctx, bare, pool)
		if err != nil {
			continue
		}
		for _, h := range hits {
			key := h.Path + ":" + strconv.Itoa(h.StartLine)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, h)
			if len(out) >= pool {
				return out
			}
		}
	}
	return out
}

// fuseWithSymbols merges a semantic hit list with a symbol hit list via
// Reciprocal Rank Fusion (k=60) and returns the top-n results. The
// dedup key is (path, start_line). Semantic hits already carry Score /
// BM25Score / RRFScore from the store; symbol-only hits get Score=1.0
// (exact-match signal). The new RRFScore field reflects the cross-lane
// fused rank for all returned hits.
//
// Like the graph lane, both legs are scored from rank position only — the
// incoming Hit.Score magnitude is discarded — so this stage is fusion-mode
// independent (FusionRRF vs FusionLinear changes only the semantic ORDER, not
// the symbol lane's relative weight).
func fuseWithSymbols(semantic, symbol []store.Hit, n int) []store.Hit {
	const kRRF = 60
	type hitKey struct {
		path string
		line int
	}
	scores := make(map[hitKey]float32, len(semantic)+len(symbol))
	byKey := make(map[hitKey]store.Hit, len(semantic)+len(symbol))

	for i, h := range semantic {
		hk := hitKey{h.Path, h.StartLine}
		scores[hk] += 1.0 / float32(kRRF+i+1)
		byKey[hk] = h
	}
	for i, h := range symbol {
		hk := hitKey{h.Path, h.StartLine}
		scores[hk] += 1.0 / float32(kRRF+i+1)
		if _, exists := byKey[hk]; !exists {
			h.Score = 1.0 // exact name match
			byKey[hk] = h
		}
	}

	type ranked struct {
		key   hitKey
		score float32
	}
	all := make([]ranked, 0, len(scores))
	for hk, s := range scores {
		all = append(all, ranked{hk, s})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > n {
		all = all[:n]
	}

	out := make([]store.Hit, 0, len(all))
	for _, r := range all {
		h := byKey[r.key]
		h.RRFScore = r.score
		out = append(out, h)
	}
	return out
}

// excluded returns true when path matches any entry in the exclude list.
// An exclude entry matches if path equals it or path has it as a prefix
// (treating it as a directory prefix).
func excluded(path string, exclude []string) bool {
	for _, ex := range exclude {
		if ex == "" {
			continue
		}
		if path == ex || strings.HasPrefix(path, ex) {
			return true
		}
	}
	return false
}

// langToExtensions maps human-readable language names to their file extensions.
// Raw extensions (with or without leading dot) pass through unchanged.
func langToExtensions(langs []string) []string {
	aliasMap := map[string][]string{
		"typescript": {"ts", "tsx"},
		"javascript": {"js", "jsx", "mjs", "cjs"},
		"c++":        {"cpp", "hpp", "cc", "hh"},
		"cpp":        {"cpp", "hpp", "cc", "hh"},
		"cc":         {"cpp", "hpp", "cc", "hh"},
		"ruby":       {"rb"},
		"kotlin":     {"kt", "kts"},
		"yaml":       {"yaml", "yml"},
		"yml":        {"yaml", "yml"},
		"python":     {"py"},
		"java":       {"java"},
		"go":         {"go"},
		"rust":       {"rs"},
		"c":          {"c", "h"},
		"swift":      {"swift"},
		"scala":      {"scala"},
		"shell":      {"sh", "bash"},
		"bash":       {"sh", "bash"},
		"html":       {"html", "htm"},
		"css":        {"css"},
		"json":       {"json"},
		"markdown":   {"md", "mdx"},
		"proto":      {"proto"},
		"sql":        {"sql"},
		"toml":       {"toml"},
	}
	seen := make(map[string]struct{})
	var out []string
	for _, lang := range langs {
		normalized := strings.ToLower(strings.TrimSpace(lang))
		if exts, ok := aliasMap[normalized]; ok {
			for _, e := range exts {
				if _, dup := seen[e]; !dup {
					seen[e] = struct{}{}
					out = append(out, e)
				}
			}
		} else {
			// Raw extension: strip leading dot
			ext := strings.TrimPrefix(normalized, ".")
			if _, dup := seen[ext]; !dup {
				seen[ext] = struct{}{}
				out = append(out, ext)
			}
		}
	}
	return out
}

// matchGlob matches pattern against path with support for ** (matches any
// number of path segments). A trailing /** suffix is treated as a directory
// prefix match. Single * is handled by filepath.Match per-segment.
func matchGlob(pattern, path string) bool {
	// Fast path: no double-star — delegate to filepath.Match directly.
	if !strings.Contains(pattern, "**") {
		ok, err := filepath.Match(pattern, path)
		return err == nil && ok
	}
	// Split on ** and match each segment in order.
	parts := strings.Split(pattern, "**")
	remaining := path
	for i, part := range parts {
		if part == "" {
			continue
		}
		// Trim leading separator from part for cleaner matching.
		part = strings.Trim(part, "/")
		if part == "" {
			continue
		}
		if i == 0 {
			// First segment must be a prefix.
			if !strings.HasPrefix(remaining, part) {
				return false
			}
			remaining = remaining[len(part):]
			remaining = strings.TrimPrefix(remaining, "/")
		} else if i == len(parts)-1 {
			// Last segment must be a suffix.
			ok, err := filepath.Match(part, filepath.Base(remaining))
			if err != nil || !ok {
				// Also try matching the full remaining path against part.
				ok2, _ := filepath.Match(part, remaining)
				if !ok2 {
					return false
				}
			}
		} else {
			idx := strings.Index(remaining, part)
			if idx < 0 {
				return false
			}
			remaining = remaining[idx+len(part):]
			remaining = strings.TrimPrefix(remaining, "/")
		}
	}
	return true
}

// filterHits applies optional language (by extension) and path_glob filters,
// then trims to at most limit results. When exts and glob are both empty
// the slice is trimmed to limit unchanged.
// clampSearchK returns the effective k and candidateK from the search input.
// candidateK is inflated when language or path filters are active so post-filter
// trimming still returns k results.
func clampSearchK(in SearchInput, projectRoot string) (k, candidateK int) {
	k = in.K
	if k <= 0 {
		k = 8
	}
	if k > 30 {
		k = 30
	}
	if prof := profiles.Active(projectRoot); prof.Budget.MaxFiles > 0 && k > prof.Budget.MaxFiles {
		k = prof.Budget.MaxFiles
	}
	candidateK = k
	if len(in.Languages) > 0 || in.PathGlob != "" {
		candidateK = k * 10
		if candidateK < 50 {
			candidateK = 50
		}
		if candidateK > 500 {
			candidateK = 500
		}
	}
	return k, candidateK
}

// applyMultiScaleFilter restricts hits to the structurally-relevant files for
// NL and Architecture queries using the in-RAM TF-IDF index. Symbol queries
// and multi-scale build failures are passed through unchanged.
func (s *Server) applyMultiScaleFilter(ctx context.Context, st *store.Store, dbPath, query string, hits []store.Hit) []store.Hit {
	qt := store.ClassifyQueryType(query)
	if qt == store.QueryTypeSymbol {
		return hits
	}
	idx, idxErr := s.cachedBuildMultiScale(ctx, st, dbPath)
	if idxErr != nil || idx == nil {
		return hits
	}
	queryToks := store.TokeniseQuery(query)
	var candidatePaths []string
	switch qt {
	case store.QueryTypeArchitecture:
		dirs := idx.SearchMacro(queryToks, 3)
		candidatePaths = idx.ExpandToFiles(dirs)
		if len(candidatePaths) < 5 {
			candidatePaths = append(candidatePaths, idx.SearchMeso(queryToks, 10)...)
		}
	case store.QueryTypeNL:
		candidatePaths = idx.SearchMeso(queryToks, 8)
	}
	if len(candidatePaths) >= 3 {
		if filtered := store.FilterByPaths(hits, candidatePaths); len(filtered) > 0 {
			return filtered
		}
	}
	return hits
}

func filterHits(hits []store.Hit, exts []string, glob string, limit int) []store.Hit {
	if len(exts) == 0 && glob == "" {
		if len(hits) > limit {
			return hits[:limit]
		}
		return hits
	}
	out := hits[:0]
	for _, h := range hits {
		if len(exts) > 0 {
			ext := strings.TrimPrefix(filepath.Ext(h.Path), ".")
			matched := false
			for _, e := range exts {
				if ext == e {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if glob != "" {
			if !matchGlob(glob, h.Path) {
				continue
			}
		}
		out = append(out, h)
		if len(out) >= limit {
			break
		}
	}
	return out
}
