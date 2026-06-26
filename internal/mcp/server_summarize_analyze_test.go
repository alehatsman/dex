package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// analyzeFixture is large enough that compression has something to save and the
// structural modes beat full.
const analyzeFixture = `package sample

import (
	"fmt"
	"strings"
)

// Greeter builds greetings.
type Greeter struct {
	prefix string
}

// Greet returns a greeting for name.
func (g *Greeter) Greet(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "world"
	}
	return fmt.Sprintf("%s %s", g.prefix, name)
}

// Farewell returns a farewell for name.
func (g *Greeter) Farewell(name string) string {
	if name == "" {
		return "goodbye"
	}
	return fmt.Sprintf("goodbye, %s", name)
}

// New builds a Greeter.
func New(prefix string) *Greeter { return &Greeter{prefix: prefix} }
`

func modeIn(a *ReadAnalysis, mode string) (ModeCost, bool) {
	if a == nil {
		return ModeCost{}, false
	}
	return modeCost(*a, mode)
}

// seedGraph populates graph_nodes for sample.go so SymbolsByFile (and the
// structural read modes) have data without running the full graph extractor.
func seedAnalyzeGraph(t *testing.T, projDir, cacheDir string) {
	t.Helper()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Skip("fts5 not available:", err)
	}
	defer st.Close()
	rows := []store.GraphNodeRow{
		{ID: "n1", Kind: "type", Name: "Greeter", QualifiedName: "Greeter", PackagePath: "sample", FilePath: "sample.go", StartLine: 9, EndLine: 11},
		{ID: "n2", Kind: "method", Name: "Greet", QualifiedName: "(*Greeter).Greet", PackagePath: "sample", FilePath: "sample.go", StartLine: 14, EndLine: 20},
		{ID: "n3", Kind: "method", Name: "Farewell", QualifiedName: "(*Greeter).Farewell", PackagePath: "sample", FilePath: "sample.go", StartLine: 23, EndLine: 28},
		{ID: "n4", Kind: "function", Name: "New", QualifiedName: "New", PackagePath: "sample", FilePath: "sample.go", StartLine: 31, EndLine: 31},
	}
	if err := st.GraphUpsertNodes(context.Background(), rows, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyzeModeIndexed(t *testing.T) {
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "sample.go"), analyzeFixture)
	seedAnalyzeGraph(t, projDir, cacheDir)

	s := &Server{IndexDir: cacheDir}
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		Path:        "sample.go",
		ProjectRoot: projDir,
		Mode:        "analyze",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "ok" || out.Analysis == nil {
		t.Fatalf("status=%q analysis=%v hint=%q", out.Status, out.Analysis, out.Hint)
	}
	a := out.Analysis
	if out.Content != "" {
		t.Errorf("analyze must NOT return file content, got %d bytes", len(out.Content))
	}
	if !a.Indexed {
		t.Fatalf("expected Indexed=true; modes=%+v", a.Modes)
	}

	full, ok := modeIn(a, "full")
	if !ok || full.Tokens <= 0 {
		t.Fatalf("missing/empty full mode: %+v", a.Modes)
	}
	// signatures and map render declarations/imports only → cheaper than full.
	for _, m := range []string{"signatures", "map", "skeleton"} {
		if _, ok := modeIn(a, m); !ok {
			t.Errorf("expected mode %q in analysis", m)
		}
	}
	sig, _ := modeIn(a, "signatures")
	if sig.Tokens >= full.Tokens || sig.SavedPct <= 0 {
		t.Errorf("signatures (%d tok, %d%%) should be cheaper than full (%d)", sig.Tokens, sig.SavedPct, full.Tokens)
	}
	mp, _ := modeIn(a, "map")
	if mp.Tokens > sig.Tokens {
		t.Errorf("map (%d) should not exceed signatures (%d)", mp.Tokens, sig.Tokens)
	}
	if a.MeanBitsPerChar <= 0 {
		t.Error("expected a positive mean_bits_per_char")
	}
	if a.Recommendation == "" {
		t.Error("expected a recommendation")
	}
}

func TestAnalyzeModeNoIndexDegrades(t *testing.T) {
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "sample.go"), analyzeFixture)

	// No graph seeded — analyze must degrade, not error.
	s := &Server{IndexDir: cacheDir}
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		Path:        "sample.go",
		ProjectRoot: projDir,
		Mode:        "analyze",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "ok" || out.Analysis == nil {
		t.Fatalf("status=%q analysis=%v", out.Status, out.Analysis)
	}
	a := out.Analysis
	if a.Indexed {
		t.Error("expected Indexed=false with no graph")
	}
	if _, ok := modeIn(a, "full"); !ok {
		t.Error("full must be present even without an index")
	}
	if _, ok := modeIn(a, "aggressive"); !ok {
		t.Error("aggressive must be present even without an index")
	}
	if _, ok := modeIn(a, "signatures"); ok {
		t.Error("signatures must be absent without an index")
	}
	if a.Recommendation == "" {
		t.Error("expected a recommendation even when degraded")
	}
}

