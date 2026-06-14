package mcp

import (
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
)

// DescriptionMode controls how verbose MCP tool descriptions are on tools/list.
// Shorter descriptions reduce the token cost of every conversation turn.
type DescriptionMode int

const (
	DescModeFull  DescriptionMode = iota // unchanged
	DescModeTerse                        // abbreviate + truncate to 3 sentences (~60% savings)
	DescModeLazy                         // first sentence only + ask hint (~85% savings)
)

// descriptionModeFromEnv reads DEX_DESCRIPTION_MODE (full|terse|lazy).
//
// Default: terse (#547). Full tool descriptions sit in the tools array of every
// turn, so the everyday surface ships compact; set DEX_DESCRIPTION_MODE=full to
// opt back into verbose descriptions.
//
// Caveat (mirrors the proxy clamp in cmd/dex/proxy.go, #242): when
// ENABLE_TOOL_SEARCH / tool_reference forwarding is active the agent relies on
// full tool docs to pick tools, so any compacted mode is forced back to full.
func descriptionModeFromEnv() DescriptionMode {
	mode := DescModeTerse // default: compact (#547)
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEX_DESCRIPTION_MODE"))) {
	case "full":
		mode = DescModeFull
	case "terse":
		mode = DescModeTerse
	case "lazy":
		mode = DescModeLazy
	}
	if mode != DescModeFull && toolSearchActive() {
		return DescModeFull
	}
	return mode
}

// toolSearchActive reports whether ENABLE_TOOL_SEARCH is truthy. Tool-search /
// tool_reference forwarding needs full tool docs for selection quality, so it
// clamps any compacted description mode back to full. Mirrors envBool(_, false).
func toolSearchActive() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ENABLE_TOOL_SEARCH"))) {
	case "1", "on", "true", "yes":
		return true
	default:
		return false
	}
}

// compressToolDesc applies the given mode to a tool description string.
func compressToolDesc(desc string, mode DescriptionMode) string {
	switch mode {
	case DescModeTerse:
		return tersifyDesc(desc)
	case DescModeLazy:
		return lazyDesc(desc)
	default:
		return desc
	}
}

// tersifyDesc applies abbreviations and truncates to the first 3 sentences.
// Lines starting with "Example", "Note:", or "See also" are stripped.
func tersifyDesc(desc string) string {
	desc = stripMetaLines(desc)
	desc = compress.AbbreviateText(desc)
	return truncateSentences(desc, 3)
}

// lazyDesc keeps the first sentence (≤80 chars) and appends an ask hint.
func lazyDesc(desc string) string {
	first := truncateSentences(desc, 1)
	if len(first) > 80 {
		// Hard-cap at a word boundary near 80 chars.
		cut := strings.LastIndex(first[:80], " ")
		if cut > 0 {
			first = first[:cut]
		} else {
			first = first[:80]
		}
	}
	first = strings.TrimRight(first, ". ")
	return first + ". (use use ask for full context)"
}

// stripMetaLines removes lines whose content starts with "Example", "Note:",
// or "See also" (case-insensitive). These are documentation anchors that add
// context for humans but cost tokens without adding routing value for agents.
func stripMetaLines(desc string) string {
	lines := strings.Split(desc, "\n")
	out := lines[:0]
	for _, l := range lines {
		trim := strings.TrimSpace(l)
		lower := strings.ToLower(trim)
		if strings.HasPrefix(lower, "example") ||
			strings.HasPrefix(lower, "note:") ||
			strings.HasPrefix(lower, "see also") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// truncateSentences returns s up to and including the nth sentence boundary
// (". " delimiter). If fewer than n boundaries exist the full string is returned.
func truncateSentences(s string, n int) string {
	s = strings.TrimSpace(s)
	count := 0
	for i := 0; i < len(s)-1; i++ {
		if s[i] == '.' && s[i+1] == ' ' {
			count++
			if count >= n {
				return strings.TrimSpace(s[:i+1])
			}
		}
	}
	return s
}
