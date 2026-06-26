package mcp

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/store"
)

// maxClaims caps a single batch so a runaway request can't fan out into
// thousands of index lookups. Excess claims are capped with a "parse_error"
// rather than silently ignored.
const maxClaims = 500

// verifyClaims resolves a batch of 'file:line[:symbol]' citations against the
// index, returning one verdict per claim in request order (#708). It is a bulk
// wrapper over verifyOneClaim (from the check verb in server_check.go): a pure
// index lookup with no callers/tests/blame and no model call, so an agent can
// cheaply confirm that locations carried from notes or memory still hold before
// citing them.
func verifyClaims(ctx context.Context, st *store.Store, root string, claims []ClaimRef) []ClaimResult {
	out := make([]ClaimResult, 0, len(claims))
	for i, c := range claims {
		if i >= maxClaims {
			out = append(out, ClaimResult{Ref: c.Ref, Status: "parse_error"})
			continue
		}
		// Normalise the ref path to project-relative before handing it to
		// verifyOneClaim so absolute paths (e.g. /home/user/project/foo.go:3)
		// still match the index's stored paths.
		c2 := ClaimRef{Ref: normalizeClaimRef(root, c.Ref), Symbol: c.Symbol}
		out = append(out, verifyOneClaim(ctx, st, c2))
		// Echo the original ref back so the caller can map verdict→citation.
		out[len(out)-1].Ref = c.Ref
	}
	return out
}

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
		if rel, err := filepath.Rel(root, pathPart); err == nil && !strings.HasPrefix(rel, "..") {
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
