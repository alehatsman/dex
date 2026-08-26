package heatmap

import (
	"os"
	"path/filepath"
	"testing"
)

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
