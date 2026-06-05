package heatmap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecordAndTopFiles(t *testing.T) {
	hm := &Heatmap{entries: make(map[string]*Entry)}
	hm.RecordAccess("internal/mcp/server.go", 1000, 200)
	hm.RecordAccess("internal/mcp/server.go", 800, 150)
	hm.RecordAccess("internal/store/store.go", 500, 50)

	top := hm.TopFiles(5)
	if len(top) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(top))
	}
	if top[0].Path != "internal/mcp/server.go" {
		t.Errorf("expected server.go first, got %q", top[0].Path)
	}
	if top[0].AccessCount != 2 {
		t.Errorf("expected AccessCount=2, got %d", top[0].AccessCount)
	}
	if top[0].OriginalTotal != 1800 {
		t.Errorf("expected OriginalTotal=1800, got %d", top[0].OriginalTotal)
	}
	if top[0].TotalSaved != 350 {
		t.Errorf("expected TotalSaved=350, got %d", top[0].TotalSaved)
	}
}

func TestColdFiles(t *testing.T) {
	hm := &Heatmap{entries: make(map[string]*Entry)}
	hm.RecordAccess("a.go", 100, 0)

	all := []string{"a.go", "b.go", "c.go"}
	cold := hm.ColdFiles(all, 0)
	if len(cold) != 2 {
		t.Fatalf("expected 2 cold files, got %d", len(cold))
	}
	if cold[0] != "b.go" || cold[1] != "c.go" {
		t.Errorf("unexpected cold files: %v", cold)
	}
}

func TestColdFiles_Limit(t *testing.T) {
	hm := &Heatmap{entries: make(map[string]*Entry)}
	all := []string{"a.go", "b.go", "c.go"}
	cold := hm.ColdFiles(all, 1)
	if len(cold) != 1 {
		t.Errorf("expected limit=1, got %d", len(cold))
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	hm := &Heatmap{entries: make(map[string]*Entry)}
	hm.RecordAccess("internal/mcp/server.go", 1000, 200)
	hm.RecordAccess("internal/store/store.go", 500, 50)

	if err := hm.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, heatmapFile)); err != nil {
		t.Fatalf("heatmap.json not created: %v", err)
	}

	hm2 := Load(dir)
	if len(hm2.entries) != 2 {
		t.Fatalf("expected 2 entries after load, got %d", len(hm2.entries))
	}
	e := hm2.entries["internal/mcp/server.go"]
	if e == nil || e.AccessCount != 1 || e.TotalSaved != 200 {
		t.Errorf("unexpected entry: %+v", e)
	}
}

func TestLoad_Missing(t *testing.T) {
	hm := Load(t.TempDir())
	if len(hm.entries) != 0 {
		t.Error("missing file should return empty heatmap")
	}
}

func TestLoad_Corrupt(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, heatmapFile), []byte("not json"), 0o644)
	hm := Load(dir)
	if len(hm.entries) != 0 {
		t.Error("corrupt file should return empty heatmap")
	}
}

func TestDirectorySummary(t *testing.T) {
	hm := &Heatmap{entries: make(map[string]*Entry)}
	for i := 0; i < 5; i++ {
		hm.RecordAccess("internal/mcp/server.go", 100, 10)
	}
	for i := 0; i < 2; i++ {
		hm.RecordAccess("internal/store/store.go", 100, 10)
	}
	hm.RecordAccess("cmd/dex/main.go", 100, 10)

	dirs := hm.DirectorySummary()
	if len(dirs) == 0 {
		t.Fatal("expected non-empty directory summary")
	}
	if dirs[0].Dir != "internal/mcp/" {
		t.Errorf("expected internal/mcp/ first, got %q", dirs[0].Dir)
	}
	if dirs[0].Accesses != 5 {
		t.Errorf("expected 5 accesses, got %d", dirs[0].Accesses)
	}
}

func TestHeatIcon(t *testing.T) {
	if heatIcon(10) != "🔥" {
		t.Error("expected 🔥 for 10+")
	}
	if heatIcon(5) != "◎" {
		t.Error("expected ◎ for 2-9")
	}
	if heatIcon(1) != "○" {
		t.Error("expected ○ for 1")
	}
}

func TestFormat_Empty(t *testing.T) {
	hm := &Heatmap{entries: make(map[string]*Entry)}
	out := hm.Format(10)
	if out != "No files accessed yet." {
		t.Errorf("unexpected empty output: %q", out)
	}
}

func TestFormat_NonEmpty(t *testing.T) {
	hm := &Heatmap{entries: make(map[string]*Entry)}
	for i := 0; i < 3; i++ {
		hm.RecordAccess("internal/mcp/server.go", 500, 100)
	}
	out := hm.Format(10)
	if out == "" {
		t.Error("format should return non-empty string")
	}
	if !contains(out, "server.go") {
		t.Error("format should contain file path")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
