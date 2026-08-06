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
