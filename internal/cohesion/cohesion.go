// Package cohesion answers "what must change together" — the blast radius of an
// intent rather than of a symbol.
//
// It is the engine behind the `cohort` verb (#643). v1 covers interface
// cohesion: given an interface, it enumerates every type in the module that
// implements it (the set you must edit in lockstep when the interface's method
// set changes) plus near-miss types that implement most of it — the backend you
// forgot to update. Pure go/packages + go/types, no index needed (like
// refactor, #638). Convention cohesion (parity/registry sites) is deferred.
package cohesion

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

// Member is one type coupled to the queried interface.
type Member struct {
	Type    string   `json:"type"`              // qualified type name, e.g. "*mcp.Server"
	Path    string   `json:"path"`              // project-relative declaration file
	Line    int      `json:"line"`              // declaration line
	Status  string   `json:"status"`            // "complete" | "partial"
	Missing []string `json:"missing,omitempty"` // interface methods absent (partial only)
}

// CohortResult is the interface's implementor cohort.
//   - "ok"                    members is the cohort (may be empty)
//   - "unsupported-language"  no go.mod at the project root
//   - "not-found"             no interface of that name
//   - "error"                 load failure (see Hint)
type CohortResult struct {
	Status    string   `json:"status"`
	Hint      string   `json:"hint,omitempty"`
	Interface string   `json:"interface,omitempty"`
	Methods   []string `json:"methods,omitempty"` // the interface's method names
	Members   []Member `json:"members,omitempty"`
	Complete  int      `json:"complete"`
	Partial   int      `json:"partial"`
}

// ImplementorsOf finds every type in the module rooted at projectRoot that
// implements (or near-implements) the named interface. ifaceName may be bare
// ("toolSurface") or package-tail-qualified ("mcp.toolSurface").
func ImplementorsOf(ctx context.Context, projectRoot, ifaceName string) (CohortResult, error) {
	res := CohortResult{Interface: ifaceName}
	if !hasGoMod(projectRoot) {
		res.Status = "unsupported-language"
		res.Hint = "cohort v1 is Go-only and needs a go.mod at the project root"
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

	iface, ifacePkg, ok := findInterface(pkgs, ifaceName)
	if !ok {
		res.Status = "not-found"
		res.Hint = fmt.Sprintf("no interface named %q in the module", ifaceName)
		return res, nil
	}

	// The interface's full method set (includes embedded interfaces).
	type ifaceMethod struct {
		name string
		sig  *types.Signature
	}
	var methods []ifaceMethod
	for i := 0; i < iface.NumMethods(); i++ {
		m := iface.Method(i)
		sig, _ := m.Type().(*types.Signature) // a func's type is always a signature
		methods = append(methods, ifaceMethod{m.Name(), sig})
		res.Methods = append(res.Methods, m.Name())
	}
	if len(methods) == 0 {
		res.Status = "ok"
		res.Hint = "the interface has no methods — every type trivially implements it"
		return res, nil
	}
	sort.Strings(res.Methods)

	half := (len(methods) + 1) / 2

	for _, named := range allNamedTypes(pkgs) {
		// Skip the interface itself and any interface types (we want concrete
		// implementors, not sub-interfaces).
		if _, isIface := named.Underlying().(*types.Interface); isIface {
			continue
		}
		// Use the pointer method set so pointer-receiver methods count.
		mset := types.NewMethodSet(types.NewPointer(named))
		var missing []string
		matched := 0
		sigMismatch := false
		for _, im := range methods {
			sel := mset.Lookup(ifacePkg, im.name)
			if sel == nil {
				missing = append(missing, im.name)
				continue
			}
			if sig, ok := sel.Type().(*types.Signature); ok && types.Identical(sig, im.sig) {
				matched++
			} else {
				// A method of the same name but a different signature is a
				// strong signal this type is NOT meant to implement the iface.
				sigMismatch = true
				break
			}
		}
		if sigMismatch || matched == 0 {
			continue
		}
		status := "complete"
		if len(missing) > 0 {
			// Only flag as a near-miss when it implements at least half — keeps
			// out coincidental same-named-method types.
			if matched < half {
				continue
			}
			status = "partial"
		}
		pos := fset.Position(named.Obj().Pos())
		members := Member{
			Type:    qualifiedTypeName(named),
			Path:    relPath(projectRoot, pos.Filename),
			Line:    pos.Line,
			Status:  status,
			Missing: missing,
		}
		res.Members = append(res.Members, members)
	}

	sort.Slice(res.Members, func(i, j int) bool {
		if res.Members[i].Status != res.Members[j].Status {
			return res.Members[i].Status < res.Members[j].Status // complete before partial
		}
		return res.Members[i].Type < res.Members[j].Type
	})
	for _, m := range res.Members {
		if m.Status == "complete" {
			res.Complete++
		} else {
			res.Partial++
		}
	}
	res.Status = "ok"
	return res, nil
}

// findInterface resolves ifaceName (bare or pkg-tail-qualified) to a
// *types.Interface and its declaring package.
func findInterface(pkgs []*packages.Package, ifaceName string) (*types.Interface, *types.Package, bool) {
	pkgName, name := "", ifaceName
	if i := strings.LastIndex(ifaceName, "."); i >= 0 {
		pkgName, name = ifaceName[:i], ifaceName[i+1:]
	}
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		if pkgName != "" && p.Types.Name() != pkgName && pkgTail(p.PkgPath) != pkgName {
			continue
		}
		if tn, ok := p.Types.Scope().Lookup(name).(*types.TypeName); ok {
			if iface, ok := tn.Type().Underlying().(*types.Interface); ok {
				return iface, p.Types, true
			}
		}
	}
	return nil, nil, false
}

// allNamedTypes returns every named type declared in the loaded packages,
// deduped by identity.
func allNamedTypes(pkgs []*packages.Package) []*types.Named {
	seen := map[*types.Named]bool{}
	var out []*types.Named
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, n := range scope.Names() {
			tn, ok := scope.Lookup(n).(*types.TypeName)
			if !ok {
				continue
			}
			if named, ok := tn.Type().(*types.Named); ok && !seen[named] {
				seen[named] = true
				out = append(out, named)
			}
		}
	}
	return out
}

func qualifiedTypeName(named *types.Named) string {
	obj := named.Obj()
	name := obj.Name()
	if obj.Pkg() != nil {
		name = obj.Pkg().Name() + "." + name
	}
	return "*" + name // implementors are reported by their pointer type (the satisfying set)
}

func relPath(root, abs string) string {
	if r, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(abs)
}

func hasGoMod(root string) bool {
	_, err := os.Stat(filepath.Join(root, "go.mod"))
	return err == nil
}

func pkgTail(pkgPath string) string {
	if i := strings.LastIndex(pkgPath, "/"); i >= 0 {
		return pkgPath[i+1:]
	}
	return pkgPath
}
