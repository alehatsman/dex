package mcp

// nonCodeMap — token-efficient structural outlines for non-code files.
// Pure Go, no LLM, no index required.  Invoked by view_summarize mode=map
// before the graph-symbol path so Markdown/JSON/YAML/TOML/lock files get
// useful output instead of "no indexed data".

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// nonCodeMap returns a structural outline of the file if the extension/name
// is recognised. Returns ("", false) for code files or unknown types.
func nonCodeMap(relPath string, data []byte) (string, bool) {
	base := filepath.Base(relPath)
	ext := strings.ToLower(filepath.Ext(relPath))

	// Lock files matched by basename before generic extension handling.
	switch base {
	case "go.sum":
		return mapGoSum(relPath, data), true
	case "package-lock.json":
		return mapPackageLock(relPath, data), true
	case "yarn.lock":
		return mapYarnLock(relPath, data), true
	case "Cargo.lock":
		return mapCargoLock(relPath, data), true
	case "poetry.lock", "uv.lock":
		return mapPoetryLock(relPath, data), true
	}

	switch ext {
	case ".md", ".markdown":
		return mapMarkdown(relPath, data), true
	case ".json":
		return mapJSON(relPath, data), true
	case ".yaml", ".yml":
		return mapYAML(relPath, data), true
	case ".toml":
		return mapTOML(relPath, data), true
	}
	return "", false
}

// mapMarkdown emits the heading outline — fence-aware so code blocks are skipped.
func mapMarkdown(relPath string, data []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FILE: %s (Markdown — heading outline)\n\n", relPath)

	inFence := false
	count := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || !strings.HasPrefix(line, "#") {
			continue
		}
		i := 0
		for i < len(line) && line[i] == '#' {
			i++
		}
		if i >= len(line) || (line[i] != ' ' && line[i] != '\t') {
			continue
		}
		text := strings.TrimSpace(line[i+1:])
		fmt.Fprintf(&b, "%s%s %s\n", strings.Repeat("  ", i-1), strings.Repeat("#", i), text)
		count++
	}
	if count == 0 {
		b.WriteString("(no headings)\n")
	}
	return b.String()
}

// mapJSON emits the top-level key structure with value type summaries (depth ≤ 3).
func mapJSON(relPath string, data []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FILE: %s (JSON)\n\n", relPath)

	var top any
	if err := json.Unmarshal(data, &top); err != nil {
		fmt.Fprintf(&b, "(parse error: %v)\n", err)
		return b.String()
	}
	writeJSONNode(&b, top, 0, 3)
	return b.String()
}

func writeJSONNode(b *strings.Builder, v any, depth, maxDepth int) {
	if depth >= maxDepth {
		return
	}
	indent := strings.Repeat("  ", depth)
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		child := m[k]
		fmt.Fprintf(b, "%s%s: %s\n", indent, k, jsonTypeSummary(child))
		if _, isMap := child.(map[string]any); isMap {
			writeJSONNode(b, child, depth+1, maxDepth)
		}
	}
}

func jsonTypeSummary(v any) string {
	switch val := v.(type) {
	case map[string]any:
		return fmt.Sprintf("{%d keys}", len(val))
	case []any:
		if len(val) == 0 {
			return "[]"
		}
		return fmt.Sprintf("[%d]", len(val))
	case string:
		if len(val) > 60 {
			return fmt.Sprintf("%q…", val[:57])
		}
		return fmt.Sprintf("%q", val)
	case float64:
		return fmt.Sprintf("%g", val)
	case bool:
		return fmt.Sprintf("%t", val)
	default:
		return "null"
	}
}

