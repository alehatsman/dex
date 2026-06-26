package refactor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// recvMethodRe matches a receiver-qualified method: "(*T).M" or "(T).M".
var recvMethodRe = regexp.MustCompile(`^\(\*?([\w.]+)\)\.(\w+)$`)

// PlanRename plans a type-precise rename of symbol → to across the Go module
// rooted at projectRoot. It never writes files: it returns the edit triples for
// the host agent to apply. When etag is non-empty and does not match the current
// touched-file hash, it returns status "stale" so a plan computed against older
// content is never applied blindly.
func PlanRename(ctx context.Context, projectRoot, symbol, to, etag string) (RenameResult, error) {
	res := RenameResult{From: symbol, To: to}

	if !token.IsIdentifier(to) || token.IsKeyword(to) {
		res.Status = "error"
		res.Hint = fmt.Sprintf("%q is not a valid Go identifier", to)
		return res, nil
	}
	if !hasGoMod(projectRoot) {
		res.Status = "unsupported-language"
		res.Hint = "rename_symbol v1 is Go-only and needs a go.mod at the project root"
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

	target, desc, status, hint := resolveTarget(pkgs, symbol)
	if status != "ok" {
		res.Status = status
		res.Hint = hint
		return res, nil
	}
	res.Object = desc

	// Renaming to a name already declared in the target's scope is likely a
	// conflict — warn but still emit, leaving the call to the agent (v1 has no
	// compile precheck; that's the deferred verify:true path).
	if scopeHasName(target, to) {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("%q already exists in the target scope — applying may shadow or collide", to))
	}

	edits := collectEdits(pkgs, fset, target, to, projectRoot)
	if len(edits) == 0 {
		res.Status = "not-found"
		res.Hint = "resolved the symbol but found no identifiers to rewrite (already renamed?)"
		return res, nil
	}
	sortEdits(edits)
	res.Edits = edits
	res.Files = distinctFiles(edits)

	computed, herr := hashTouchedFiles(projectRoot, edits)
	if herr != nil {
		res.Status = "error"
		res.Hint = fmt.Sprintf("hash files: %v", herr)
		return res, nil
	}
	res.Etag = computed
	if etag != "" && etag != computed {
		res.Status = "stale"
		res.Hint = "files changed since the plan etag was issued — re-plan before applying"
		return res, nil
	}

	res.Status = "ok"
	return res, nil
}

// resolveTarget turns a symbol string into its types.Object. It recognises
// receiver-qualified methods ("(*T).M"), package-tail-qualified names
// ("pkg.Name"), and bare names ("Name"). Returns a human description and a
// status ("ok"|"not-found"|"ambiguous").
func resolveTarget(pkgs []*packages.Package, symbol string) (types.Object, string, string, string) {
	symbol = strings.TrimSpace(symbol)

	if m := recvMethodRe.FindStringSubmatch(symbol); m != nil {
		typeName, method := m[1], m[2]
		// typeName may be pkg-tail-qualified (pkg.T) — take the trailing type.
		if i := strings.LastIndex(typeName, "."); i >= 0 {
			typeName = typeName[i+1:]
		}
		obj := lookupMethod(pkgs, typeName, method)
		if obj == nil {
			return nil, "", "not-found", fmt.Sprintf("no method %q on type %q", method, typeName)
		}
		return obj, fmt.Sprintf("method %s", symbol), "ok", ""
	}

	if i := strings.LastIndex(symbol, "."); i >= 0 {
		left, right := symbol[:i], symbol[i+1:]
		// Try package-tail-qualified first (left is a package name).
		if obj := lookupTopLevelInPkg(pkgs, left, right); obj != nil {
			return obj, describeObj(obj), "ok", ""
		}
		// Else treat left as a type name: a method or field of that type.
		if obj := lookupMethod(pkgs, left, right); obj != nil {
			return obj, fmt.Sprintf("method %s.%s", left, right), "ok", ""
		}
		if obj := lookupField(pkgs, left, right); obj != nil {
			return obj, fmt.Sprintf("field %s.%s", left, right), "ok", ""
		}
		return nil, "", "not-found", fmt.Sprintf("could not resolve %q (not a pkg-qualified name, method, or field)", symbol)
	}

	// Bare name: search top-level scopes, then methods, across all packages.
	var matches []types.Object
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		if obj := p.Types.Scope().Lookup(symbol); obj != nil {
			matches = appendUniqueObj(matches, obj)
		}
	}
	if len(matches) == 1 {
		return matches[0], describeObj(matches[0]), "ok", ""
	}
	if len(matches) > 1 {
		return nil, "", "ambiguous", fmt.Sprintf("%q is declared in %d packages — qualify it as pkg.%s", symbol, len(matches), symbol)
	}
	// Fall back to a method search by bare name.
	methods := lookupMethodsByName(pkgs, symbol)
	if len(methods) == 1 {
		return methods[0], "method " + symbol, "ok", ""
	}
	if len(methods) > 1 {
		return nil, "", "ambiguous", fmt.Sprintf("%q names %d methods — qualify it as (*T).%s", symbol, len(methods), symbol)
	}
	return nil, "", "not-found", fmt.Sprintf("no top-level symbol or method named %q", symbol)
}

