package mcp

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// Expansion handles (#344) are the anti-hallucination primitive of the explore
// envelope. Every locator a verb emits carries a handle: an opaque token the
// agent echoes back into read(handle=…) instead of constructing a path:line
// reference by hand. Because the agent never types a path, it cannot invent one
// that points nowhere — a fabricated handle fails to decode or fails the index
// check at resolve time.
//
// The codec is deliberately STATELESS: a handle is just base64url(path\tstart\tend)
// with no server-side registry behind it. There is no per-session map to grow,
// evict, or lose across a reconnect — decode is a pure function and resolution
// is a path lookup against the index the caller already holds. The token is
// opaque to the agent (it is not asked to parse it) while staying trivially
// resolvable to dex.

// handleSep separates the fields packed into a handle's decoded payload.
const handleSep = "\t"

// EncodeHandle packs a project-relative path and an inclusive 1-based line range
// into an opaque, URL-safe handle. start/end are clamped to be sane (start >= 1,
// end >= start) so a malformed call can't mint a handle that decodes to garbage.
func EncodeHandle(path string, start, end int) string {
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	// Line numbers first, path last: decode splits with a limit of 3 so the path
	// is the untouched remainder and may itself contain the separator byte.
	payload := strconv.Itoa(start) + handleSep + strconv.Itoa(end) + handleSep + path
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// DecodeHandle reverses EncodeHandle. ok is false for any token that is not a
// well-formed handle: bad base64, wrong field count, unparseable line numbers,
// or a path that fails validateHandlePath (empty, absolute, or escaping the
// project via ".."). A false ok is the structural guard against hallucinated or
// traversal handles; callers still validate the path exists in the index.
func DecodeHandle(h string) (path string, start, end int, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(h)
	if err != nil {
		return "", 0, 0, false
	}
	parts := strings.SplitN(string(raw), handleSep, 3)
	if len(parts) != 3 {
		return "", 0, 0, false
	}
	start, err = strconv.Atoi(parts[0])
	if err != nil || start < 1 {
		return "", 0, 0, false
	}
	end, err = strconv.Atoi(parts[1])
	if err != nil || end < start {
		return "", 0, 0, false
	}
	path = parts[2]
	if !validateHandlePath(path) {
		return "", 0, 0, false
	}
	return path, start, end, true
}

// validateHandlePath rejects paths that could escape the project root or that
// aren't project-relative. It is intentionally structural — it does NOT touch
// the filesystem or index; existence is the resolver's job. A path passes only
// if it is non-empty, uses forward slashes, is not absolute, and contains no
// "." or ".." segment.
func validateHandlePath(path string) bool {
	if path == "" {
		return false
	}
	if strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return false
	}
	// A leading "~" or a Windows drive letter ("C:") is not a relative repo path.
	if strings.HasPrefix(path, "~") || (len(path) >= 2 && path[1] == ':') {
		return false
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
