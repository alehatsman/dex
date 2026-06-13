package chunk

import "strings"

func windowChunks(relPath string, src []byte) []Chunk {
	lines := strings.Split(string(src), "\n")
	wins := windowOver(lines, 1)
	for i := range wins {
		wins[i].Path = relPath
		wins[i].Kind = KindWindow
	}
	return wins
}

// windowOver slices `lines` into WindowLines-sized windows with
// WindowOverlap rows of repeat. firstLineNumber is the 1-based line
// number of lines[0]. Chunks larger than MaxBytes are further split by
// halving the window size until they fit.
func windowOver(lines []string, firstLineNumber int) []Chunk {
	var out []Chunk
	step := WindowLines - WindowOverlap
	if step <= 0 {
		step = WindowLines
	}
	for i := 0; i < len(lines); i += step {
		j := min(i+WindowLines, len(lines))
		content := strings.Join(lines[i:j], "\n")
		if len(content) > MaxBytes {
			// Halve and re-split this slice.
			out = append(out, halveAndChunk(lines[i:j], firstLineNumber+i)...)
		} else if strings.TrimSpace(content) != "" {
			out = append(out, Chunk{
				StartLine: firstLineNumber + i,
				EndLine:   firstLineNumber + j - 1,
				Content:   content,
			})
		}
		if j == len(lines) {
			break
		}
	}
	return out
}

func halveAndChunk(lines []string, firstLineNumber int) []Chunk {
	if len(lines) == 0 {
		return nil
	}
	if len(lines) == 1 {
		// A single oversized line (typical: minified JS bundle, generated
		// parser, single-line JSON config) cannot be split further on a
		// newline boundary. Fall back to byte-window slicing so we don't
		// silently lose the content from the index.
		return byteWindows(lines[0], firstLineNumber)
	}
	mid := len(lines) / 2
	first := lines[:mid]
	second := lines[mid:]
	var out []Chunk
	if c := strings.Join(first, "\n"); len(c) <= MaxBytes && strings.TrimSpace(c) != "" {
		out = append(out, Chunk{
			StartLine: firstLineNumber,
			EndLine:   firstLineNumber + len(first) - 1,
			Content:   c,
		})
	} else {
		out = append(out, halveAndChunk(first, firstLineNumber)...)
	}
	if c := strings.Join(second, "\n"); len(c) <= MaxBytes && strings.TrimSpace(c) != "" {
		out = append(out, Chunk{
			StartLine: firstLineNumber + len(first),
			EndLine:   firstLineNumber + len(lines) - 1,
			Content:   c,
		})
	} else {
		out = append(out, halveAndChunk(second, firstLineNumber+len(first))...)
	}
	return out
}

// byteWindows splits a single long line into MaxBytes-sized chunks. All
// chunks share the same start_line/end_line since they came from the
// same source line. Empty inputs yield no chunks. Cut points are
// snapped forward to UTF-8 boundaries so a multi-byte rune is never
// split.
func byteWindows(line string, lineNumber int) []Chunk {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	var out []Chunk
	for i := 0; i < len(line); {
		j := min(i+MaxBytes, len(line))
		for j < len(line) && (line[j]&0xC0) == 0x80 {
			j++
		}
		out = append(out, Chunk{
			StartLine: lineNumber,
			EndLine:   lineNumber,
			Content:   line[i:j],
		})
		i = j
	}
	return out
}