// lookupTopLevelInPkg finds a top-level object `name` in the package whose name
// or path-tail equals pkgName.
func lookupTopLevelInPkg(pkgs []*packages.Package, pkgName, name string) types.Object {
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		if p.Types.Name() == pkgName || pkgTail(p.PkgPath) == pkgName {
			if obj := p.Types.Scope().Lookup(name); obj != nil {
				return obj
			}
		}
	}
	return nil
}

// lookupMethod finds method `method` on the named type `typeName` (in any
// package). Covers value- and pointer-receiver methods.
func lookupMethod(pkgs []*packages.Package, typeName, method string) types.Object {
	for _, named := range namedTypes(pkgs, typeName) {
		for i := 0; i < named.NumMethods(); i++ {
			if m := named.Method(i); m.Name() == method {
				return m
			}
		}
	}
	return nil
}

// lookupField finds struct field `field` on the named struct `typeName`.
func lookupField(pkgs []*packages.Package, typeName, field string) types.Object {
	for _, named := range namedTypes(pkgs, typeName) {
		st, ok := named.Underlying().(*types.Struct)
		if !ok {
			continue
		}
		for i := 0; i < st.NumFields(); i++ {
			if f := st.Field(i); f.Name() == field {
				return f
			}
		}
	}
	return nil
}

// lookupMethodsByName returns every method named `name` across all named types.
func lookupMethodsByName(pkgs []*packages.Package, name string) []types.Object {
	var out []types.Object
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, sn := range scope.Names() {
			tn, ok := scope.Lookup(sn).(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok {
				continue
			}
			for i := 0; i < named.NumMethods(); i++ {
				if m := named.Method(i); m.Name() == name {
					out = appendUniqueObj(out, m)
				}
			}
		}
	}
	return out
}

// namedTypes returns every *types.Named with the given name across all packages.
func namedTypes(pkgs []*packages.Package, typeName string) []*types.Named {
	var out []*types.Named
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		if tn, ok := p.Types.Scope().Lookup(typeName).(*types.TypeName); ok {
			if named, ok := tn.Type().(*types.Named); ok {
				out = append(out, named)
			}
		}
	}
	return out
}

// collectEdits finds every identifier that refers to target (its definition and
// all uses) across all loaded packages and turns each into an EditTriple.
// Matching is by types.Object identity, so a method rename touches only that
// exact method — not same-named methods on other types (type-resolved).
func collectEdits(pkgs []*packages.Package, fset *token.FileSet, target types.Object, to, root string) []EditTriple {
	seen := map[string]bool{} // dedupe by abs-path:offset across package variants
	var edits []EditTriple

	record := func(id *ast.Ident) {
		startPos := fset.Position(id.Pos())
		key := startPos.Filename + ":" + fmt.Sprint(startPos.Offset)
		if seen[key] {
			return
		}
		seen[key] = true
		rel := startPos.Filename
		if r, err := filepath.Rel(root, startPos.Filename); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		edits = append(edits, EditTriple{
			Path:        filepath.ToSlash(rel),
			StartByte:   startPos.Offset,
			EndByte:     fset.Position(id.End()).Offset,
			Replacement: to,
			Line:        startPos.Line,
		})
	}

	for _, p := range pkgs {
		if p.TypesInfo == nil {
			continue
		}
		for id, obj := range p.TypesInfo.Defs {
			if obj == target {
				record(id)
			}
		}
		for id, obj := range p.TypesInfo.Uses {
			if obj == target {
				record(id)
			}
		}
	}
	return edits
}

// scopeHasName reports whether `name` is already declared in target's package
// scope (a cheap collision signal for top-level renames).
func scopeHasName(target types.Object, name string) bool {
	if target == nil || target.Pkg() == nil {
		return false
	}
	return target.Pkg().Scope().Lookup(name) != nil
}

func describeObj(obj types.Object) string {
	switch obj.(type) {
	case *types.Func:
		return "func " + obj.Name()
	case *types.TypeName:
		return "type " + obj.Name()
	case *types.Var:
		return "var " + obj.Name()
	case *types.Const:
		return "const " + obj.Name()
	default:
		return obj.Name()
	}
}

func appendUniqueObj(s []types.Object, obj types.Object) []types.Object {
	for _, o := range s {
		if o == obj {
			return s
		}
	}
	return append(s, obj)
}

func distinctFiles(edits []EditTriple) int {
	set := map[string]bool{}
	for _, e := range edits {
		set[e.Path] = true
	}
	return len(set)
}

func sortEdits(edits []EditTriple) {
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].Path != edits[j].Path {
			return edits[i].Path < edits[j].Path
		}
		return edits[i].StartByte < edits[j].StartByte
	})
}

// hashTouchedFiles returns a stable hash over the current content of every file
// the plan touches — the plan's etag. Applying against changed content yields a
// different etag, so a stale plan is detectable.
func hashTouchedFiles(root string, edits []EditTriple) (string, error) {
	files := map[string]bool{}
	for _, e := range edits {
		files[e.Path] = true
	}
	rels := make([]string, 0, len(files))
	for f := range files {
		rels = append(rels, f)
	}
	sort.Strings(rels)
	h := sha256.New()
	for _, rel := range rels {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", rel, len(b)) // hash.Hash.Write never errors
		_, _ = h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
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
