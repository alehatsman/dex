// Package trigram provides a RAM-resident trigram inverted index for fast
// candidate narrowing on identifier/literal queries. It is a pure-postings
// tier (no Bloom filter) suited for repos up to ~200K files / 12M entries.
package trigram

import (
	"os"
	"regexp/syntax"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	maxFiles   = 200_000
	maxEntries = 12_000_000
	IndexTTL   = 15 * time.Second
)

// fileFP is a lightweight (mtime, size) fingerprint for a single file.
type fileFP struct {
	mtime int64
	size  int64
}

// Index is a RAM-resident trigram inverted index.
// Build it with Build; query with Narrow; check freshness with Stale.
type Index struct {
	postings map[uint32][]uint32 // trigram → sorted file IDs
	files    []string            // file ID → absolute path
	fps      []fileFP            // file ID → fingerprint at build time
	builtAt  time.Time
	mu       sync.RWMutex
}

// Build creates a new Index over absFiles. Files are stored as-is; caller
// controls which files to include. Individual file read errors are skipped.
func Build(absFiles []string) *Index {
	if len(absFiles) > maxFiles {
		absFiles = absFiles[:maxFiles]
	}
	files := make([]string, len(absFiles))
	copy(files, absFiles)

	postings := make(map[uint32][]uint32, len(files)*8)
	fps := make([]fileFP, len(files))
	totalEntries := 0
	indexedUntil := len(files)

	for id, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		fps[id] = fileFP{mtime: info.ModTime().UnixNano(), size: info.Size()}

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		seen := make(map[uint32]struct{})
		extractTrigrams(string(data), func(tg uint32) {
			if _, dup := seen[tg]; dup {
				return
			}
			seen[tg] = struct{}{}
			postings[tg] = append(postings[tg], uint32(id))
			totalEntries++
		})
		if totalEntries >= maxEntries {
			indexedUntil = id + 1
			break
		}
	}

	files = files[:indexedUntil]
	fps = fps[:indexedUntil]

	for tg := range postings {
		list := postings[tg]
		sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
	}

	return &Index{
		postings: postings,
		files:    files,
		fps:      fps,
		builtAt:  time.Now(),
	}
}

// Stale returns true when the TTL has expired or any spot-checked file
// fingerprint changed.
func (idx *Index) Stale() bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if time.Since(idx.builtAt) > IndexTTL {
		return true
	}
	stride := len(idx.files) / 50
	if stride < 1 {
		stride = 1
	}
	for i := 0; i < len(idx.files); i += stride {
		info, err := os.Stat(idx.files[i])
		if err != nil {
			return true
		}
		fp := idx.fps[i]
		if info.ModTime().UnixNano() != fp.mtime || info.Size() != fp.size {
			return true
		}
	}
	return false
}

// Narrow returns the absolute paths of candidate files that may contain query
// and true. When the query has no word-trigrams (pure regex metacharacter
// pattern), it returns (nil, false) and the caller should use the full list.
func (idx *Index) Narrow(query string) ([]string, bool) {
	// A case-insensitive pattern (e.g. `(?i)Foo`) matches text the
	// case-sensitive trigrams extracted here would never index, so narrowing
	// would soundlessly drop real matches. Decline to narrow and let the
	// caller full-scan instead (#541).
	if patternFoldsCase(query) {
		return nil, false
	}

	idx.mu.RLock()
	defer idx.mu.RUnlock()

	trigrams := wordTrigrams(query)
	if len(trigrams) == 0 {
		return nil, false
	}

	// Seed with the rarest (shortest posting list) trigram.
	seedTG := uint32(0)
	seedLen := -1
	for _, tg := range trigrams {
		list, ok := idx.postings[tg]
		if !ok {
			return []string{}, true // no file can contain all trigrams
		}
		if seedLen < 0 || len(list) < seedLen {
			seedTG = tg
			seedLen = len(list)
		}
	}
	if seedLen < 0 {
		return nil, false
	}

	candidates := append([]uint32(nil), idx.postings[seedTG]...)
	for _, tg := range trigrams {
		if tg == seedTG {
			continue
		}
		list, ok := idx.postings[tg]
		if !ok {
			return []string{}, true
		}
		candidates = intersectSorted(candidates, list)
		if len(candidates) == 0 {
			return []string{}, true
		}
	}

	out := make([]string, 0, len(candidates))
	for _, id := range candidates {
		if int(id) < len(idx.files) {
			out = append(out, idx.files[id])
		}
	}
	return out, true
}

// wordTrigrams extracts unique trigrams from [A-Za-z0-9_] runs in s.
// Returns nil when no run of length ≥ 3 exists.
// patternFoldsCase reports whether the RE2 pattern matches case-insensitively
// anywhere — via a `(?i)` flag or an inline fold — so the trigram prefilter
// can decline a query whose case-sensitive trigrams would be unsound. A
// pattern that fails to parse is treated as non-folding: the caller has
// already validated/compiled it, and a parse miss here just means no narrowing
// is skipped.
func patternFoldsCase(pattern string) bool {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return false
	}
	return reFoldsCase(re)
}

func reFoldsCase(re *syntax.Regexp) bool {
	if re.Flags&syntax.FoldCase != 0 {
		return true
	}
	for _, sub := range re.Sub {
		if reFoldsCase(sub) {
			return true
		}
	}
	return false
}

func wordTrigrams(s string) []uint32 {
	seen := make(map[uint32]struct{})
	var out []uint32
	var run strings.Builder
	flush := func() {
		w := run.String()
		run.Reset()
		for i := 0; i+2 < len(w); i++ {
			tg := mkTrigram(w[i], w[i+1], w[i+2])
			if _, dup := seen[tg]; !dup {
				seen[tg] = struct{}{}
				out = append(out, tg)
			}
		}
	}
	for _, r := range s {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			run.WriteRune(r)
		} else if run.Len() > 0 {
			flush()
		}
	}
	if run.Len() > 0 {
		flush()
	}
	return out
}

// extractTrigrams calls fn for every byte-level trigram in s.
// Uses byte-level sliding window so no file content is missed.
func extractTrigrams(s string, fn func(uint32)) {
	for i := 0; i+2 < len(s); i++ {
		fn(mkTrigram(s[i], s[i+1], s[i+2]))
	}
}

func mkTrigram(a, b, c byte) uint32 {
	return uint32(a)<<16 | uint32(b)<<8 | uint32(c)
}

// intersectSorted returns the sorted intersection of two sorted uint32 slices.
func intersectSorted(a, b []uint32) []uint32 {
	out := make([]uint32, 0, min(len(a), len(b)))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
