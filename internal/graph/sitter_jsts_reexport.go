package graph

import (
	sitter "github.com/smacker/go-tree-sitter"
)

// reExportTable records the barrel re-exports of a single module — the
// `export … from './x'` statements that forward another module's symbols
// without defining them locally. Consulted by resolveExport when a direct
// symbolIn misses, so a cross-package call landing on a barrel index.ts binds
// through to the real definition (#127 Phase 2).
//
// Specifiers are captured raw during the walk and rewritten to packagePaths in
// Finalize (mirrors fileImports), so the fields below hold raw specifiers until
// resolveReExports runs.
type reExportTable struct {
	// named: exportedName → (targetModule, originalName). Covers
	// `export { a, b as c } from './x'` ⇒ {"a": (x,"a"), "c": (x,"b")}.
	named map[string]reNamed
	// namespaces: exportedName → targetModule. Covers
	// `export * as String from './String'` ⇒ {"String": "…/String"}. A consumer's
	// `String.capitalize()` binds via the attr case.
	namespaces map[string]string
	// stars: target modules of `export * from './x'`. A name not otherwise
	// bound is searched through each, first hit wins (source order).
	stars []string
}

type reNamed struct {
	mod  string // target module: raw specifier until Finalize, then packagePath
	orig string // original exported name on the target side
}

func newReExportTable() *reExportTable {
	return &reExportTable{
		named:      map[string]reNamed{},
		namespaces: map[string]string{},
	}
}

// parseExportStatement captures a top-level `export … from './x'` re-export into
// the module's reExportTable. A local export (`export function foo`,
// `export { foo }` with no `from`) has no source and is ignored — those symbols
// already land in e.symbols via the declaration emit. Called during the walk;
// specifiers stay raw and are resolved in resolveReExports.
func (e *jstsBase) parseExportStatement(node *sitter.Node, src []byte, pkg string) {
	source := node.ChildByFieldName("source")
	if source == nil {
		return // not a re-export
	}
	spec := stripQuotes(nodeText(source, src))
	if spec == "" {
		return
	}

	var clause, nsExport *sitter.Node
	for i := 0; i < int(node.NamedChildCount()); i++ {
		switch c := node.NamedChild(i); c.Type() {
		case "export_clause":
			clause = c
		case "namespace_export":
			nsExport = c
		}
	}

	tbl := e.reExports[pkg]
	if tbl == nil {
		tbl = newReExportTable()
		e.reExports[pkg] = tbl
	}

	switch {
	case clause != nil: // export { a, b as c } from './x'
		for i := 0; i < int(clause.NamedChildCount()); i++ {
			spc := clause.NamedChild(i)
			if spc.Type() != "export_specifier" {
				continue
			}
			nameNode := spc.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			orig := nodeText(nameNode, src)
			exported := orig
			if alias := spc.ChildByFieldName("alias"); alias != nil {
				exported = nodeText(alias, src)
			}
			if orig == "" || exported == "" {
				continue
			}
			tbl.named[exported] = reNamed{mod: spec, orig: orig}
		}
	case nsExport != nil: // export * as String from './String'
		var ns string
		for i := 0; i < int(nsExport.NamedChildCount()); i++ {
			if c := nsExport.NamedChild(i); c.Type() == "identifier" {
				ns = nodeText(c, src)
				break
			}
		}
		if ns != "" {
			tbl.namespaces[ns] = spec
		}
	default: // export * from './x' (or `export type * from`, which the grammar
		// emits as an ERROR node plus a source — still a star re-export)
		tbl.stars = append(tbl.stars, spec)
	}
}

// resolveReExports rewrites every captured raw specifier to its packagePath, now
// that knownFiles is complete. Mirrors the import-specifier pass in Finalize. A
// specifier that doesn't resolve to a project file stays as its raw string and
// simply never matches a symbol bucket — no error, no fabricated edge.
func (e *jstsBase) resolveReExports() {
	for pkg, tbl := range e.reExports {
		fromFile := e.knownFiles[pkg]
		if fromFile == "" {
			continue
		}
		for exported, r := range tbl.named {
			r.mod = e.resolveModuleSpecifier(r.mod, fromFile)
			tbl.named[exported] = r
		}
		for ns, mod := range tbl.namespaces {
			tbl.namespaces[ns] = e.resolveModuleSpecifier(mod, fromFile)
		}
		for i, mod := range tbl.stars {
			tbl.stars[i] = e.resolveModuleSpecifier(mod, fromFile)
		}
	}
}

// resolveExport resolves `name` as exported by module `pkg`, following barrel
// re-exports when the module doesn't define the symbol itself. A local
// definition always wins; then named re-exports, then star re-exports (first hit
// in source order). `visited` bounds cycles and prevents re-scanning a barrel
// twice within one resolution — allocate a fresh map at each top-level call.
func (e *jstsBase) resolveExport(pkg, name string, visited map[string]bool) string {
	if id := e.symbolIn(pkg, name); id != "" {
		return id
	}
	if visited[pkg] {
		return ""
	}
	visited[pkg] = true

	tbl := e.reExports[pkg]
	if tbl == nil {
		return ""
	}
	if r, ok := tbl.named[name]; ok {
		if id := e.resolveExport(r.mod, r.orig, visited); id != "" {
			return id
		}
	}
	for _, starMod := range tbl.stars {
		if id := e.resolveExport(starMod, name, visited); id != "" {
			return id
		}
	}
	return ""
}

// namespaceTarget returns the module a namespace re-export in `pkg` binds `name`
// to (`export * as name from './mod'`), or "" when there is none. Lets the attr
// case bind `name.method()` through a barrel's namespace re-export.
func (e *jstsBase) namespaceTarget(pkg, name string) string {
	tbl := e.reExports[pkg]
	if tbl == nil {
		return ""
	}
	return tbl.namespaces[name]
}
