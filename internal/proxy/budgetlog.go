package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// BudgetLog writes a per-session JSONL budget event log to disk.
// One turn event is appended per completed API response; compact events are
// appended when the caller signals a PreCompact hook. Thread-safe.
type BudgetLog struct {
	mu     sync.Mutex
	f      *os.File
	path   string
	window int // current window number; increments on each compact event
}

// TurnEntry is one line in the JSONL log for a completed API turn.
type TurnEntry struct {
	TS         string  `json:"ts"`
	Event      string  `json:"event"` // "turn"
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	CostUSD    float64 `json:"cost_usd"`
	Window     int     `json:"window"`
}

// CompactEntry is one line in the JSONL log when a context compaction fires.
type CompactEntry struct {
	TS             string  `json:"ts"`
	Event          string  `json:"event"` // "compact"
	Window         int     `json:"window"`
	WindowTotalUSD float64 `json:"window_total_usd"`
}

// NewBudgetLog opens (or creates) the JSONL log file for sessionID under
// baseDir (e.g. ~/.cache/dex/sessions). The file is opened with O_APPEND so
// it survives proxy restarts.
func NewBudgetLog(baseDir, sessionID string) (*BudgetLog, error) {
	dir := filepath.Join(baseDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "budget.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &BudgetLog{f: f, path: path, window: 1}, nil
}

// LogPath returns the absolute path of the JSONL log file.
func (l *BudgetLog) LogPath() string { return l.path }

// Window returns the current window number.
func (l *BudgetLog) Window() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.window
}

// AppendTurn writes a turn event for a completed API response.
func (l *BudgetLog) AppendTurn(u ProviderUsage) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := TurnEntry{
		TS:         time.Now().UTC().Format(time.RFC3339),
		Event:      "turn",
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadTokens,
		CacheWrite: u.CacheWriteTokens,
		CostUSD:    0,
		Window:     l.window,
	}
	return l.appendLine(entry)
}

// AppendCompact writes a compact event and increments the window counter.
// It returns the window number that was compacted.
func (l *BudgetLog) AppendCompact() (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	closing := l.window
	entry := CompactEntry{
		TS:             time.Now().UTC().Format(time.RFC3339),
		Event:          "compact",
		Window:         closing,
		WindowTotalUSD: 0,
	}
	if err := l.appendLine(entry); err != nil {
		return closing, err
	}
	l.window++
	return closing, nil
}

// Close releases the underlying file handle.
func (l *BudgetLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

func (l *BudgetLog) appendLine(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	_, err = l.f.Write(line)
	return err
}
