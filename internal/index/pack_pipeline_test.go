package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// TestDenseFilePackedNotDropped: a file whose structural chunk count exceeds the
// cap is coarsened by PackDense and stays in the index (greppable/searchable),
// while a small file is indexed unchanged. Regression for #131 — the old
// behaviour dropped the dense file entirely.
func TestDenseFilePackedNotDropped(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	projDir := t.TempDir()
	cacheDir := t.TempDir()
	cfgDir := filepath.Join(projDir, ".dex")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Low cap so a modest fixture trips packing without generating 500+ decls.
	cfg := "index:\n  include: [\"*\"]\n  max_chunks_per_file: 40\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	const denseDecls = 300
	var b strings.Builder
	for i := 0; i < denseDecls; i++ {
		fmt.Fprintf(&b, "export const token%d = 'TOKEN_%d'\n", i, i)
	}
	writeFile(t, filepath.Join(projDir, "dense.ts"), b.String())
	writeFile(t, filepath.Join(projDir, "small.ts"), "export function keep() { return 1 }\n")

	ctx := context.Background()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ig, err := ignore.New(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	em := embed.New(srv.URL, "fake", 8, 10*time.Second)
	if err := New(p, st, em, ig, Options{}).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	tree, err := st.FileTree(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range tree {
		counts[e.Path] = e.Chunks
	}
	// dense.ts must be present (not dropped) but coarsened well below its
	// structural declaration count.
	dc, ok := counts["dense.ts"]
	if !ok || dc == 0 {
		t.Fatalf("dense.ts absent from index (dropped?): tree=%v", counts)
	}
	if dc >= denseDecls {
		t.Errorf("dense.ts chunks=%d, expected coarsened below %d", dc, denseDecls)
	}
	if dc > 40 {
		t.Errorf("dense.ts chunks=%d, expected under the cap after packing", dc)
	}
	// small.ts indexed normally.
	if counts["small.ts"] == 0 {
		t.Errorf("small.ts absent from index: tree=%v", counts)
	}
	t.Logf("dense.ts packed to %d chunks (from %d declarations)", dc, denseDecls)
}

// TestDisablePackingKeepsAllChunks: max_chunks_per_file <= 0 disables packing,
// so a dense file keeps one chunk per declaration.
func TestDisablePackingKeepsAllChunks(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	projDir := t.TempDir()
	cacheDir := t.TempDir()
	cfgDir := filepath.Join(projDir, ".dex")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "index:\n  include: [\"*\"]\n  max_chunks_per_file: -1\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	const denseDecls = 300
	var b strings.Builder
	for i := 0; i < denseDecls; i++ {
		fmt.Fprintf(&b, "export const token%d = 'TOKEN_%d'\n", i, i)
	}
	writeFile(t, filepath.Join(projDir, "dense.ts"), b.String())

	ctx := context.Background()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ig, err := ignore.New(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	em := embed.New(srv.URL, "fake", 8, 10*time.Second)
	if err := New(p, st, em, ig, Options{}).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tree, err := st.FileTree(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tree {
		if e.Path == "dense.ts" {
			if e.Chunks < denseDecls {
				t.Errorf("packing disabled but dense.ts has %d chunks, want >= %d", e.Chunks, denseDecls)
			}
			return
		}
	}
	t.Fatal("dense.ts absent from index")
}
