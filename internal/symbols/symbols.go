// Package symbols provides type-precise Go symbol queries via go/types.
// v1 is Go-only; non-Go returns status "unsupported-language".
package symbols

import (
	"context"
	"fmt"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Action names what a Query computes.
type Action string

const (
	References      Action = "references"
	Implementations Action = "implementations"
	Supertypes      Action = "supertypes"
	Subtypes        Action = "subtypes"
)

// Site is one result location.
type Site struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	// Kind: "def" | "use" | "implementor" | "super" | "sub"
	Kind string `json:"kind"`
}

// Result is the query response.
type Result struct {
	Status  string `json:"status"` // "ok" | "unsupported-language" | "not-found" | "error"
	Hint    string `json:"hint,omitempty"`
	Symbol  string `json:"symbol,omitempty"`
	Action  string `json:"action,omitempty"`
	Project string `json:"project,omitempty"`
	Sites   []Site `json:"sites,omitempty"`
}

// Query runs a type-precise symbol query on the Go module rooted at projectRoot.
// symbol accepts: bare name ("Foo"), receiver-qualified ("(*T).M"), or
// package-tail-qualified ("mcp.NewServer").
func Query(ctx context.Context, projectRoot string, action Action, symbol string) (Result, error) {
	res := Result{Symbol: symbol, Action: string(action), Project: projectRoot}
	if !hasGoMod(projectRoot) {
		res.Status = "unsupported-language"
		res.Hint = "refs v1 is Go-only and needs a go.mod at the project root"
		return res, nil
	}

	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports |
			packages.NeedDeps | packages.NeedModule,
		Dir:     projectRoot,
		Context: ctx,
		Fset:    fset,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		res.Status = "error"
		res.Hint = fmt.Sprintf("packages.Load: %v", err)
		return res, nil
	}
	if len(pkgs) == 0 {
		res.Status = "not-found"
		res.Hint = "no Go packages loaded under the project root"
		return res, nil
	}

	switch action {
	case References:
		return queryReferences(res, pkgs, fset, projectRoot, symbol)
	case Implementations:
		return queryImplementations(res, pkgs, fset, projectRoot, symbol)
	case Supertypes:
		return querySupertypes(res, pkgs, fset, projectRoot, symbol)
	case Subtypes:
		return querySubtypes(res, pkgs, fset, projectRoot, symbol)
	default:
		res.Status = "error"
		res.Hint = fmt.Sprintf("unknown action %q; valid: references, implementations, supertypes, subtypes", action)
		return res, nil
	}
}

