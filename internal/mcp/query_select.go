package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph/resolve"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// query_select.go — selector-grammar seeds (#210 / docs/design/95i). A new seed
// shape: `field:pattern` tokens that enumerate a symbol set straight from the
// graph index (100% deterministic — the first, static Phase 3 item). Composes
// with pipes like any other seed: `pkg:store | callers | impact`.

// SelectResult is the selector lane's compact body for a standalone query. The
// selected symbols are ALSO on QueryOutput.Refs (the currency pipes thread) —
// this is the readable projection, not a second source of truth.
type SelectResult struct {
	Count   int   `json:"count"`
	Symbols []Ref `json:"symbols"`
}

// selectorFields is the fixed keyword vocabulary. A seed is a selector query iff
// EVERY whitespace token is `<field>:<non-empty>` with field in this set — which
// disambiguates cleanly from `path:line` (head is a path, not a keyword) and
// from prose (no `field:` shape).
var selectorFields = map[string]bool{
	"pkg": true, "func": true, "type": true, "file": true, "kind": true,
}

// isSelectorQuery reports whether every token of s is a well-formed selector.
func isSelectorQuery(s string) bool {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return false
	}
	for _, f := range fields {
		i := strings.IndexByte(f, ':')
		if i <= 0 || i == len(f)-1 { // no colon, empty field, or empty value
			return false
		}
		if !selectorFields[strings.ToLower(f[:i])] {
			return false
		}
	}
	return true
}

// kindAliases maps the short/natural kind words an agent is likely to type onto
// the kinds actually stored in graph_nodes. The `kind:` selector value is passed
// through this so `kind:func` (mirroring the `func:` field name) doesn't silently
// miss — the stored kinds are `function`/`method`, not `func`. Only genuine
// shorthands are listed; an exact stored kind (`function`, `interface`, …) is not
// a key, so it passes through unchanged and its behavior is untouched (#217).
var kindAliases = map[string][]string{
	"func":  {"function", "method"},
	"fn":    {"function", "method"},
	"meth":  {"method"},
	"iface": {"interface"},
}

// normalizeKind expands a `kind:` value to the stored kinds it should match. A
// known shorthand expands (func → function+method, mirroring the func: field);
// any other value returns itself verbatim (exact match on a stored kind), so
// `kind:function`/`kind:interface`/`kind:struct` keep matching exactly.
func normalizeKind(val string) []string {
	v := strings.ToLower(strings.TrimSpace(val))
	if ks, ok := kindAliases[v]; ok {
		return ks
	}
	return []string{v}
}

// resolvePkgTokens rewrites every `pkg:<val>` token whose value exactly
// matches a workspace package's declared package.json name (e.g. @bright/api)
// to `pkg:<dir>`, the file-derived directory parseSelector actually knows how
// to match. ws may be a nil/empty *resolve.Workspace (a repo with no
// workspace config) — Projects() is nil-safe and returns nothing, so this is
// then a no-op. Non-pkg tokens and values with no matching project name pass
// through unchanged.
func resolvePkgTokens(s string, ws *resolve.Workspace) string {
	projects := ws.Projects()
	if len(projects) == 0 {
		return s
	}
	fields := strings.Fields(s)
	for i, f := range fields {
		field, val, ok := strings.Cut(f, ":")
		if !ok || strings.ToLower(field) != "pkg" {
			continue
		}
		for _, p := range projects {
			if p.Name == val {
				fields[i] = "pkg:" + p.Dir
				break
			}
		}
	}
	return strings.Join(fields, " ")
}

// parseSelector folds the tokens into a conjunctive store.SymbolSelector. func:
// and type: constrain name + kind; pkg:/file: are substring/glob path filters;
// kind: adds a bare kind constraint. Distinct fields AND together.
func parseSelector(s string) store.SymbolSelector {
	var sel store.SymbolSelector
	kinds := map[string]bool{}
	for _, f := range strings.Fields(s) {
		i := strings.IndexByte(f, ':')
		field := strings.ToLower(f[:i])
		val := f[i+1:]
		switch field {
		case "func":
			sel.Name = store.GlobToLike(val, false)
			kinds["function"], kinds["method"] = true, true
		case "type":
			sel.Name = store.GlobToLike(val, false)
			kinds["struct"], kinds["interface"], kinds["type"] = true, true, true
		case "pkg":
			sel.Pkg = store.GlobToLike(val, true)
		case "file":
			// A bare basename glob (no "/") is a match-anywhere-in-the-path
			// request — GlobToLike's substring wrap only fires when the glob
			// has no wildcard, so "query*.go" would otherwise anchor to the
			// start of the full repo-relative path and never match a nested
			// file (#231). A pattern that already names a directory (has "/")
			// keeps the existing anchored-from-start behavior.
			//
			// Only the LEADING boundary is ever added here (never a trailing
			// "%"): wrapping both ends turns a suffix glob like "*.go" into an
			// unanchored substring match ("%.go%"), which also matches
			// ".golangci.yml" or "algorithm.gox" — anything containing ".go"
			// anywhere, not just paths ending in it (#231 review fix). The
			// glob's own trailing "*" (if any) already becomes a trailing "%"
			// via GlobToLike's translation, so no extra wrap is needed there;
			// omitting one when absent correctly anchors the match to the end
			// of the path.
			if strings.Contains(val, "/") {
				sel.File = store.GlobToLike(val, true)
			} else {
				like := store.GlobToLike(val, false)
				if !strings.HasPrefix(val, "*") {
					like = "%" + like
				}
				sel.File = like
			}
		case "kind":
			for _, k := range normalizeKind(val) {
				kinds[k] = true
			}
		}
	}
	if len(kinds) > 0 {
		sel.Kinds = make([]string, 0, len(kinds))
		for k := range kinds {
			sel.Kinds = append(sel.Kinds, k)
		}
		sort.Strings(sel.Kinds) // deterministic query + test stability
	}
	return sel
}

