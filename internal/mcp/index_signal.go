// Copyright 2026 Aleh Atsman

package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/alehatsman/dex/internal/store"
)

// indexingNotice reports whether a re-index is currently underway for the
// given store and, if so, a caller-facing message. A full rebuild deletes
// and re-inserts chunks, so a query that lands mid-rebuild sees only the
// rows written so far — partial results that previously looked complete
// (stale=false). Surfacing the signal lets an agent retry instead of
// trusting a half-built index as authoritative (#531).
func indexingNotice(ctx context.Context, st *store.Store) (bool, string) {
	inProgress, started := st.IndexingInProgress(ctx)
	if !inProgress {
		return false, ""
	}
	return true, fmt.Sprintf(
		"index rebuild in progress (started %s ago) — results are partial; retry shortly for complete output",
		time.Since(started).Round(time.Second))
}
