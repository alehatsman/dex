package index

import (
	"bytes"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Chunk-density guard: defend the chunk/embed path against machine-emitted
// files (minified bundles, large data fixtures) that produce hundreds of
// near-zero-value chunks, bloating the index and dominating embed time. The
// graph parser already caps parse size (#443); this is the equivalent guard
// on the chunk/embed side. Two independent checks, both configurable via
// .dex/config.yml:
//
//	index:
//	  max_chunks_per_file: 500   # per-file chunk cap; <=0 disables
//	  skip_minified: true        # skip minified/bundled files before chunking
const (
	// DefaultMaxChunksPerFile is the per-file chunk count above which a file is
	// coarsened by chunk.PackDense (see index.go) when neither the caller nor
	// .dex/config.yml set one. A file emitting more chunks than this is almost
	// always a dense generated declaration table rather than hand-written
	// source; packing bounds its vector count without dropping content.
	// Deliberately high: ordinary large source files stay well under it and are
	// never coarsened.
	DefaultMaxChunksPerFile = 500

	// minifiedMinBytes gates the minified heuristic: below this size a file
	// produces too few chunks to matter, so we never flag it (avoids
	// false-positives on short single-line configs).
	minifiedMinBytes = 8 * 1024

	// minifiedAvgLineLen is the average bytes-per-line above which a file is
	// treated as machine-emitted. Hand-written source averages tens of bytes
	// per line; minified/bundled output packs thousands onto one line.
	minifiedAvgLineLen = 400
)

// ChunkGuardSettings are the resolved chunk-density guard thresholds for one
// project.
type ChunkGuardSettings struct {
	// MaxChunksPerFile caps how many chunks a single file may contribute.
	// A value <= 0 disables the cap.
	MaxChunksPerFile int
	// SkipMinified enables the minified/bundled-file heuristic (LooksMinified).
	SkipMinified bool
}

// guardConfigFile is the subset of .dex/config.yml the guard reads. Pointer
// fields distinguish "unset" (inherit the default) from an explicit value —
// notably skip_minified: false, which must override the default-on. Unknown
// keys are ignored by yaml.Unmarshal, so this coexists with the ignore
// package reading the same file.
type guardConfigFile struct {
	Index struct {
		MaxChunksPerFile *int  `yaml:"max_chunks_per_file"`
		SkipMinified     *bool `yaml:"skip_minified"`
	} `yaml:"index"`
}

// LoadChunkGuard resolves the guard settings for the project rooted at root
// from .dex/config.yml, applying defaults for any unset key. A missing or
// malformed config yields the defaults: the file's syntax is already
// validated upstream by ignore.New (same file), so a parse error here is not
// re-surfaced — the caller has already failed on it.
func LoadChunkGuard(root string) ChunkGuardSettings {
	s := ChunkGuardSettings{
		MaxChunksPerFile: DefaultMaxChunksPerFile,
		SkipMinified:     true,
	}
	raw, err := os.ReadFile(filepath.Join(root, ".dex", "config.yml"))
	if err != nil {
		return s
	}
	var f guardConfigFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return s
	}
	if f.Index.MaxChunksPerFile != nil {
		s.MaxChunksPerFile = *f.Index.MaxChunksPerFile
	}
	if f.Index.SkipMinified != nil {
		s.SkipMinified = *f.Index.SkipMinified
	}
	return s
}

// LooksMinified reports whether data looks machine-emitted (a minified or
// bundled file) via a cheap single-pass average-line-length heuristic. It
// assumes data is already known to be text (binary files are filtered
// earlier). Files below minifiedMinBytes are never flagged.
func LooksMinified(data []byte) bool {
	if len(data) < minifiedMinBytes {
		return false
	}
	lines := bytes.Count(data, []byte{'\n'}) + 1
	return len(data)/lines >= minifiedAvgLineLen
}
