package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/store"
)

// unresolvedInbounder is the optional capability of a tool surface to attribute
// known-unresolved imports to a file's package (#130). Only the local surfaces
// (*Server and its projectScoped wrapper) implement it; remote/maintenance/test
// surfaces don't, and foldUnresolvedInbound simply skips them — the trace is
// still correct, just without the extra recall signal.
type unresolvedInbounder interface {
	unresolvedInbound(ctx context.Context, projectRoot, file string, limit int) ([]store.UnresolvedInbound, error)
}

// foldUnresolvedInbound queries known-unresolved imports into each non-Go target
// file's package and folds the distinct specifiers (summed by count) into
// out.UnresolvedInbound, plus a hint pointing at grep. Best-effort: skipped when
// the surface can't answer or on any error, and a no-op when there are none, so
// clean traces are byte-identical. Complements the bare-name grep sweep, which
// cannot see these edges because the specifier and the target's name differ.
func foldUnresolvedInbound(ctx context.Context, h toolSurface, projectRoot string, out *TraceOutput) {
	ui, ok := h.(unresolvedInbounder)
	if !ok {
		return
	}
	merged := mergeUnresolvedInbound(out.Targets, func(file string) ([]store.UnresolvedInbound, error) {
		return ui.unresolvedInbound(ctx, projectRoot, file, 0)
	})
	if len(merged) == 0 {
		return
	}
	out.UnresolvedInbound = merged
	out.Recall = "partial"
	hint := UnresolvedInboundHint(merged)
	if out.Hint != "" {
		out.Hint += " | " + hint
	} else {
		out.Hint = hint
	}
}

// mergeUnresolvedInbound queries unresolved-inbound imports for each non-Go
// target file (via query) and merges them by specifier, summed and sorted
// most-frequent first (ties by specifier). Shared by the MCP trace fold and the
// CLI so both surfaces report the same numbers. A per-file query error is
// skipped, not fatal — the signal is best-effort. #130.
func mergeUnresolvedInbound(targets []TargetMatch, query func(file string) ([]store.UnresolvedInbound, error)) []store.UnresolvedInbound {
	sum := map[string]int{}
	var order []string
	for _, t := range targets {
		if t.Path == "" || strings.HasSuffix(t.Path, ".go") {
			continue
		}
		rows, err := query(t.Path)
		if err != nil {
			continue
		}
		for _, r := range rows {
			if _, seen := sum[r.Specifier]; !seen {
				order = append(order, r.Specifier)
			}
			sum[r.Specifier] += r.Count
		}
	}
	if len(order) == 0 {
		return nil
	}
	sort.SliceStable(order, func(i, j int) bool {
		if sum[order[i]] != sum[order[j]] {
			return sum[order[i]] > sum[order[j]]
		}
		return order[i] < order[j]
	})
	out := make([]store.UnresolvedInbound, 0, len(order))
	for _, spec := range order {
		out = append(out, store.UnresolvedInbound{Specifier: spec, Count: sum[spec]})
	}
	return out
}

// UnresolvedInboundHint renders the one-line grep cue for a merged unresolved-
// inbound set (up to the first three specifiers). Exported so the CLI prints the
// same wording as the MCP hint. #130.
func UnresolvedInboundHint(rows []store.UnresolvedInbound) string {
	total := 0
	shown := make([]string, 0, 3)
	for i, r := range rows {
		total += r.Count
		if i < 3 {
			shown = append(shown, r.Specifier)
		}
	}
	return fmt.Sprintf("%d unresolved import(s) into this symbol's package (build-mediated / workspace subpath) that name-based recall cannot see — grep the specifier(s) to confirm: %s",
		total, strings.Join(shown, ", "))
}