// resolveObject finds the types.Object for symbol in pkgs.
// Supports: bare name, (*Recv).Method or (Recv).Method, pkg.Name.
func resolveObject(pkgs []*packages.Package, symbol string) types.Object {
	// Receiver-qualified: (*T).M or (T).M
	if idx := strings.Index(symbol, ")."); idx > 0 {
		recv := strings.TrimPrefix(symbol[:idx], "(")
		recv = strings.TrimPrefix(recv, "*")
		meth := symbol[idx+2:]
		for _, p := range pkgs {
			if p.Types == nil {
				continue
			}
			scope := p.Types.Scope()
			for _, name := range scope.Names() {
				obj := scope.Lookup(name)
				if obj == nil {
					continue
				}
				named, ok := obj.Type().(*types.Named)
				if !ok {
					continue
				}
				typeName := named.Obj().Name()
				tail := pkgTail(p.PkgPath)
				if typeName != recv && tail+"."+typeName != recv {
					continue
				}
				// named.NumMethods returns the canonical *types.Func objects —
				// the same pointers stored in TypesInfo.Defs/Uses. Method-set
				// selection can introduce indirection for promoted methods.
				for i := 0; i < named.NumMethods(); i++ {
					m := named.Method(i)
					if m.Name() == meth {
						return m
					}
				}
			}
		}
		return nil
	}

	// Package-tail-qualified: pkg.Name
	var pkgFilter, bareName string
	if dot := strings.LastIndex(symbol, "."); dot >= 0 {
		pkgFilter = symbol[:dot]
		bareName = symbol[dot+1:]
	} else {
		bareName = symbol
	}

	var candidates []types.Object
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		if pkgFilter != "" {
			tail := pkgTail(p.PkgPath)
			if tail != pkgFilter && p.Types.Name() != pkgFilter {
				continue
			}
		}
		if obj := p.Types.Scope().Lookup(bareName); obj != nil {
			candidates = append(candidates, obj)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return candidates[0] // first match; ambiguity is rare in practice
}

// queryReferences returns all definition and use sites for symbol.
func queryReferences(res Result, pkgs []*packages.Package, fset *token.FileSet, projectRoot, symbol string) (Result, error) {
	target := resolveObject(pkgs, symbol)
	if target == nil {
		res.Status = "not-found"
		res.Hint = fmt.Sprintf("symbol %q not found in the module", symbol)
		return res, nil
	}

	seen := map[string]bool{}
	var sites []Site
	add := func(pos token.Pos, kind string) {
		p := fset.Position(pos)
		if !p.IsValid() {
			return
		}
		key := fmt.Sprintf("%s:%d:%s", p.Filename, p.Line, kind)
		if seen[key] {
			return
		}
		seen[key] = true
		sites = append(sites, Site{
			Path: relPath(projectRoot, p.Filename),
			Line: p.Line,
			Kind: kind,
		})
	}

	for _, p := range pkgs {
		if p.TypesInfo == nil {
			continue
		}
		for ident, obj := range p.TypesInfo.Defs {
			if obj == target {
				add(ident.Pos(), "def")
			}
		}
		for ident, obj := range p.TypesInfo.Uses {
			if obj == target {
				add(ident.Pos(), "use")
			}
		}
	}
	sortSites(sites)
	res.Status = "ok"
	res.Sites = sites
	return res, nil
}

// queryImplementations returns concrete types that implement the named interface.
func queryImplementations(res Result, pkgs []*packages.Package, fset *token.FileSet, projectRoot, symbol string) (Result, error) {
	target := resolveObject(pkgs, symbol)
	if target == nil {
		res.Status = "not-found"
		res.Hint = fmt.Sprintf("symbol %q not found in the module", symbol)
		return res, nil
	}
	named, ok := target.Type().(*types.Named)
	if !ok {
		res.Status = "error"
		res.Hint = "implementations: symbol must be a named type"
		return res, nil
	}
	iface, ok := named.Underlying().(*types.Interface)
	if !ok {
		res.Status = "error"
		res.Hint = "implementations: symbol is not an interface — use subtypes to find embedding structs"
		return res, nil
	}
	if iface.NumMethods() == 0 {
		res.Status = "ok"
		res.Hint = "empty interface — every type implements it"
		return res, nil
	}

	seen := map[*types.Named]bool{}
	var sites []Site
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			n, ok := tn.Type().(*types.Named)
			if !ok || seen[n] || n == named {
				continue
			}
			if _, isIface := n.Underlying().(*types.Interface); isIface {
				continue // concrete implementors only
			}
			seen[n] = true
			if types.Implements(n, iface) || types.Implements(types.NewPointer(n), iface) {
				pos := fset.Position(n.Obj().Pos())
				sites = append(sites, Site{
					Path: relPath(projectRoot, pos.Filename),
					Line: pos.Line,
					Kind: "implementor",
				})
			}
		}
	}
	sortSites(sites)
	res.Status = "ok"
	res.Sites = sites
	return res, nil
}

