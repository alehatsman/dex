package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	diaryMaxEntries = 100
	diaryRecallN    = 10
)

// DiaryEntry is one structured log entry in an agent's diary.
type DiaryEntry struct {
	ID        int    `json:"id"`
	Category  string `json:"category"` // discovery|decision|blocker|progress|insight
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"` // RFC3339
}

// diaryFile is the on-disk shape of a diary JSON file.
type diaryFile struct {
	AgentID string       `json:"agent_id"`
	Entries []DiaryEntry `json:"entries"`
}

// DiaryListEntry is a summary row returned by the diaries action.
type DiaryListEntry struct {
	AgentID     string `json:"agent_id"`
	EntryCount  int    `json:"entry_count"`
	LastUpdated string `json:"last_updated"` // RFC3339 of newest entry, or file mtime
}

var diaryMu sync.Mutex // coarse lock; diary I/O is rare

// diaryDir returns the per-agent diaries directory under indexDir.
func diaryDir(indexDir string) string {
	return filepath.Join(indexDir, "agents", "diaries")
}

// diaryPath returns the path to a specific agent's diary file.
func diaryPath(indexDir, agentID string) string {
	// Sanitize agentID so it's safe as a filename.
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			return '_'
		}
		return r
	}, agentID)
	return filepath.Join(diaryDir(indexDir), safe+".json")
}

// DiaryAppend appends a new entry to agentID's diary.
// Category must be one of: discovery|decision|blocker|progress|insight.
// The file is capped at diaryMaxEntries (oldest evicted).
func DiaryAppend(indexDir, agentID, category, content string) (DiaryEntry, error) {
	diaryMu.Lock()
	defer diaryMu.Unlock()

	path := diaryPath(indexDir, agentID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return DiaryEntry{}, err
	}

	df, _ := readDiaryFile(path)
	if df.AgentID == "" {
		df.AgentID = agentID
	}

	nextID := 1
	if len(df.Entries) > 0 {
		nextID = df.Entries[len(df.Entries)-1].ID + 1
	}
	entry := DiaryEntry{
		ID:        nextID,
		Category:  category,
		Content:   content,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	df.Entries = append(df.Entries, entry)

	// Evict oldest when over cap.
	if len(df.Entries) > diaryMaxEntries {
		df.Entries = df.Entries[len(df.Entries)-diaryMaxEntries:]
	}

	if err := writeDiaryFile(path, df); err != nil {
		return DiaryEntry{}, err
	}
	return entry, nil
}

// DiaryRecall returns the last n entries from agentID's diary, newest first.
func DiaryRecall(indexDir, agentID string, n int) ([]DiaryEntry, error) {
	diaryMu.Lock()
	defer diaryMu.Unlock()

	if n <= 0 {
		n = diaryRecallN
	}
	df, err := readDiaryFile(diaryPath(indexDir, agentID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	entries := df.Entries
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	// Reverse to newest-first.
	out := make([]DiaryEntry, len(entries))
	for i, e := range entries {
		out[len(entries)-1-i] = e
	}
	return out, nil
}

// DiaryList scans the diary directory and returns a summary for each agent.
func DiaryList(indexDir string) ([]DiaryListEntry, error) {
	diaryMu.Lock()
	defer diaryMu.Unlock()

	dir := diaryDir(indexDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []DiaryListEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		df, ferr := readDiaryFile(path)
		if ferr != nil {
			continue
		}
		lastUpdated := ""
		if len(df.Entries) > 0 {
			lastUpdated = df.Entries[len(df.Entries)-1].CreatedAt
		} else if info, serr := e.Info(); serr == nil {
			lastUpdated = info.ModTime().UTC().Format(time.RFC3339)
		}
		out = append(out, DiaryListEntry{
			AgentID:     df.AgentID,
			EntryCount:  len(df.Entries),
			LastUpdated: lastUpdated,
		})
	}
	return out, nil
}

func readDiaryFile(path string) (diaryFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return diaryFile{}, err
	}
	var df diaryFile
	if err := json.Unmarshal(data, &df); err != nil {
		return diaryFile{}, err
	}
	return df, nil
}

func writeDiaryFile(path string, df diaryFile) error {
	data, err := json.MarshalIndent(df, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
