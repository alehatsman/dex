package index

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/veccache"
)

// countingEmbedder hash-embeds deterministically and counts embedded inputs,
// so a test can assert how many live embeds a run performed.
type countingEmbedder struct {
	n atomic.Int64
}

func (c *countingEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	c.n.Add(int64(len(inputs)))
	out := make([][]float32, len(inputs))
	for i, s := range inputs {
		v := make([]float32, 16)
		for _, b := range []byte(s) {
			v[int(b)%16]++
		}
		out[i] = v
	}
	return out, nil
}
func (c *countingEmbedder) Health(context.Context) error { return nil }
func (c *countingEmbedder) Endpoint() string             { return "counting" }
func (c *countingEmbedder) ModelName() string            { return "fake@16" }
func (c *countingEmbedder) BatchSize() int               { return 8 }
func (c *countingEmbedder) EmbedConcurrency() int        { return 1 }

// TestVecCacheReuseAcrossReindex proves #121: a second index pass over
// byte-identical content, backed by the same content-addressed vector cache
// but a FRESH store (the reindex-drop scenario), performs zero live embeds —
// the vectors are served from the surviving cache instead of recomputed.
func TestVecCacheReuseAcrossReindex(t *testing.T) {
	ctx := context.Background()
	projDir := t.TempDir()
	writeIndexAll(t, projDir)
	writeFile(t, filepath.Join(projDir, "alpha.go"), `package main

func Alpha() string { return "alpha" }

func Beta() string { return "beta" }
`)
	writeFile(t, filepath.Join(projDir, "README.md"),
		"# Project\n\nSome indexable prose across a couple of lines.\n\nMore text.\n")

	ig, err := ignore.New(projDir)
	if err != nil {
		t.Fatal(err)
	}
	// The cache lives at a stable path that outlives each run's store — exactly
	// how veccache.db survives the reindex sweep in the real flow.
	cachePath := filepath.Join(t.TempDir(), veccache.FileName)

	// runOnce indexes into a FRESH store (simulating the reindex drop) with a
	// caching embedder backed by the shared on-disk cache, returning how many
	// inputs the backend actually embedded.
	runOnce := func() int64 {
		p, err := proj.Resolve(projDir, t.TempDir())
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

		vc, err := veccache.Open(cachePath, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer vc.Close()

		counter := &countingEmbedder{}
		ix := New(p, st, embed.WithCache(counter, vc), ig, Options{})
		if err := ix.Run(ctx); err != nil {
			t.Fatal(err)
		}
		return counter.n.Load()
	}

	if first := runOnce(); first == 0 {
		t.Fatal("cold run embedded 0 inputs — test setup indexed nothing")
	}
	if second := runOnce(); second != 0 {
		t.Fatalf("reindex embedded %d inputs, want 0 (all served from the vector cache)", second)
	}
}
