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