// mapYAML emits the key hierarchy via line scanning — no external YAML library.
// Covers the 80% case: standard CI configs, Helm values, Docker Compose.
func mapYAML(relPath string, data []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FILE: %s (YAML)\n\n", relPath)

	inBlockScalar := false
	blockIndent := -1
	count := 0

	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			if inBlockScalar {
				inBlockScalar = false
				blockIndent = -1
			}
			continue
		}
		indent := 0
		for indent < len(line) && line[indent] == ' ' {
			indent++
		}
		if inBlockScalar {
			if indent > blockIndent {
				continue
			}
			inBlockScalar = false
			blockIndent = -1
		}
		trimmed := line[indent:]
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			continue
		}
		colon := strings.IndexByte(trimmed, ':')
		if colon <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:colon])
		if strings.ContainsAny(key, "[]{}|>&*!,") {
			continue
		}
		rest := strings.TrimSpace(trimmed[colon+1:])
		indentOut := strings.Repeat("  ", indent/2)
		switch rest {
		case "|", ">", "|-", ">-", "|+", ">+":
			fmt.Fprintf(&b, "%s%s: (multiline)\n", indentOut, key)
			inBlockScalar = true
			blockIndent = indent
		case "":
			fmt.Fprintf(&b, "%s%s:\n", indentOut, key)
		default:
			if len(rest) > 50 {
				fmt.Fprintf(&b, "%s%s: %s…\n", indentOut, key, rest[:47])
			} else {
				fmt.Fprintf(&b, "%s%s: %s\n", indentOut, key, rest)
			}
		}
		count++
	}
	if count == 0 {
		b.WriteString("(empty or unrecognised structure)\n")
	}
	return b.String()
}

// mapTOML emits section headers and root-level key=value pairs.
func mapTOML(relPath string, data []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FILE: %s (TOML)\n\n", relPath)

	inSection := false
	count := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "[[") {
			if end := strings.Index(t, "]]"); end > 2 {
				fmt.Fprintf(&b, "[[%s]]\n", t[2:end])
				inSection = true
				count++
			}
		} else if strings.HasPrefix(t, "[") {
			if end := strings.Index(t, "]"); end > 1 {
				fmt.Fprintf(&b, "[%s]\n", t[1:end])
				inSection = true
				count++
			}
		} else if !inSection {
			if eq := strings.IndexByte(t, '='); eq > 0 {
				key := strings.TrimSpace(t[:eq])
				val := strings.TrimSpace(t[eq+1:])
				if len(val) > 40 {
					val = val[:37] + "…"
				}
				fmt.Fprintf(&b, "  %s = %s\n", key, val)
				count++
			}
		}
	}
	if count == 0 {
		b.WriteString("(empty)\n")
	}
	return b.String()
}

func mapGoSum(relPath string, data []byte) string {
	mods := make(map[string]bool)
	for line := range strings.SplitSeq(string(data), "\n") {
		if parts := strings.Fields(line); len(parts) >= 2 {
			mod := parts[0]
			if i := strings.Index(mod, "@"); i > 0 {
				mod = mod[:i]
			}
			mods[mod] = true
		}
	}
	return fmt.Sprintf("FILE: %s (go.sum — %d modules)\n", relPath, len(mods))
}

func mapPackageLock(relPath string, data []byte) string {
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Sprintf("FILE: %s (package-lock.json — parse error)\n", relPath)
	}
	count := 0
	if pkgs, ok := v["packages"].(map[string]any); ok {
		count = len(pkgs)
	} else if deps, ok := v["dependencies"].(map[string]any); ok {
		count = len(deps)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "FILE: %s (package-lock.json — %d packages)\n", relPath, count)
	if name, ok := v["name"].(string); ok {
		fmt.Fprintf(&b, "  name: %s\n", name)
	}
	if ver, ok := v["version"].(string); ok {
		fmt.Fprintf(&b, "  version: %s\n", ver)
	}
	if lv, ok := v["lockfileVersion"].(float64); ok {
		fmt.Fprintf(&b, "  lockfileVersion: %d\n", int(lv))
	}
	return b.String()
}

func mapYarnLock(relPath string, data []byte) string {
	count := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasSuffix(t, ":") && !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, " ") && t != ":" {
			count++
		}
	}
	return fmt.Sprintf("FILE: %s (yarn.lock — ~%d packages)\n", relPath, count)
}

func mapCargoLock(relPath string, data []byte) string {
	count := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "[[package]]" {
			count++
		}
	}
	return fmt.Sprintf("FILE: %s (Cargo.lock — %d packages)\n", relPath, count)
}

func mapPoetryLock(relPath string, data []byte) string {
	count := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "[[package]]" {
			count++
		}
	}
	return fmt.Sprintf("FILE: %s (%s — %d packages)\n", relPath, filepath.Base(relPath), count)
}
