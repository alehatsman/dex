package mcp

import (
	"context"
	"os"
)

// indexProber lets look consult whether the backing index is mid-rebuild, so an
// empty index-backed lane (trace/locate) is flagged as possibly-incomplete
// rather than presented as authoritative absence (#152). Implemented by *Server
// (directly) and projectScoped (by delegation), mirroring seenLooker — an
// optional interface type-asserted at the call site so mock surfaces stay lean.
type indexProber interface {
	indexRebuilding(ctx context.Context, root string) (bool, string)
}

// indexRebuilding reports whether a destructive re-index is underway for the
// project rooted at root, plus a one-line note when it is. It resolves the
// project and opens its index through the shared cache (no Close — the handle is
// reused) and returns (false, "") on any resolution/open error: an unavailable
// index is the caller's own no-index path, not a rebuild caveat. The normal
// upsert-then-prune index run never sets the marker; only `dex reindex`'s
// destructive rebuild does (see index_signal.go / store.SetIndexing).
func (s *Server) indexRebuilding(ctx context.Context, root string) (bool, string) {
	p, hint := s.resolveProject(ctx, root)
	if hint != "" {
		return false, ""
	}
	if _, err := os.Stat(p.DBPath); err != nil {
		return false, ""
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return false, ""
	}
	return indexingNotice(ctx, st)
}

// flagRebuildIfEmpty downgrades an index-backed lane's trust when it returned an
// authoritative-looking empty (not-found / no-graph / no-path) while the index is
// mid-rebuild: the absence may be an artifact of the destructive reindex, not
// ground truth (#152). Only trace/locate reach here — grep/read are
// disk-authoritative and their empties are never caveated. Provenance stays
// "exact" (the lane is exact); fresh:false is the honest signal that the index it
// read is being rewritten, prompting the agent to retry once indexing settles.
func flagRebuildIfEmpty(ctx context.Context, h toolSurface, root, status string, out *LookOutput) {
	switch status {
	case "not-found", "no-graph", "no-path":
	default:
		return // ok / no-index / any hit — not an authoritative-looking empty
	}
	pr, ok := h.(indexProber)
	if !ok {
		return
	}
	rebuilding, note := pr.indexRebuilding(ctx, root)
	if !rebuilding {
		return
	}
	f := false
	out.Trust.Fresh = &f
	out.Trust.Caveat = "index rebuild in progress — this empty result may be incomplete; retry once indexing settles"
	out.Hint = appendHint(out.Hint, note)
}
