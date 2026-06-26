package compress

import (
	"encoding/json"
	"io"
	"strings"
)

// Lossless JSON compaction (#619). Pretty-printed JSON is typically 20–50%
// insignificant whitespace. These helpers strip that whitespace with a plain
// text-level scan — no encoding/json round-trip — so key order, number
// formatting, and string internals are preserved byte-for-byte. The result is
// always semantically identical to the input: pure whitespace removal,
// deterministic, and cache-safe.

// LooksLikeJSON reports whether s's first non-whitespace byte is '{' or '[',
// the cheap shape test used to gate compaction on JSON-looking output.
func LooksLikeJSON(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

// CompactJSON strips insignificant whitespace (space, tab, CR, LF found
// outside string literals) from a single JSON document, or from a
// concatenated stream of JSON objects/arrays (e.g. `go list -json ./...`).
// It returns (compact, true) only when the result is strictly smaller;
// otherwise (input, false).
//
// The scan tracks string state — an unescaped '"' toggles in/out of a string,
// and a backslash escapes the next byte — so whitespace inside a string value
// is copied verbatim. Valid JSON never requires whitespace between structural
// tokens, so dropping it is always lossless. It is NOT safe for line-delimited
// JSON of bare scalars (`1\n2` would merge to `12`); use CompactJSONL or
// CompactJSONAuto for that.
func CompactJSON(input string) (string, bool) {
	// Only strip when the input genuinely IS JSON (one or more well-formed
	// values, whitespace-only between them). Text that merely starts with '{'
	// or '[' — a "[INFO] started on port 8080" log line, or JSON with trailing
	// prose — must pass through untouched; stripping its whitespace as if it
	// were JSON structure would corrupt it (#669). The decoder validates shape
	// only; the byte-level strip below still produces the output, so key order,
	// number formatting, and string internals are preserved verbatim.
	if !isValidJSONStream(input) {
		return input, false
	}
	var b strings.Builder
	b.Grow(len(input))
	inString := false
	escaped := false
	for i := 0; i < len(input); i++ {
		c := input[i]
		if inString {
			b.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case ' ', '\t', '\n', '\r':
			// insignificant whitespace outside a string — drop it
			continue
		case '"':
			inString = true
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	out := b.String()
	if len(out) < len(input) {
		return out, true
	}
	return input, false
}

// isValidJSONStream reports whether s is one or more well-formed JSON values
// separated only by whitespace — exactly the shape CompactJSON can strip
// losslessly (a single pretty document, or a concatenated object stream like
// `go list -json ./...`). Decoding into RawMessage validates each value's
// structure without re-serializing it, and trailing non-JSON bytes make the
// final Decode fail rather than returning io.EOF, so JSON-with-trailing-prose
// is correctly rejected.
func isValidJSONStream(s string) bool {
	dec := json.NewDecoder(strings.NewReader(s))
	n := 0
	for {
		var raw json.RawMessage
		switch err := dec.Decode(&raw); err {
		case nil:
			n++
		case io.EOF:
			return n > 0
		default:
			return false
		}
	}
}

// CompactJSONL compacts each newline-delimited JSON record independently,
// preserving the '\n' separators (which are significant in JSONL — they keep
// adjacent records from merging). Blank lines are kept as-is. Returns
// (compact, true) only when the joined result is strictly smaller.
func CompactJSONL(input string) (string, bool) {
	lines := strings.Split(input, "\n")
	changed := false
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if c, ok := CompactJSON(ln); ok {
			lines[i] = c
			changed = true
		}
	}
	if !changed {
		return input, false
	}
	out := strings.Join(lines, "\n")
	if len(out) < len(input) {
		return out, true
	}
	return input, false
}

// CompactJSONAuto picks the right compactor for JSON-shaped text. Line-
// delimited JSON (more than one non-empty line, each itself starting with '{'
// or '[') is compacted with CompactJSONL so record boundaries survive;
// everything else — a single pretty document or a concatenated object stream —
// goes through CompactJSON for full whitespace removal. Non-JSON input returns
// (input, false).
func CompactJSONAuto(input string) (string, bool) {
	if !LooksLikeJSON(input) {
		return input, false
	}
	if isJSONLines(input) {
		return CompactJSONL(input)
	}
	return CompactJSON(input)
}

// isJSONLines reports whether input looks like line-delimited JSON: more than
// one non-empty line, with every non-empty line beginning with '{' or '['.
// Pretty-printed single documents fail this test (their interior lines start
// with '"' or whitespace), so they fall through to CompactJSON.
func isJSONLines(input string) bool {
	nonEmpty := 0
	for _, ln := range strings.Split(input, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		nonEmpty++
		if t[0] != '{' && t[0] != '[' {
			return false
		}
	}
	return nonEmpty > 1
}