// TestAnalyzeEmitsExpandableHandle covers #620: mode=analyze returns a
// whole-file handle, and read(handle=…, mode=…) expands it.
func TestAnalyzeEmitsExpandableHandle(t *testing.T) {
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "sample.go"), analyzeFixture)
	seedAnalyzeGraph(t, projDir, cacheDir)

	s := &Server{IndexDir: cacheDir}
	ctx := context.Background()
	_, out, err := s.summarize(ctx, nil, SummarizeInput{Path: "sample.go", ProjectRoot: projDir, Mode: "analyze"})
	if err != nil || out.Analysis == nil {
		t.Fatalf("analyze: err=%v analysis=%v", err, out.Analysis)
	}
	h := out.Analysis.Handle
	if h == "" {
		t.Fatal("analyze must emit a handle (#620)")
	}
	// The handle decodes to the file with a whole-file range.
	path, start, end, ok := DecodeHandle(h)
	if !ok || path != "sample.go" || start != 1 || end < 1 {
		t.Fatalf("handle decode = (%q,%d,%d,%v), want sample.go/1/>=1/true", path, start, end, ok)
	}

	// Expanding the handle with an explicit mode resolves to the file and
	// renders that view (mode is honored, not overridden to lines:N-M).
	_, exp, err := s.summarize(ctx, nil, SummarizeInput{ProjectRoot: projDir, Handle: h, Mode: "signatures"})
	if err != nil || exp.Status != "ok" {
		t.Fatalf("expand handle: status=%q err=%v hint=%q", exp.Status, err, exp.Hint)
	}
	if exp.Path != "sample.go" {
		t.Errorf("expanded path = %q, want sample.go", exp.Path)
	}
	if !strings.Contains(exp.Content, "Greet") {
		t.Errorf("signatures expansion should name the symbols, got:\n%s", exp.Content)
	}

	// Expanding with no mode reads the whole file (lines:1-N default).
	_, full, err := s.summarize(ctx, nil, SummarizeInput{ProjectRoot: projDir, Handle: h})
	if err != nil || full.Status != "ok" {
		t.Fatalf("expand handle (no mode): status=%q err=%v", full.Status, err)
	}
	if !strings.Contains(full.Content, "package sample") {
		t.Errorf("whole-file expansion should contain the source, got:\n%s", full.Content)
	}
}

func TestRecommendReadMode(t *testing.T) {
	// Small file → full.
	if mode, reason := recommendReadMode(ReadAnalysis{}, 120); mode != "full" || !strings.Contains(reason, "small") {
		t.Errorf("small file: got mode=%q reason=%q", mode, reason)
	}
	// Large + indexed → signatures.
	indexed := ReadAnalysis{Modes: []ModeCost{
		{Mode: "full", Tokens: 5000},
		{Mode: "signatures", Tokens: 800, SavedPct: 84},
	}}
	if mode, _ := recommendReadMode(indexed, 5000); mode != "signatures" {
		t.Errorf("large indexed: want signatures, got %q", mode)
	}
	// Large + unindexed but compressible → aggressive.
	unindexed := ReadAnalysis{Modes: []ModeCost{
		{Mode: "full", Tokens: 5000},
		{Mode: "aggressive", Tokens: 2500, SavedPct: 50, Lossy: true},
	}}
	if mode, _ := recommendReadMode(unindexed, 5000); mode != "aggressive" {
		t.Errorf("large unindexed: want aggressive, got %q", mode)
	}
}

func TestCompressibilityLabel(t *testing.T) {
	cases := []struct {
		saved int
		want  string
	}{{85, "high"}, {55, "medium"}, {10, "low"}}
	for _, tc := range cases {
		a := ReadAnalysis{Modes: []ModeCost{{Mode: "signatures", SavedPct: tc.saved}}}
		if got := compressibilityLabel(a); got != tc.want {
			t.Errorf("saved=%d%% → %q, want %q", tc.saved, got, tc.want)
		}
	}
}

func TestMeanBitsPerChar(t *testing.T) {
	if got := meanBitsPerChar(""); got != 0 {
		t.Errorf("empty → %v, want 0", got)
	}
	// A single repeated char has zero entropy.
	if got := meanBitsPerChar("aaaaaa"); got != 0 {
		t.Errorf("uniform → %v, want 0", got)
	}
	// Mixed content has positive entropy.
	if got := meanBitsPerChar("package main\nfunc F() {}\n"); got <= 0 {
		t.Errorf("mixed → %v, want > 0", got)
	}
}
