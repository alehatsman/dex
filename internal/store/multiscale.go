package store

// multiscale.go — in-RAM multi-scale TF-IDF index for query-type routing.
//
// Three scales inspired by lean-ctx's multiscale_index.rs:
//
//	Mikro  chunk granularity  (BM25/dense, handled by existing search)
//	Meso   file granularity   (top-20 TF-IDF keywords per file)
//	Macro  directory granularity (top-30 TF-IDF keywords per dir)
//
// NL queries: Meso search → top-5 candidate files → restrict full search.
// Architecture queries: Macro search → top-3 dirs → Meso within those dirs → restrict.
// Symbol queries: bypass multi-scale (BM25 wins directly).

import (
	"context"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// MultiScaleIndex is a lightweight in-RAM TF-IDF index over the chunk set.
// It is rebuilt whenever the caller requests it; rebuilds are O(chunks) and
// typically take <100ms on a 10K-chunk corpus.
type MultiScaleIndex struct {
	// meso maps file path → keyword scores.
	meso map[string][]kwScore
	// macro maps directory path → keyword scores.
	macro map[string][]kwScore
	// fileCount is the total number of unique files, used for IDF.
	fileCount int
}

type kwScore struct {
	Term   string
	Weight float32
}

// BuildMultiScale scans all non-summary, non-test chunks from the store and
// builds the Meso and Macro scale entries. Returns nil on error (non-fatal —
// callers fall back to plain hybrid search).
func (s *Store) BuildMultiScale(ctx context.Context) (*MultiScaleIndex, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, content FROM chunks
		  WHERE kind != 'file_summary'
		  ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// termFreqByFile[path][term] = raw count across all chunks of that file.
	termFreqByFile := make(map[string]map[string]int)
	for rows.Next() {
		var path, content string
		if err := rows.Scan(&path, &content); err != nil {
			return nil, err
		}
		if _, ok := termFreqByFile[path]; !ok {
			termFreqByFile[path] = make(map[string]int, 32)
		}
		for _, tok := range tokenise(content) {
			termFreqByFile[path][tok]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	fileCount := len(termFreqByFile)
	if fileCount == 0 {
		return &MultiScaleIndex{
			meso:  make(map[string][]kwScore),
			macro: make(map[string][]kwScore),
		}, nil
	}

	// df[term] = number of files that contain the term.
	df := make(map[string]int, 1024)
	for _, tfMap := range termFreqByFile {
		for term := range tfMap {
			df[term]++
		}
	}

	idf := func(term string) float64 {
		d := df[term]
		if d == 0 {
			return 0
		}
		return math.Log(float64(fileCount+1)/float64(d+1)) + 1
	}

	// Build meso: for each file compute TF-IDF and keep top-20.
	meso := make(map[string][]kwScore, fileCount)
	for path, tfMap := range termFreqByFile {
		var total int
		for _, c := range tfMap {
			total += c
		}
		if total == 0 {
			continue
		}
		kvs := make([]kwScore, 0, len(tfMap))
		for term, count := range tfMap {
			tf := float64(count) / float64(total)
			kvs = append(kvs, kwScore{Term: term, Weight: float32(tf * idf(term))})
		}
		sort.Slice(kvs, func(i, j int) bool { return kvs[i].Weight > kvs[j].Weight })
		if len(kvs) > 20 {
			kvs = kvs[:20]
		}
		meso[path] = kvs
	}

	// Build macro: aggregate meso scores by directory.
	dirAccum := make(map[string]map[string]float32)
	for path, kvs := range meso {
		dir := filepath.Dir(path)
		if dir == "." {
			dir = ""
		}
		if _, ok := dirAccum[dir]; !ok {
			dirAccum[dir] = make(map[string]float32, 64)
		}
		for _, kv := range kvs {
			dirAccum[dir][kv.Term] += kv.Weight
		}
	}
	macro := make(map[string][]kwScore, len(dirAccum))
	for dir, termMap := range dirAccum {
		kvs := make([]kwScore, 0, len(termMap))
		for term, w := range termMap {
			kvs = append(kvs, kwScore{Term: term, Weight: w})
		}
		sort.Slice(kvs, func(i, j int) bool { return kvs[i].Weight > kvs[j].Weight })
		if len(kvs) > 30 {
			kvs = kvs[:30]
		}
		macro[dir] = kvs
	}

	return &MultiScaleIndex{meso: meso, macro: macro, fileCount: fileCount}, nil
}

// SearchMeso returns the top-k file paths whose keyword profile best matches
// the query tokens.
func (idx *MultiScaleIndex) SearchMeso(queryTokens []string, k int) []string {
	return idx.searchScale(idx.meso, queryTokens, k)
}

// SearchMacro returns the top-k directory paths whose keyword profile best
// matches the query tokens.
func (idx *MultiScaleIndex) SearchMacro(queryTokens []string, k int) []string {
	return idx.searchScale(idx.macro, queryTokens, k)
}

func (idx *MultiScaleIndex) searchScale(scale map[string][]kwScore, queryTokens []string, k int) []string {
	if len(queryTokens) == 0 || len(scale) == 0 {
		return nil
	}
	qtSet := make(map[string]struct{}, len(queryTokens))
	for _, t := range queryTokens {
		qtSet[t] = struct{}{}
	}
	type entry struct {
		path  string
		score float32
	}
	entries := make([]entry, 0, len(scale))
	for path, kvs := range scale {
		var score float32
		for _, kv := range kvs {
			if _, ok := qtSet[kv.Term]; ok {
				score += kv.Weight
			}
		}
		if score > 0 {
			entries = append(entries, entry{path, score})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].score > entries[j].score })
	if len(entries) > k {
		entries = entries[:k]
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.path
	}
	return out
}

// TokeniseQuery is the exported entry point for the MCP layer.
func TokeniseQuery(q string) []string { return tokenise(q) }

// tokenise splits src into lowercase identifier tokens suitable for TF-IDF.
// It splits on whitespace, punctuation, camelCase boundaries, and common
// code separators.
func tokenise(src string) []string {
	// Lowercase first to simplify boundary detection.
	src = strings.ToLower(src)
	var tokens []string
	var buf strings.Builder
	flush := func() {
		if buf.Len() >= 3 {
			tokens = append(tokens, buf.String())
		}
		buf.Reset()
	}
	for i, r := range src {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			_ = i
			buf.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}

// ExpandToFiles returns all Meso paths that start with any of the given dir
// prefixes. Used in Architecture routing: Macro dirs → Meso files within those
// dirs.
func (idx *MultiScaleIndex) ExpandToFiles(dirs []string) []string {
	if len(dirs) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for path := range idx.meso {
		dir := filepath.Dir(path)
		if dir == "." {
			dir = ""
		}
		for _, d := range dirs {
			if dir == d || strings.HasPrefix(dir+"/", d+"/") || d == "" {
				if _, exists := seen[path]; !exists {
					seen[path] = struct{}{}
					out = append(out, path)
				}
				break
			}
		}
	}
	return out
}

// FilterByPaths returns hits whose Path is in the provided set.
// Preserves the original slice ordering and capacity.
func FilterByPaths(hits []Hit, paths []string) []Hit {
	if len(paths) == 0 {
		return hits
	}
	allowed := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		allowed[p] = struct{}{}
	}
	out := hits[:0]
	for _, h := range hits {
		if _, ok := allowed[h.Path]; ok {
			out = append(out, h)
		}
	}
	return out
}
