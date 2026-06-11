package proxy

import (
	"encoding/json"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
)

// Tool-description compression (#242, epic #232).
//
// dex (and Claude Code) ship dozens of MCP tools; their full descriptions sit
// in the `tools` array of EVERY /v1/messages request and cost tokens on every
// turn. This pass rewrites each tool's human-readable `description` in flight,
// leaving `name` and `input_schema` byte-for-byte untouched — changing those
// would break tool dispatch. Port of lean-ctx core/terse/mcp_compress.rs.
//
// Modes (least → most aggressive):
//   - Full:  no-op. The conservative default and the opt-out.
//   - Terse: apply the GENERAL abbreviation dictionary, drop Example/Note/
//            See-also lines, keep at most the first 3 surviving lines.
//   - Lazy:  first line only (truncated to ~77 runes) plus a pointer suffix.
//
// Determinism matters: the mode is static per session, so the same tools block
// compresses to the same bytes every turn. That keeps the tools prefix stable
// for the cache-alignment pass (cache.go), which runs after this one and marks
// breakpoints on the already-compressed bytes.
//
// Caveat (documented on #242): when ENABLE_TOOL_SEARCH / tool_reference
// forwarding is in play the agent relies on full tool docs to pick tools, so
// Lazy/Terse would degrade selection quality. The cmd layer clamps the mode to
// Full in that case; this pass itself is mechanism-only and honors whatever
// mode it is handed.

// ToolDescMode selects how aggressively tool descriptions are rewritten.
type ToolDescMode int

const (
	// ToolDescFull leaves descriptions untouched (default / opt-out).
	ToolDescFull ToolDescMode = iota
	// ToolDescTerse abbreviates and trims to ≤3 substantive lines.
	ToolDescTerse
	// ToolDescLazy keeps only a truncated first line plus a pointer suffix.
	ToolDescLazy
)

func (m ToolDescMode) String() string {
	switch m {
	case ToolDescTerse:
		return "terse"
	case ToolDescLazy:
		return "lazy"
	default:
		return "full"
	}
}

// ParseToolDescMode maps a config/env string to a mode, defaulting to Full
// (the safe no-op) for empty or unrecognized input.
func ParseToolDescMode(s string) ToolDescMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "terse":
		return ToolDescTerse
	case "lazy":
		return ToolDescLazy
	default:
		return ToolDescFull
	}
}

// lazyPointerSuffix is appended to a Lazy-mode description so the model knows
// the text was truncated rather than authored short.
const lazyPointerSuffix = " (dex proxy: description truncated)"

// lazyMaxRunes is the rune budget for a Lazy first line before the ellipsis,
// mirroring the rust port's ~77-char floor_char_boundary truncation.
const lazyMaxRunes = 77

// ToolDescStats reports the outcome of one CompressToolDescriptions call.
type ToolDescStats struct {
	Applied         bool         // at least one description was rewritten
	Mode            ToolDescMode // the mode this request ran under
	ToolsCompressed int          // count of tool descriptions actually changed
}

// CompressToolDescriptions rewrites the `description` of each tool in the
// request's `tools` array according to mode, leaving every other field
// untouched. It fails open: any parse error or Full mode returns the original
// body with a zero ToolDescStats. The returned body is only re-serialized when
// at least one description actually changed.
func CompressToolDescriptions(body []byte, mode ToolDescMode) ([]byte, ToolDescStats) {
	if mode == ToolDescFull || len(body) == 0 {
		return body, ToolDescStats{Mode: mode}
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body, ToolDescStats{Mode: mode}
	}
	rawTools, ok := root["tools"]
	if !ok || len(rawTools) == 0 {
		return body, ToolDescStats{Mode: mode}
	}

	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(rawTools, &tools); err != nil || len(tools) == 0 {
		return body, ToolDescStats{Mode: mode}
	}

	changed := 0
	for _, tool := range tools {
		name := unmarshalString(tool["name"])
		desc := unmarshalString(tool["description"])
		if desc == "" {
			continue
		}
		compressed := compressDescription(name, desc, mode)
		if compressed == desc {
			continue
		}
		enc, err := json.Marshal(compressed)
		if err != nil {
			continue // fail open per tool
		}
		tool["description"] = enc
		changed++
	}

	if changed == 0 {
		return body, ToolDescStats{Mode: mode}
	}

	newTools, err := json.Marshal(tools)
	if err != nil {
		return body, ToolDescStats{Mode: mode}
	}
	root["tools"] = newTools
	out, err := json.Marshal(root)
	if err != nil {
		return body, ToolDescStats{Mode: mode}
	}
	return out, ToolDescStats{Applied: true, Mode: mode, ToolsCompressed: changed}
}

// unmarshalString decodes a JSON string field, returning "" for missing or
// non-string values rather than erroring — callers treat empty as "skip".
func unmarshalString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// compressDescription applies one mode to a single description string.
func compressDescription(name, desc string, mode ToolDescMode) string {
	switch mode {
	case ToolDescTerse:
		return terseDescription(desc)
	case ToolDescLazy:
		return lazyDescription(name, desc)
	default:
		return desc
	}
}

// terseDescription abbreviates each line via the GENERAL dictionary, drops
// empty / Example / Note: / See-also lines, and keeps at most the first 3
// surviving lines. Mirrors terse_description in the rust port.
func terseDescription(desc string) string {
	kept := make([]string, 0, 3)
	for _, line := range strings.Split(desc, "\n") {
		abbreviated := compress.AbbreviateText(line)
		trimmed := strings.TrimSpace(abbreviated)
		if trimmed == "" ||
			strings.HasPrefix(trimmed, "Example") ||
			strings.HasPrefix(trimmed, "Note:") ||
			strings.HasPrefix(trimmed, "See also") {
			continue
		}
		kept = append(kept, abbreviated)
		if len(kept) == 3 {
			break
		}
	}
	return strings.Join(kept, "\n")
}

// lazyDescription keeps only the first line, truncated to lazyMaxRunes runes
// with an ellipsis when it overflows, plus the pointer suffix. Mirrors
// lazy_description in the rust port. Truncation is rune-safe so a multibyte
// description never splits a codepoint.
func lazyDescription(name, desc string) string {
	first := name
	if line, _, _ := strings.Cut(desc, "\n"); line != "" {
		first = line
	}
	runes := []rune(first)
	if len(runes) > lazyMaxRunes {
		first = string(runes[:lazyMaxRunes]) + "…"
	}
	return first + lazyPointerSuffix
}
