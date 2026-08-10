package chunk

import "sort"

// DenseBigThreshold is the byte size at or above which a declaration keeps its
// own chunk during density packing. Below it, a declaration is "small" and is
// eligible to be merged into a packed run. Grounded in measured generated
// files: dense token/param tables run tens-to-hundreds of ~100-byte one-liners
// (all packed), while real entity interfaces are >512 B (kept standalone, full
// precision). Tunable.
const DenseBigThreshold = 512

// PackDense coarsens an over-dense structural chunk list without dropping any
// content. It is applied only to files that exceed the per-file chunk cap
// (see the indexer): such a file is almost always a dense generated
// declaration table (thousands of tiny consts/types) whose one-chunk-per-
// declaration partition floods the vector index with near-duplicate embeddings.
//
// The transform preserves precision where it matters and collapses only the
// noise: consecutive small declarations (< DenseBigThreshold) are greedily
// merged into MaxBytes-bounded packed chunks, while any big declaration
// (>= DenseBigThreshold) is emitted standalone and unchanged. Nothing is
// dropped — every byte of every declaration remains in some chunk, so grep /
// FTS / semantic search all still find it, only at coarser granularity. Exact
// symbol lookup stays served by the graph index.
//
// A packed chunk spans src[first.startByte : last.endByte] (including any
// inter-declaration whitespace/comments) with line numbers taken from its
// first and last members. Chunks lacking byte offsets (the window-fallback
// path, used only when tree-sitter fails and which never reaches the cap) are
// treated as big and passed through untouched, so packing can never mis-slice.
func PackDense(relPath string, src []byte, chunks []Chunk) []Chunk {
	if len(chunks) == 0 {
		return chunks
	}
	ordered := make([]Chunk, len(chunks))
	copy(ordered, chunks)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].startByte < ordered[j].startByte
	})

	out := make([]Chunk, 0, len(ordered))
	var run []Chunk
	flush := func() {
		out = append(out, mergeRun(relPath, src, run)...)
		run = run[:0]
	}
	for _, c := range ordered {
		if !hasByteRange(c) || len(c.Content) >= DenseBigThreshold {
			flush()
			out = append(out, c)
			continue
		}
		run = append(run, c)
	}
	flush()
	return out
}

// hasByteRange reports whether c carries usable byte offsets. Structural and
// orphan chunks set them; the pure line-window fallback does not.
func hasByteRange(c Chunk) bool {
	return c.endByte > c.startByte
}

// mergeRun greedily packs a run of consecutive small chunks into
// MaxBytes-bounded packed chunks, re-slicing src across each group so no
// inter-declaration text is lost. Returns nil for an empty run.
func mergeRun(relPath string, src []byte, run []Chunk) []Chunk {
	if len(run) == 0 {
		return nil
	}
	var out []Chunk
	groupStart := 0 // index into run of the current group's first member
	for i := range run {
		span := run[i].endByte - run[groupStart].startByte
		// Close the current group before adding run[i] if doing so would
		// overflow MaxBytes and the group is non-empty.
		if i > groupStart && span > MaxBytes {
			out = append(out, packedChunk(relPath, src, run[groupStart:i]))
			groupStart = i
		}
	}
	out = append(out, packedChunk(relPath, src, run[groupStart:]))
	return out
}

// packedChunk builds one KindPacked chunk covering members[0..n], its content
// re-sliced from src so gaps between declarations are retained.
func packedChunk(relPath string, src []byte, members []Chunk) Chunk {
	first, last := members[0], members[len(members)-1]
	s, e := first.startByte, last.endByte
	if s < 0 {
		s = 0
	}
	if e > len(src) {
		e = len(src)
	}
	return Chunk{
		Path:      relPath,
		Kind:      KindPacked,
		StartLine: first.StartLine,
		EndLine:   last.EndLine,
		Content:   string(src[s:e]),
		startByte: s,
		endByte:   e,
	}
}
