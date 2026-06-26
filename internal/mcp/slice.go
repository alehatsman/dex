package mcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// applySlice applies a surgical slice spec to raw content bytes.
// Returns (slicedContent, hintMessage, error). When spec is empty returns data
// unchanged. A non-nil error means the spec is malformed or the operation failed;
// the caller should surface it as status=error. An empty result from "search:"
// is not an error — the hint describes the no-match outcome.
//
// Supported specs:
//
//	head:N          first N lines
//	tail:N          last N lines
//	range:L1-L2     lines L1–L2 (1-indexed, inclusive)
//	search:PATTERN  RE2 grep ± 3 context lines; groups separated by ---
//	json_path:EXPR  dot-path JSON extraction ($.a.b, $.a[0].b)
func applySlice(data []byte, spec string) ([]byte, string, error) {
	if spec == "" {
		return data, "", nil
	}
	kind, arg, ok := parseSliceSpec(spec)
	if !ok {
		return nil, "", fmt.Errorf("unrecognized slice spec %q; expected head:N, tail:N, range:L1-L2, search:PATTERN, or json_path:EXPR", spec)
	}
	switch kind {
	case "head":
		n, err := strconv.Atoi(arg)
		if err != nil || n <= 0 {
			return nil, "", fmt.Errorf("slice head: %q must be a positive integer", arg)
		}
		lines := sliceLines(data)
		if n > len(lines) {
			n = len(lines)
		}
		return joinLines(lines[:n]), fmt.Sprintf("first %d lines", n), nil

	case "tail":
		n, err := strconv.Atoi(arg)
		if err != nil || n <= 0 {
			return nil, "", fmt.Errorf("slice tail: %q must be a positive integer", arg)
		}
		lines := sliceLines(data)
		start := len(lines) - n
		if start < 0 {
			start = 0
		}
		return joinLines(lines[start:]), fmt.Sprintf("last %d lines", n), nil

	case "range":
		l1, l2, err := parseLineRange(arg)
		if err != nil {
			return nil, "", fmt.Errorf("slice range: %w", err)
		}
		lines := sliceLines(data)
		start := l1 - 1
		end := l2
		if start < 0 {
			start = 0
		}
		if end > len(lines) {
			end = len(lines)
		}
		if start >= end {
			return nil, "", fmt.Errorf("slice range: %d-%d out of bounds (file has %d lines)", l1, l2, len(lines))
		}
		return joinLines(lines[start:end]), fmt.Sprintf("lines %d-%d", l1, l2), nil

	case "search":
		out, count, err := grepLines(data, arg)
		if err != nil {
			return nil, "", fmt.Errorf("slice search: %w", err)
		}
		if count == 0 {
			return []byte{}, fmt.Sprintf("search %q: no matches", arg), nil
		}
		return out, fmt.Sprintf("search %q: %d match group(s)", arg, count), nil

	case "json_path":
		out, err := extractJSONPath(data, arg)
		if err != nil {
			return nil, "", fmt.Errorf("slice json_path: %w", err)
		}
		return out, fmt.Sprintf("json_path %s", arg), nil

	default:
		return nil, "", fmt.Errorf("unknown slice kind %q", kind)
	}
}

// parseSliceSpec splits "kind:arg" from a slice spec string.
// Returns ok=false for unrecognized kinds.
func parseSliceSpec(spec string) (kind, arg string, ok bool) {
	idx := strings.IndexByte(spec, ':')
	if idx < 0 {
		return "", "", false
	}
	kind = strings.ToLower(spec[:idx])
	arg = spec[idx+1:]
	switch kind {
	case "head", "tail", "range", "search", "json_path":
		return kind, arg, true
	}
	return "", "", false
}

