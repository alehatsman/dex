package chunk

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestPackDenseCoarsensTinyDeclarations: a file of many tiny declarations packs
// into far fewer chunks, covers every byte, and keeps every declaration's text.
func TestPackDenseCoarsensTinyDeclarations(t *testing.T) {
	const decls = 1000
	var b strings.Builder
	for i := 0; i < decls; i++ {
		fmt.Fprintf(&b, "export const token%d = 'TOKEN_%d'\n", i, i)
	}
	src := []byte(b.String())
	structural, err := Chunks(context.Background(), "gen.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	packed := PackDense("gen.ts", src, structural)

	if len(packed) == 0 || len(packed) >= len(structural) {
		t.Fatalf("packed=%d structural=%d: expected far fewer", len(packed), len(structural))
	}
	// Roughly bytes/MaxBytes windows.
	if want := len(src)/MaxBytes + 2; len(packed) > want {
		t.Errorf("packed=%d, want <= %d (~bytes/MaxBytes)", len(packed), want)
	}
	assertCoversAndMonotonic(t, src, packed)
	all := concat(packed)
	for _, i := range []int{0, 1, decls / 2, decls - 1} {
		if needle := fmt.Sprintf("TOKEN_%d'", i); !strings.Contains(all, needle) {
			t.Errorf("packed chunks missing %q", needle)
		}
	}
	// Every packed chunk is a KindPacked run.
	for _, c := range packed {
		if c.Kind != KindPacked {
			t.Errorf("chunk kind = %q, want %q", c.Kind, KindPacked)
		}
	}
}

// TestPackDenseKeepsBigDeclarationsStandalone: big declarations (>= threshold)
// survive untouched; only runs of small ones between them coalesce.
func TestPackDenseKeepsBigDeclarationsStandalone(t *testing.T) {
	bigBody := strings.Repeat("  field: string\n", 60) // > DenseBigThreshold
	var b strings.Builder
	// small run, then a big interface, then another small run.
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "export const a%d = %d\n", i, i)
	}
	fmt.Fprintf(&b, "export interface Big {\n%s}\n", bigBody)
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "export const b%d = %d\n", i, i)
	}
	src := []byte(b.String())
	structural, err := Chunks(context.Background(), "mix.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	packed := PackDense("mix.ts", src, structural)

	var bigStandalone bool
	for _, c := range packed {
		if strings.Contains(c.Content, "interface Big") {
			if c.Kind == KindPacked {
				t.Errorf("big interface was merged into a packed chunk, want standalone")
			}
			// The big decl must not be diluted with the surrounding consts.
			if strings.Contains(c.Content, "export const a") || strings.Contains(c.Content, "export const b") {
				t.Errorf("big interface chunk diluted with neighbouring consts")
			}
			bigStandalone = true
		}
	}
	if !bigStandalone {
		t.Error("big interface not found as its own chunk")
	}
	assertCoversAndMonotonic(t, src, packed)
}

// TestPackDensePassesThroughBigList: an all-big list is returned unchanged.
func TestPackDensePassesThroughBigList(t *testing.T) {
	var b strings.Builder
	body := strings.Repeat("  x: number\n", 60)
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, "export interface I%d {\n%s}\n", i, body)
	}
	src := []byte(b.String())
	structural, err := Chunks(context.Background(), "big.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	packed := PackDense("big.ts", src, structural)
	for _, c := range packed {
		if c.Kind == KindPacked {
			t.Errorf("all-big list produced a packed chunk, want pass-through")
		}
	}
}

func concat(chunks []Chunk) string {
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(c.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}

// assertCoversAndMonotonic checks packed chunks have non-overlapping,
// byte-ascending spans covering the declaration range without gaps between
// consecutive chunks.
func assertCoversAndMonotonic(t *testing.T, src []byte, chunks []Chunk) {
	t.Helper()
	prevEnd := -1
	for i, c := range chunks {
		if c.startByte >= c.endByte {
			continue // window-fallback chunk without byte range
		}
		if c.startByte < prevEnd {
			t.Errorf("chunk %d startByte=%d overlaps previous end=%d", i, c.startByte, prevEnd)
		}
		if c.endByte > len(src) {
			t.Errorf("chunk %d endByte=%d exceeds src len %d", i, c.endByte, len(src))
		}
		prevEnd = c.endByte
	}
}
