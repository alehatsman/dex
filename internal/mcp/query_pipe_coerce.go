package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// query_pipe_coerce.go — the coercion half of the pipe (#206 / docs/design/95h).
// A transform (callers/callees/impact) consumes `symbol` refs, but a seed may
// hand it a different kind (a path seed → file refs, a grep/semantic seed →
// chunk refs). Coercion turns the input set into symbols before the transform
// fans out, and is provenance-logged — a coercion never *raises* trust.

// symbolCoercer is the optional capability a toolSurface may implement to turn a
// file/dir path into its contained symbols. It is discovered by type assertion
// (like seenLooker in look.go), NOT a method on the 30-strong toolSurface
// interface: only the store-backed *Server can serve it, and the remote surface
// runs the whole pipe server-side on a *Server via /query, so it never needs its
// own coercer. When the capability is absent and a file/dir coercion is needed,
// the pipe errors honestly rather than silently dropping refs.
type symbolCoercer interface {
	// symbolsUnder returns the top-level symbols contained in a file, or — when
	// the path is a directory/package — the exported symbols under it.
	symbolsUnder(ctx context.Context, projectRoot, pathOrDir string) ([]Ref, error)
}

// nonSymbolNodeKinds are graph-node kinds that are not code symbols a transform
// can trace (mirrors GraphQualifiedNameAt's exclusion set). Coercion drops them.
var nonSymbolNodeKinds = map[string]bool{
	"file": true, "import": true, "package": true, "module": true, "document": true,
}

// symbolsUnder implements symbolCoercer on *Server: SymbolsByFile for a file,
// falling back to ExportedSymbolsByDir when the path names a directory/package.
// Provenance is name-based — these come from the tree-sitter symbol table, not a
// resolved call edge.
func (s *Server) symbolsUnder(ctx context.Context, projectRoot, pathOrDir string) ([]Ref, error) {
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
	syms, err := st.SymbolsByFile(ctx, pathOrDir)
	if err != nil {
		return nil, err
	}
	if len(syms) == 0 {
		// Not a file with indexed symbols — try it as a directory/package.
		syms, err = st.ExportedSymbolsByDir(ctx, pathOrDir)
		if err != nil {
			return nil, err
		}
	}
	out := make([]Ref, 0, len(syms))
	for _, gs := range syms {
		if nonSymbolNodeKinds[gs.Kind] {
			continue
		}
		out = append(out, Ref{
			Kind: "symbol", ID: gs.QualifiedName, Path: gs.FilePath,
			Span: span(gs.StartLine, gs.EndLine), Prov: "name-based", Score: gs.PageRank,
		})
	}
	return out, nil
}

// coerceToSymbols normalizes a heterogeneous ref set into symbol refs so a
// transform can fan out over it. It dedupes by ID and caps the result at
// pipeMaxRefs. `coerced` is true when any ref needed conversion (the caller
// weakens provenance accordingly). Unconvertible kinds are an honest error whose
// clustering signals which coercion to add next (spec Coercion policy).
func coerceToSymbols(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput, refs []Ref) (out []Ref, coerced bool, err error) {
	seen := make(map[string]bool, len(refs))
	add := func(r Ref) {
		if r.ID == "" || seen[r.ID] || len(out) >= pipeMaxRefs {
			return
		}
		seen[r.ID] = true
		out = append(out, r)
	}
	for _, r := range refs {
		switch r.Kind {
		case "symbol":
			add(r)
		case "chunk":
			if sym, ok := coerceChunkToSymbol(ctx, h, req, in, r); ok {
				coerced = true
				add(sym)
			}
			// A chunk with no enclosing symbol (e.g. a comment/import match) is
			// silently skipped — it is not an error, just not traceable.
		case "file", "package":
			sc, ok := h.(symbolCoercer)
			if !ok {
				return nil, false, fmt.Errorf("cannot expand %s %q to symbols on this surface", r.Kind, r.ID)
			}
			syms, serr := sc.symbolsUnder(ctx, in.ProjectRoot, refPath(r))
			if serr != nil {
				return nil, false, serr
			}
			coerced = true
			for _, sym := range syms {
				add(sym)
			}
		default:
			return nil, false, fmt.Errorf("cannot coerce a %q ref to a symbol", r.Kind)
		}
		if len(out) >= pipeMaxRefs {
			break
		}
	}
	if len(out) == 0 {
		return nil, coerced, errors.New("no symbols to transform — the input refs did not resolve to any traceable symbol")
	}
	return out, coerced, nil
}

// coerceChunkToSymbol resolves a chunk ref (path:line) to its enclosing symbol
// via the existing locate lane. Provenance stays name-based (the enclosing
// symbol is a structural, not a resolved-edge, fact).
func coerceChunkToSymbol(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput, r Ref) (Ref, bool) {
	ref := r.Path
	if len(r.Span) > 0 && r.Span[0] > 0 {
		ref = r.Path + ":" + strconv.Itoa(r.Span[0])
	}
	_, lo, err := h.locate(ctx, req, LocateInput{Ref: ref, ProjectRoot: in.ProjectRoot})
	if err != nil || lo.Symbol == "" {
		return Ref{}, false
	}
	return Ref{
		Kind: "symbol", ID: lo.Symbol, Path: lo.Path,
		Span: span(lo.StartLine, lo.EndLine), Prov: "name-based",
	}, true
}

// refPath is the project-relative path a coercer should look up: Path when set,
// else the ID (a bare path seed carries the path in ID).
func refPath(r Ref) string {
	if r.Path != "" {
		return r.Path
	}
	return r.ID
}