// dispatchSelector runs the selector lane: parse the tokens, enumerate matching
// symbols from the store, and project them into the uniform Ref currency (the
// payload pipes thread) plus a compact SelectResult body. Provenance is
// name-based — these come from the tree-sitter symbol table, not a resolved edge.
func dispatchSelector(ctx context.Context, h toolSurface, _ *sdk.CallToolRequest, in QueryInput, cleaned string, route QueryRoute) (*sdk.CallToolResult, QueryOutput, error) {
	fail := func(reason string, err error) (*sdk.CallToolResult, QueryOutput, error) {
		return nil, QueryOutput{
			Status: "error", Route: route,
			Trust: EnvTrust{Provenance: "name-based", Caveat: reason},
		}, err
	}
	src, ok := h.(symbolSelectorSource)
	if !ok {
		return fail("selector lane is unavailable on this surface", nil)
	}
	refs, err := src.selectSymbols(ctx, in.ProjectRoot, cleaned, in.K)
	if err != nil {
		return fail(err.Error(), err)
	}
	status := "ok"
	var next []NextStep
	if len(refs) == 0 {
		status = "not-found"
		// A dead-end selector is a genuine fallback point (#231, mirroring the
		// symbol/grep/locate lanes): the pattern may be too narrow, or this
		// shouldn't have been a selector query at all.
		next = append(next, searchFallbackNext(cleaned, "no symbol matches this selector — search for the behavior instead"))
	}
	return nil, QueryOutput{
		Status: status,
		Route:  route,
		Result: QueryResult{Select: &SelectResult{Count: len(refs), Symbols: refs}},
		Refs:   refs,
		Trust:  EnvTrust{Provenance: "name-based"},
		Next:   next,
	}, nil
}

// symbolSelectorSource is the optional capability backing the selector lane —
// type-asserted like symbolCoercer/seenLooker, not a toolSurface method. Only
// the store-backed *Server implements it; the remote surface runs the whole
// query server-side on a *Server via /query, so it never needs its own.
type symbolSelectorSource interface {
	selectSymbols(ctx context.Context, projectRoot, cleaned string, limit int) ([]Ref, error)
}

// selectSymbols implements symbolSelectorSource on *Server, mirroring
// symbolsUnder: resolve the project, open the (cached) store, run the indexed
// query, and project GraphSymbols into symbol Refs ranked by pagerank.
func (s *Server) selectSymbols(ctx context.Context, projectRoot, cleaned string, limit int) ([]Ref, error) {
	p, hint := s.resolveProject(ctx, projectRoot)
	if hint != "" {
		return nil, errors.New(hint)
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no index for %s — run `dex index %s` first", p.Root, p.Root)
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	// A `pkg:` token is resolved against the workspace's declared package.json
	// names before parsing — the store only carries the file-derived directory
	// (packages/bright-api), never the scoped npm name (@bright/api) that
	// imports/docs actually use, so a bare directory-basename match would
	// silently miss every scoped workspace package (#232). resolve.Load walks
	// the whole project tree for package.json/tsconfig.json, so it's only
	// worth paying for when the selector actually has a pkg: token to resolve
	// — every func:/type:/file:/kind: selector query skips it entirely.
	resolved := cleaned
	if strings.Contains(strings.ToLower(cleaned), "pkg:") {
		resolved = resolvePkgTokens(cleaned, resolve.Load(p.Root))
	}
	sel := parseSelector(resolved)
	syms, err := st.SelectSymbols(ctx, sel, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Ref, 0, len(syms))
	for _, gs := range syms {
		out = append(out, Ref{
			Kind: "symbol", ID: gs.QualifiedName, Path: gs.FilePath,
			Span: span(gs.StartLine, gs.EndLine), Prov: "name-based", Score: gs.PageRank,
		})
	}
	return out, nil
}