// querySupertypes returns the interfaces embedded by an interface, or
// the interfaces (within the module) implemented by a concrete type.
func querySupertypes(res Result, pkgs []*packages.Package, fset *token.FileSet, projectRoot, symbol string) (Result, error) {
	target := resolveObject(pkgs, symbol)
	if target == nil {
		res.Status = "not-found"
		res.Hint = fmt.Sprintf("symbol %q not found in the module", symbol)
		return res, nil
	}
	named, ok := target.Type().(*types.Named)
	if !ok {
		res.Status = "error"
		res.Hint = "supertypes: symbol must be a named type"
		return res, nil
	}

	var sites []Site
	if iface, ok := named.Underlying().(*types.Interface); ok {
		// For an interface: return directly embedded interfaces.
		for i := 0; i < iface.NumEmbeddeds(); i++ {
			emb := iface.EmbeddedType(i)
			if embNamed, ok := emb.(*types.Named); ok {
				pos := fset.Position(embNamed.Obj().Pos())
				sites = append(sites, Site{
					Path: relPath(projectRoot, pos.Filename),
					Line: pos.Line,
					Kind: "super",
				})
			}
		}
	} else {
		// For a concrete type: interfaces within this module that it implements.
		seen := map[*types.Named]bool{}
		for _, p := range pkgs {
			if p.Types == nil {
				continue
			}
			scope := p.Types.Scope()
			for _, name := range scope.Names() {
				tn, ok := scope.Lookup(name).(*types.TypeName)
				if !ok {
					continue
				}
				n, ok := tn.Type().(*types.Named)
				if !ok || seen[n] || n == named {
					continue
				}
				ifaceType, ok := n.Underlying().(*types.Interface)
				if !ok || ifaceType.NumMethods() == 0 {
					continue
				}
				seen[n] = true
				if types.Implements(named, ifaceType) || types.Implements(types.NewPointer(named), ifaceType) {
					pos := fset.Position(n.Obj().Pos())
					sites = append(sites, Site{
						Path: relPath(projectRoot, pos.Filename),
						Line: pos.Line,
						Kind: "super",
					})
				}
			}
		}
	}
	sortSites(sites)
	res.Status = "ok"
	res.Sites = sites
	return res, nil
}

// querySubtypes returns types implementing this interface, or structs embedding this type.
func querySubtypes(res Result, pkgs []*packages.Package, fset *token.FileSet, projectRoot, symbol string) (Result, error) {
	target := resolveObject(pkgs, symbol)
	if target == nil {
		res.Status = "not-found"
		res.Hint = fmt.Sprintf("symbol %q not found in the module", symbol)
		return res, nil
	}
	named, ok := target.Type().(*types.Named)
	if !ok {
		res.Status = "error"
		res.Hint = "subtypes: symbol must be a named type"
		return res, nil
	}

	_, isIface := named.Underlying().(*types.Interface)
	seen := map[*types.Named]bool{}
	var sites []Site

	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			n, ok := tn.Type().(*types.Named)
			if !ok || seen[n] || n == named {
				continue
			}
			seen[n] = true

			var match bool
			if isIface {
				iface, ok := named.Underlying().(*types.Interface)
				if !ok {
					continue // should not happen: isIface guards this
				}
				if subIface, ok := n.Underlying().(*types.Interface); ok {
					// Sub-interface: extends named by being a superset of its methods.
					match = types.Implements(subIface, iface) && subIface != iface
				} else {
					match = types.Implements(n, iface) || types.Implements(types.NewPointer(n), iface)
				}
			} else {
				// Concrete type: look for anonymous (embedded) field of our type.
				st, ok := n.Underlying().(*types.Struct)
				if !ok {
					continue
				}
				for i := 0; i < st.NumFields(); i++ {
					f := st.Field(i)
					if !f.Anonymous() {
						continue
					}
					ft := f.Type()
					if ptr, ok := ft.(*types.Pointer); ok {
						ft = ptr.Elem()
					}
					if embNamed, ok := ft.(*types.Named); ok && embNamed == named {
						match = true
						break
					}
				}
			}
			if match {
				pos := fset.Position(n.Obj().Pos())
				sites = append(sites, Site{
					Path: relPath(projectRoot, pos.Filename),
					Line: pos.Line,
					Kind: "sub",
				})
			}
		}
	}
	sortSites(sites)
	res.Status = "ok"
	res.Sites = sites
	return res, nil
}

func sortSites(sites []Site) {
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].Path != sites[j].Path {
			return sites[i].Path < sites[j].Path
		}
		return sites[i].Line < sites[j].Line
	})
}

func hasGoMod(root string) bool {
	_, err := os.Stat(filepath.Join(root, "go.mod"))
	return err == nil
}

func relPath(root, abs string) string {
	if r, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(abs)
}

func pkgTail(pkgPath string) string {
	if i := strings.LastIndex(pkgPath, "/"); i >= 0 {
		return pkgPath[i+1:]
	}
	return pkgPath
}
