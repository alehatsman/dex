package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/review"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// query_since.go — the since:/diff: pipe seed (#219). Folds review_diff's
// blast-radius resolution (a git range → the symbols its hunks touch) into the
// pipe grammar as a seed, so `query("since:HEAD~3 | impact")` runs through the
// same composable pipeline as every other seed instead of a separate hard-coded
// path. Reuses resolveReviewRange + resolveHunkSymbols verbatim — this file adds
// no new diff-resolution logic, only a seed that calls into the existing one.

// SinceResult is the since lane's compact body for a standalone query. The
// selected symbols are ALSO on QueryOutput.Refs (the currency pipes thread) —
// this is the readable projection, not a second source of truth.
type SinceResult struct {
	Range   string `json:"range"`
	Count   int    `json:"count"`
	Symbols []Ref  `json:"symbols"`
}

// sincePrefixes are the seed keywords, both resolving identically — `diff:` is
// offered as the natural-language alias an agent reaching for review_diff might
// type first.
var sincePrefixes = []string{"since:", "diff:"}

// parseSinceSeed reports whether s is a `since:<ref>` / `diff:<ref>` seed and,
// if so, returns the ref (trimmed; empty means "working tree"). A colon inside
// the ref itself (e.g. `since:v1.0.0..v2.0.0`) is fine — everything after the
// first prefix match is the ref, unlike the selector grammar's field tokens.
func parseSinceSeed(s string) (ref string, ok bool) {
	t := strings.TrimSpace(s)
	low := strings.ToLower(t)
	for _, p := range sincePrefixes {
		if strings.HasPrefix(low, p) {
			return strings.TrimSpace(t[len(p):]), true
		}
	}
	return "", false
}

// dispatchSince runs the since lane: resolve the ref to a git range, diff it,
// resolve the touched symbols, and project them into the uniform Ref currency
// (the payload pipes thread) plus a compact SinceResult body. Provenance is
// name-based — these come from the tree-sitter symbol table, not a resolved
// edge, mirroring the selector lane.
func dispatchSince(ctx context.Context, h toolSurface, _ *sdk.CallToolRequest, in QueryInput, ref string, route QueryRoute) (*sdk.CallToolResult, QueryOutput, error) {
	fail := func(reason string, err error) (*sdk.CallToolResult, QueryOutput, error) {
		return nil, QueryOutput{
			Status: "error", Route: route,
			Trust: EnvTrust{Provenance: "name-based", Caveat: reason},
		}, err
	}
	src, ok := h.(sinceSymbolSource)
	if !ok {
		return fail("since lane is unavailable on this surface", nil)
	}
	rng, refs, err := src.sinceSymbols(ctx, in.ProjectRoot, ref, in.K)
	if err != nil {
		return fail(err.Error(), err)
	}
	status := "ok"
	if len(refs) == 0 {
		status = "not-found"
	}
	return nil, QueryOutput{
		Status: status,
		Route:  route,
		Result: QueryResult{Since: &SinceResult{Range: rng, Count: len(refs), Symbols: refs}},
		Refs:   refs,
		Trust:  EnvTrust{Provenance: "name-based"},
	}, nil
}

// sinceSymbolSource is the optional capability backing the since lane —
// type-asserted like symbolSelectorSource, not a toolSurface method. Only the
// store-backed *Server implements it; the remote surface runs the whole query
// server-side on a *Server via /query, so it never needs its own.
type sinceSymbolSource interface {
	sinceSymbols(ctx context.Context, projectRoot, ref string, limit int) (rng string, refs []Ref, err error)
}

// sinceSymbols implements sinceSymbolSource on *Server: resolve the ref to a
// git range via resolveReviewRange (the exact logic review_diff uses — ref
// wins, "working"/empty means the uncommitted working tree), diff it, and
// resolve each hunk's touched symbols via resolveHunkSymbols (the same
// hunk→symbol mapping review_diff uses, without the caller/risk enrichment a
// seed doesn't need).
func (s *Server) sinceSymbols(ctx context.Context, projectRoot, ref string, limit int) (string, []Ref, error) {
	p, hint := s.resolveProject(ctx, projectRoot)
	if hint != "" {
		return "", nil, errors.New(hint)
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("no index for %s — run `dex index %s` first", p.Root, p.Root)
	}

	reviewIn := ReviewInput{ProjectRoot: p.Root}
	if ref == "" || strings.EqualFold(ref, "working") {
		reviewIn.Worktree = true
	} else {
		reviewIn.Ref = ref
	}
	rng, status, rhint := resolveReviewRange(ctx, p.Root, reviewIn)
	if status != "ok" {
		return "", nil, errors.New(rhint)
	}

	diffText, err := gitDiffUnified(ctx, p.Root, rng)
	if err != nil {
		return rng, nil, fmt.Errorf("could not diff %q — check it is a valid git ref/range (try `git rev-parse %s`)", rng, rng)
	}
	files := review.ParseUnified(diffText)
	if len(files) == 0 {
		return rng, nil, nil
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return rng, nil, fmt.Errorf("open index: %w", err)
	}

	seen := map[string]bool{}
	var out []Ref
	for _, fd := range files {
		if fd.Status == "deleted" {
			continue
		}
		for _, h := range fd.Hunks {
			for _, sym := range resolveHunkSymbols(ctx, st, fd.Path, h, nil) {
				if seen[sym.Name] {
					continue
				}
				seen[sym.Name] = true
				out = append(out, Ref{
					Kind: "symbol", ID: sym.Name, Path: fd.Path,
					Span: span(sym.StartLine, sym.EndLine), Prov: "name-based",
				})
				if limit > 0 && len(out) >= limit {
					return rng, out, nil
				}
			}
		}
	}
	return rng, out, nil
}
