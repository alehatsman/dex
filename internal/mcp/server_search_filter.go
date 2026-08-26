package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/store"
)

// excluded returns true when path matches any entry in the exclude list.
//
// An entry that contains glob metacharacters (*?[) is matched as a glob via
// the same engine as path_glob — against the full relative path AND the
// basename, so a slash-free pattern like "*_test.go" excludes nested files
// (.gitignore / rg --glob semantics) instead of silently no-op'ing (#536). A
// plain entry keeps directory-prefix/equality semantics, so "internal/" still
// excludes the whole subtree.
func excluded(path string, exclude []string) bool {
	for _, ex := range exclude {
		if ex == "" {
			continue
		}
		if strings.ContainsAny(ex, "*?[") {
			if matchGlob(ex, path) || matchGlob(ex, filepath.Base(path)) {
				return true
			}
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
// clampSearchK returns the effective k and candidateK from the search input,
// plus a kHint when the caller's explicit k was overridden so the adjustment
// isn't silent (parity with the CLI's strict validation, #523/#543). An
// omitted k (0) defaults silently; a negative k is reported as invalid; a k
// that is capped (over the max 30 or a profile budget) is reported as capped.
// candidateK is inflated when language or path filters are active so post-filter
// trimming still returns k results.
func clampSearchK(in SearchInput, projectRoot string) (k, candidateK int, kHint string) {
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
	switch {
	case in.K < 0:
		kHint = fmt.Sprintf("requested k=%d is invalid; using k=%d", in.K, k)
	case in.K > 0 && k != in.K:
		kHint = fmt.Sprintf("requested k=%d capped to k=%d (max 30)", in.K, k)
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
	return k, candidateK, kHint
}

// applyMultiScaleFilter restricts hits to the structurally-relevant files for
// NL and Architecture queries using the in-RAM TF-IDF index. Symbol queries
// and multi-scale build failures are passed through unchanged.
//
// After path-filtering, any dropped hit with a positive BM25Score is re-added.
// BM25Score is zero only when a hit came through the semantic lane with no
// lexical match at all; a positive value means FTS5 matched at least one query
// term (BM25 IDF already down-weights common words, so this is not noisy).
// This rescues implementation chunks whose file lost the meso TF-IDF race to a
// higher-entropy doc (e.g. a spec file) while still having direct keyword hits.
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
			// Re-admit dropped hits that had a lexical match (BM25Score > 0).
			// Track by path so we don't re-add multiple chunks from the same
			// already-included file.
			inFiltered := make(map[string]struct{}, len(filtered))
			for _, h := range filtered {
				inFiltered[h.Path] = struct{}{}
			}
			for _, h := range hits {
				if _, ok := inFiltered[h.Path]; !ok && h.BM25Score > 0 {
					filtered = append(filtered, h)
					inFiltered[h.Path] = struct{}{}
				}
			}
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

// filterMissHint explains why a language/path_glob filter produced no
// results when the unfiltered ranking did have hits. It reports the
// extensions actually present among the ranked hits so the caller can tell
// a typo'd filter from a genuine miss (issue #512).
func filterMissHint(langs, exts []string, glob string, preFilter []store.Hit) string {
	present := make(map[string]bool)
	for _, h := range preFilter {
		if ext := strings.TrimPrefix(filepath.Ext(h.Path), "."); ext != "" {
			present[ext] = true
		}
	}
	availy := make([]string, 0, len(present))
	for e := range present {
		availy = append(availy, e)
	}
	sort.Strings(availy)

	var parts []string
	if len(exts) > 0 {
		var missing []string
		for _, e := range exts {
			if !present[e] {
				missing = append(missing, e)
			}
		}
		if len(missing) > 0 {
			parts = append(parts, fmt.Sprintf("languages filter %v matched none of the ranked hits (extensions present: %s)",
				langs, strings.Join(availy, ", ")))
		} else {
			parts = append(parts, fmt.Sprintf("languages filter %v matched none of the ranked hits", langs))
		}
	}
	if glob != "" {
		parts = append(parts, fmt.Sprintf("path_glob %q matched none of the ranked hits", glob))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") +
		fmt.Sprintf(" — %d unfiltered hit(s) were dropped; relax or remove the filter.", len(preFilter))
}
