package retrieve

import "testing"

func TestChunkImportance_Empty(t *testing.T) {
	if got := ChunkImportance(""); got != 0 {
		t.Errorf("empty content: want 0, got %v", got)
	}
}

func TestChunkImportance_Average(t *testing.T) {
	// Single high-signal line: func def = 1.5, averaged over 1 line.
	if got := ChunkImportance("func foo() {"); got != 1.5 {
		t.Errorf("func line: want 1.5, got %v", got)
	}
	// Error line dominates: 2.0.
	if got := ChunkImportance("return fmt.Errorf(\"boom\")"); got != 2.0 {
		t.Errorf("error line: want 2.0, got %v", got)
	}
}

func TestChunkImportance_LongerNotHigher(t *testing.T) {
	// Averaging means padding a chunk with low-signal lines lowers its score,
	// so a long boilerplate chunk can't outrank a short high-signal one.
	short := ChunkImportance("panic(\"x\")")
	long := ChunkImportance("panic(\"x\")\nx := 1\ny := 2\nz := 3")
	if !(short > long) {
		t.Errorf("short high-signal (%v) should outrank padded long (%v)", short, long)
	}
}

func TestChunkImportance_LandmarkOrdering(t *testing.T) {
	// errors > imports > defs > plain.
	err := ChunkImportance("error: boom")
	imp := ChunkImportance("import \"fmt\"")
	def := ChunkImportance("type Foo struct {")
	plain := ChunkImportance("x := compute()")
	if !(err > imp && imp > def && def > plain) {
		t.Errorf("landmark ordering violated: err=%v imp=%v def=%v plain=%v", err, imp, def, plain)
	}
}