// sliceLines splits content into lines, stripping a trailing newline so the
// result length equals the logical line count.
func sliceLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	s := strings.TrimRight(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// joinLines reassembles lines with newlines, adding a trailing newline.
func joinLines(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// parseLineRange parses "L1-L2" into 1-indexed line numbers.
func parseLineRange(s string) (l1, l2 int, err error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected L1-L2, got %q", s)
	}
	l1, err = strconv.Atoi(parts[0])
	if err != nil || l1 < 1 {
		return 0, 0, fmt.Errorf("invalid start line %q", parts[0])
	}
	l2, err = strconv.Atoi(parts[1])
	if err != nil || l2 < l1 {
		return 0, 0, fmt.Errorf("invalid end line %q (must be >= %d)", parts[1], l1)
	}
	return l1, l2, nil
}

const searchContextLines = 3 // lines of context before/after each match

// grepLines finds lines matching pattern (RE2) and returns them with
// ±searchContextLines context lines. Adjacent/overlapping groups are merged;
// non-adjacent groups are separated by "---\n".
func grepLines(data []byte, pattern string) ([]byte, int, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid regex %q: %w", pattern, err)
	}
	lines := sliceLines(data)
	n := len(lines)
	if n == 0 {
		return nil, 0, nil
	}
	// Mark which lines belong to a match window.
	included := make([]bool, n)
	for i, line := range lines {
		if re.MatchString(line) {
			lo, hi := i-searchContextLines, i+searchContextLines
			if lo < 0 {
				lo = 0
			}
			if hi >= n {
				hi = n - 1
			}
			for j := lo; j <= hi; j++ {
				included[j] = true
			}
		}
	}
	var sb strings.Builder
	inGroup := false
	groupCount := 0
	for i, line := range lines {
		if !included[i] {
			inGroup = false
			continue
		}
		if !inGroup {
			if groupCount > 0 {
				sb.WriteString("---\n")
			}
			groupCount++
			inGroup = true
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return []byte(sb.String()), groupCount, nil
}

// extractJSONPath extracts a value from JSON data by a dot-path expression
// like "$.a.b[0].c". Returns the extracted value as pretty-printed JSON.
func extractJSONPath(data []byte, expr string) ([]byte, error) {
	var root interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	// Strip leading "$." or "$"
	path := strings.TrimPrefix(expr, "$")
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return json.MarshalIndent(root, "", "  ")
	}
	segs, err := parseJSONPathSegs(path)
	if err != nil {
		return nil, err
	}
	cur := root
	for _, seg := range segs {
		switch v := cur.(type) {
		case map[string]interface{}:
			next, ok := v[seg.key]
			if !ok {
				return nil, fmt.Errorf("key %q not found", seg.key)
			}
			if seg.idx >= 0 {
				arr, ok := next.([]interface{})
				if !ok {
					return nil, fmt.Errorf("value at %q is not an array", seg.key)
				}
				if seg.idx >= len(arr) {
					return nil, fmt.Errorf("[%d] out of range (len=%d)", seg.idx, len(arr))
				}
				cur = arr[seg.idx]
			} else {
				cur = next
			}
		case []interface{}:
			if seg.key != "" {
				return nil, fmt.Errorf("expected object, got array (key=%q)", seg.key)
			}
			if seg.idx < 0 || seg.idx >= len(v) {
				return nil, fmt.Errorf("[%d] out of range (len=%d)", seg.idx, len(v))
			}
			cur = v[seg.idx]
		default:
			return nil, fmt.Errorf("cannot navigate into %T at path segment %q", cur, seg.key)
		}
	}
	return json.MarshalIndent(cur, "", "  ")
}

type jsonPathSeg struct {
	key string
	idx int // -1 means no array index
}

// parseJSONPathSegs splits "a.b[0].c" into [{key:"a"},{key:"b",idx:0},{key:"c"}].
func parseJSONPathSegs(path string) ([]jsonPathSeg, error) {
	var segs []jsonPathSeg
	for path != "" {
		part := path
		if i := strings.IndexByte(path, '.'); i >= 0 {
			part = path[:i]
			path = path[i+1:]
		} else {
			path = ""
		}
		if part == "" {
			continue
		}
		seg := jsonPathSeg{idx: -1}
		if i := strings.IndexByte(part, '['); i >= 0 {
			seg.key = part[:i]
			rest := part[i:]
			if !strings.HasSuffix(rest, "]") {
				return nil, fmt.Errorf("malformed index expression %q", rest)
			}
			idxStr := rest[1 : len(rest)-1]
			n, err := strconv.Atoi(idxStr)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("invalid array index %q", idxStr)
			}
			seg.idx = n
		} else {
			seg.key = part
		}
		segs = append(segs, seg)
	}
	return segs, nil
}
