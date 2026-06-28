package mcp

import (
	"path/filepath"
	"strings"
)

// Claim/ref path normalization shared by the check verb (batch
// 'file:line[:symbol]' verification) and locate's single-target resolver.

// maxClaims caps a single batch so a runaway request can't fan out into
// thousands of index lookups. Excess claims are capped with a "parse_error"
// rather than silently ignored.
const maxClaims = 500

// normalizeClaimRef extracts the path component of a 'file:line[:sym]' ref,
// makes it project-relative if absolute, then reconstructs the ref string.
func normalizeClaimRef(root, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ref
	}
	// Detect and peel off a trailing ':col' and ':line' so we can normalize
	// just the path segment, then put the suffix back.
	parts := strings.SplitN(ref, ":", 4)
	if len(parts) == 0 {
		return ref
	}
	pathPart := parts[0]
	if filepath.IsAbs(pathPart) {
		// Canonicalize so filepath.Rel works on platforms where temp/project
		// paths are symlinks (e.g. /var → /private/var on macOS). The file
		// itself may not exist on disk (index-only entry), so fall back to
		// resolving just the parent directory.
		canonical := pathPart
		if real, err := filepath.EvalSymlinks(pathPart); err == nil {
			canonical = real
		} else if realDir, err := filepath.EvalSymlinks(filepath.Dir(pathPart)); err == nil {
			canonical = filepath.Join(realDir, filepath.Base(pathPart))
		}
		if rel, err := filepath.Rel(root, canonical); err == nil && !strings.HasPrefix(rel, "..") {
			pathPart = rel
		}
	}
	pathPart = filepath.ToSlash(filepath.Clean(pathPart))
	if len(parts) == 1 {
		return pathPart
	}
	return pathPart + ":" + strings.Join(parts[1:], ":")
}

// normalizeRefPath makes a ref path project-relative and slash-cleaned so it
// matches the index's stored paths. Used by resolveByRef.
func normalizeRefPath(root, path string) string {
	if filepath.IsAbs(path) {
		if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
			path = rel
		}
	}
	return filepath.ToSlash(filepath.Clean(path))
}
