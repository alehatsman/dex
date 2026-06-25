package mcp

import "strings"

// ReadMode is the typed form of the `read` MCP tool's `mode` parameter.
//
// Wire format stays a string (JSON in/out, profile config, parity tests); use
// ParseReadMode at the boundary and the typed value within dispatch, cache, and
// bounce logic so mode comparisons have a single source of truth.
type ReadMode string

// User-facing modes plus two internal values (lines is a stand-in for the
// lines:N-M prefix family; handle is the budget-downgrade terminal, #487).
const (
	ReadModeFull       ReadMode = "full"
	ReadModeSignatures ReadMode = "signatures"
	ReadModeSkeleton   ReadMode = "skeleton"
	ReadModeMap        ReadMode = "map"
	ReadModeAggressive ReadMode = "aggressive"
	ReadModeSummary    ReadMode = "summary"
	ReadModeLines      ReadMode = "lines"
	ReadModeHandle     ReadMode = "handle"
)

func (m ReadMode) String() string { return string(m) }

// IsLines reports whether m is a lines:N-M slice.
func (m ReadMode) IsLines() bool { return strings.HasPrefix(string(m), "lines:") }

// IsLLM reports whether the mode runs through the chat model. Only `summary`
// does; the structural modes are deterministic.
func (m ReadMode) IsLLM() bool { return m == ReadModeSummary }

// IsComplete reports whether the mode already delivers exhaustive content for
// the requested range (`full` and `summary`). escalateOnBounce uses this to
// skip modes that have no further escalation target.
func (m ReadMode) IsComplete() bool { return m == ReadModeFull || m == ReadModeSummary }

// IsLossySummary reports whether the mode drops or transforms source content
// (signatures / skeleton / map / aggressive / summary). `full` and `lines:*`
// return bytes verbatim and are not lossy in this sense.
func (m ReadMode) IsLossySummary() bool {
	switch m {
	case ReadModeSignatures, ReadModeSkeleton, ReadModeMap, ReadModeAggressive, ReadModeSummary:
		return true
	}
	return false
}

// NeedsIndex reports whether the mode requires the symbol index to render.
// signatures / skeleton / map are index-backed; the rest read the file
// directly or run text compression.
func (m ReadMode) NeedsIndex() bool {
	switch m {
	case ReadModeSignatures, ReadModeSkeleton, ReadModeMap:
		return true
	}
	return false
}

// IsCompressedCacheable reports whether the rendered output for a mode is safe
// to cache keyed by (path, etag, mode) — i.e. deterministic for a given input.
// `summary` is excluded: chat output varies with model / temperature / focus.
func (m ReadMode) IsCompressedCacheable() bool {
	if m.IsLines() {
		return true
	}
	switch m {
	case ReadModeFull, ReadModeSignatures, ReadModeSkeleton,
		ReadModeMap, ReadModeAggressive, ReadModeHandle:
		return true
	}
	return false
}

// ParseReadMode normalizes a wire string into a ReadMode. Empty / whitespace
// input returns ok=false so callers can apply profile or `full` defaults.
// Unknown values pass through verbatim — the dispatcher rejects them via
// ValidReadMode rather than this parser silently mapping to a fallback.
func ParseReadMode(s string) (ReadMode, bool) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return "", false
	}
	return ReadMode(v), true
}

// AllReadModes returns the operator-selectable modes in documentation order.
// `handle` is intentionally excluded (internal terminal); `lines` stands in for
// the lines:N-M prefix family. Mirrors the dispatch switch — keep in sync.
func AllReadModes() []ReadMode {
	return []ReadMode{
		ReadModeFull, ReadModeSignatures, ReadModeSkeleton, ReadModeMap,
		ReadModeAggressive, ReadModeLines, ReadModeSummary,
	}
}

// ValidReadMode reports whether m is one the Summarize dispatch actually
// handles: an exact AllReadModes entry (minus the bare `lines` stand-in), the
// `lines:N-M` prefix family, or the internal `handle` terminal.
func ValidReadMode(m ReadMode) bool {
	if m.IsLines() || m == ReadModeHandle {
		return true
	}
	for _, x := range AllReadModes() {
		if x != ReadModeLines && x == m {
			return true
		}
	}
	return false
}
