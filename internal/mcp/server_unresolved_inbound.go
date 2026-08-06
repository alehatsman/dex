package mcp

import (
	"context"
	"errors"
	"os"

	"github.com/alehatsman/dex/internal/store"
)

// unresolvedInbound attributes known-unresolved imports (build-mediated /
// workspace-subpath, #130) to file's package, so trace/impact can surface the
// edges that name-based recall cannot see. Best-effort: any project-resolution,
// missing-index, or store-open error yields nil so it never fails an otherwise
// good trace. Satisfies the unresolvedInbounder optional interface (verbs.go).
func (s *Server) unresolvedInbound(ctx context.Context, projectRoot, file string, limit int) ([]store.UnresolvedInbound, error) {
	p, hint := s.resolveProject(ctx, projectRoot)
	if hint != "" {
		return nil, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, nil
	}
	return st.UnresolvedInboundForFile(ctx, file, limit)
}

// UnresolvedInboundForTargets merges unresolved-inbound imports across a trace's
// non-Go targets — the CLI's route to the same signal the MCP trace fold
// surfaces, so `dex trace` on the terminal agrees with the MCP verb (#130).
func (s *Server) UnresolvedInboundForTargets(ctx context.Context, projectRoot string, targets []TargetMatch) []store.UnresolvedInbound {
	return mergeUnresolvedInbound(targets, func(file string) ([]store.UnresolvedInbound, error) {
		return s.unresolvedInbound(ctx, projectRoot, file, 0)
	})
}
