// Package heatmap tracks per-file access frequency and compression savings
// for a project, persisted to <cacheDir>/heatmap.json. Used by file_view
// to accumulate cumulative read history across server restarts.
package heatmap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const heatmapFile = "heatmap.json"

// Entry records cumulative access statistics for a single file.
type Entry struct {
	Path          string `json:"path"`
	AccessCount   int    `json:"access_count"`
	OriginalTotal int    `json:"original_total"` // total original tokens seen
	TotalSaved    int    `json:"total_saved"`    // total tokens saved via compression
	LastAccessed  string `json:"last_accessed"`  // RFC3339
}

// Heatmap holds access statistics for all files in a project.
type Heatmap struct {
	entries map[string]*Entry
}

type heatmapJSON struct {
	Entries []*Entry `json:"entries"`
}

// Load reads the heatmap from cacheDir/heatmap.json. Returns an empty
// Heatmap (not an error) when the file does not exist or is corrupt.
func Load(cacheDir string) *Heatmap {
	hm := &Heatmap{entries: make(map[string]*Entry)}
	data, err := os.ReadFile(filepath.Join(cacheDir, heatmapFile))
	if err != nil {
		return hm
	}
	var hj heatmapJSON
	if err := json.Unmarshal(data, &hj); err != nil {
		return hm
	}
	for _, e := range hj.Entries {
		hm.entries[e.Path] = e
	}
	return hm
}

// RecordAccess records one file_view call for path. originalTokens is the
// estimated token count before compression; savedTokens is original minus
// compressed (0 if no compression was applied).
func (hm *Heatmap) RecordAccess(path string, originalTokens, savedTokens int) {
	e, ok := hm.entries[path]
	if !ok {
		e = &Entry{Path: path}
		hm.entries[path] = e
	}
	e.AccessCount++
	e.OriginalTotal += originalTokens
	e.TotalSaved += savedTokens
	e.LastAccessed = time.Now().UTC().Format(time.RFC3339)
}

// Save writes the heatmap to cacheDir/heatmap.json via atomic rename.
// Best-effort: callers should not fail on Save errors.
func (hm *Heatmap) Save(cacheDir string) error {
	entries := make([]*Entry, 0, len(hm.entries))
	for _, e := range hm.entries {
		entries = append(entries, e)
	}
	// Sort by path for deterministic output.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	hj := heatmapJSON{Entries: entries}
	data, err := json.MarshalIndent(hj, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(cacheDir, heatmapFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(cacheDir, heatmapFile))
}

// ColdFiles returns files from allFiles that have never been accessed,
// up to n results. allFiles should be relative paths (matching Entry.Path).
func (hm *Heatmap) ColdFiles(allFiles []string, n int) []string {
	var cold []string
	for _, f := range allFiles {
		if _, ok := hm.entries[f]; !ok {
			cold = append(cold, f)
			if n > 0 && len(cold) >= n {
				break
			}
		}
	}
	return cold
}
