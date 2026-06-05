// Package heatmap tracks per-file access frequency and compression savings
// for a project, persisted to <cacheDir>/heatmap.json. Used by file_view
// to accumulate cumulative read history across server restarts.
package heatmap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const heatmapFile = "heatmap.json"

// Entry records cumulative access statistics for a single file.
type Entry struct {
	Path          string `json:"path"`
	AccessCount   int    `json:"access_count"`
	OriginalTotal int    `json:"original_total"` // total original tokens seen
	TotalSaved    int    `json:"total_saved"`     // total tokens saved via compression
	LastAccessed  string `json:"last_accessed"`   // RFC3339
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

// TopFiles returns the N most-accessed files, sorted by access count desc.
func (hm *Heatmap) TopFiles(n int) []*Entry {
	entries := make([]*Entry, 0, len(hm.entries))
	for _, e := range hm.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].AccessCount != entries[j].AccessCount {
			return entries[i].AccessCount > entries[j].AccessCount
		}
		return entries[i].Path < entries[j].Path
	})
	if n > 0 && len(entries) > n {
		entries = entries[:n]
	}
	return entries
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

// DirSummary returns an aggregated view grouped by top-level directory.
// Each entry contains the directory prefix, total accesses, and file count.
type DirSummary struct {
	Dir         string
	Accesses    int
	Files       int
	TotalSaved  int
}

// DirectorySummary aggregates access counts by the first two path components.
func (hm *Heatmap) DirectorySummary() []DirSummary {
	byDir := make(map[string]*DirSummary)
	for _, e := range hm.entries {
		dir := dirOf(e.Path)
		ds, ok := byDir[dir]
		if !ok {
			ds = &DirSummary{Dir: dir}
			byDir[dir] = ds
		}
		ds.Accesses += e.AccessCount
		ds.TotalSaved += e.TotalSaved
		ds.Files++
	}
	result := make([]DirSummary, 0, len(byDir))
	for _, ds := range byDir {
		result = append(result, *ds)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Accesses != result[j].Accesses {
			return result[i].Accesses > result[j].Accesses
		}
		return result[i].Dir < result[j].Dir
	})
	return result
}

// Format renders the heatmap summary as a markdown string suitable for
// returning in an MCP tool response.
func (hm *Heatmap) Format(topN int) string {
	if len(hm.entries) == 0 {
		return "No files accessed yet."
	}
	var sb strings.Builder

	dirs := hm.DirectorySummary()
	top := hm.TopFiles(topN)

	totalAccesses := 0
	totalSaved := 0
	for _, e := range hm.entries {
		totalAccesses += e.AccessCount
		totalSaved += e.TotalSaved
	}

	sb.WriteString("## File Access Heatmap\n\n")
	if totalSaved > 0 {
		sb.WriteString("| Metric | Value |\n|---|---|\n")
		sb.WriteString("| Files tracked | " + itoa(len(hm.entries)) + " |\n")
		sb.WriteString("| Total accesses | " + itoa(totalAccesses) + " |\n")
		sb.WriteString("| Total tokens saved | " + itoa(totalSaved) + " |\n\n")
	} else {
		sb.WriteString("Files tracked: " + itoa(len(hm.entries)) + "  |  Total accesses: " + itoa(totalAccesses) + "\n\n")
	}

	if len(dirs) > 0 {
		sb.WriteString("### Hot Directories\n\n")
		shown := dirs
		if len(shown) > 10 {
			shown = shown[:10]
		}
		for _, d := range shown {
			icon := heatIcon(d.Accesses)
			sb.WriteString(icon + " `" + d.Dir + "` — " + itoa(d.Accesses) + " accesses, " + itoa(d.Files) + " files\n")
		}
		sb.WriteByte('\n')
	}

	if len(top) > 0 {
		sb.WriteString("### Top Files\n\n")
		for _, e := range top {
			icon := heatIcon(e.AccessCount)
			line := icon + " `" + e.Path + "` — " + itoa(e.AccessCount) + "x"
			if e.TotalSaved > 0 {
				line += ", saved ~" + itoa(e.TotalSaved) + " tokens"
			}
			sb.WriteString(line + "\n")
		}
	}

	return sb.String()
}

func heatIcon(accesses int) string {
	switch {
	case accesses >= 10:
		return "🔥"
	case accesses >= 2:
		return "◎"
	default:
		return "○"
	}
}

// dirOf returns the directory portion of a relative path, capped at the
// first two components (e.g. "internal/mcp/server.go" → "internal/mcp/").
func dirOf(path string) string {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) <= 1 {
		return "./"
	}
	return strings.Join(parts[:len(parts)-1], "/") + "/"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
