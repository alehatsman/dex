package proxy

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBudgetLog_TurnAndCompact(t *testing.T) {
	dir := t.TempDir()
	bl, err := NewBudgetLog(dir, "test-session")
	if err != nil {
		t.Fatalf("NewBudgetLog: %v", err)
	}
	t.Cleanup(func() { _ = bl.Close() })

	if bl.Window() != 1 {
		t.Fatalf("initial window want 1 got %d", bl.Window())
	}

	// Two turns in window 1.
	u1 := ProviderUsage{InputTokens: 1200, OutputTokens: 450, CacheReadTokens: 8400}
	u2 := ProviderUsage{InputTokens: 300, OutputTokens: 120}
	if err := bl.AppendTurn(u1); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	if err := bl.AppendTurn(u2); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}

	// Compact closes window 1.
	closed, err := bl.AppendCompact()
	if err != nil {
		t.Fatalf("AppendCompact: %v", err)
	}
	if closed != 1 {
		t.Fatalf("compact closed window want 1 got %d", closed)
	}
	if bl.Window() != 2 {
		t.Fatalf("window after compact want 2 got %d", bl.Window())
	}

	// One turn in window 2.
	u3 := ProviderUsage{InputTokens: 500, OutputTokens: 200, CacheWriteTokens: 2400}
	if err := bl.AppendTurn(u3); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}

	// Read and validate the JSONL file.
	path := filepath.Join(dir, "test-session", "budget.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	type genericEntry struct {
		TS         string  `json:"ts"`
		Event      string  `json:"event"`
		Input      int64   `json:"input"`
		Output     int64   `json:"output"`
		CacheRead  int64   `json:"cache_read"`
		CacheWrite int64   `json:"cache_write"`
		Window     int     `json:"window"`
		CostUSD    float64 `json:"cost_usd"`
	}

	var entries []genericEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e genericEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("unmarshal line: %v", err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(entries) != 4 {
		t.Fatalf("want 4 entries got %d", len(entries))
	}

	// turn 1 — window 1
	if entries[0].Event != "turn" || entries[0].Window != 1 ||
		entries[0].Input != 1200 || entries[0].Output != 450 || entries[0].CacheRead != 8400 {
		t.Errorf("entry[0] mismatch: %+v", entries[0])
	}
	// turn 2 — window 1
	if entries[1].Event != "turn" || entries[1].Window != 1 {
		t.Errorf("entry[1] mismatch: %+v", entries[1])
	}
	// compact — window 1
	if entries[2].Event != "compact" || entries[2].Window != 1 {
		t.Errorf("entry[2] mismatch: %+v", entries[2])
	}
	// turn 3 — window 2
	if entries[3].Event != "turn" || entries[3].Window != 2 ||
		entries[3].CacheWrite != 2400 {
		t.Errorf("entry[3] mismatch: %+v", entries[3])
	}

	// ts must be non-empty RFC3339
	for i, e := range entries {
		if e.TS == "" {
			t.Errorf("entry[%d] missing ts", i)
		}
	}
}

func TestBudgetLog_LogPath(t *testing.T) {
	dir := t.TempDir()
	bl, err := NewBudgetLog(dir, "s1")
	if err != nil {
		t.Fatalf("NewBudgetLog: %v", err)
	}
	defer bl.Close()

	want := filepath.Join(dir, "s1", "budget.jsonl")
	if bl.LogPath() != want {
		t.Errorf("LogPath want %s got %s", want, bl.LogPath())
	}
}

func TestBudgetLog_MultipleCompacts(t *testing.T) {
	dir := t.TempDir()
	bl, err := NewBudgetLog(dir, "mc")
	if err != nil {
		t.Fatalf("NewBudgetLog: %v", err)
	}
	defer bl.Close()

	for i := 0; i < 3; i++ {
		_ = bl.AppendTurn(ProviderUsage{InputTokens: 100})
		closed, _ := bl.AppendCompact()
		if closed != i+1 {
			t.Errorf("compact %d: closed window want %d got %d", i, i+1, closed)
		}
		if bl.Window() != i+2 {
			t.Errorf("after compact %d: window want %d got %d", i, i+2, bl.Window())
		}
	}
}
