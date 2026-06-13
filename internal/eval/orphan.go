package eval

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// OrphanOpts configures GenerateOrphan.
type OrphanOpts struct {
	MaxFiles   int `json:"max_files"`    // 0 = all
	MaxPerKind int `json:"max_per_kind"` // 0 = 50
}

// OrphanGenCounts records how many queries were produced per kind.
type OrphanGenCounts struct {
	Imports int `json:"imports"`
	Consts  int `json:"consts"`
	Vars    int `json:"vars"`
}

// GenerateOrphan mines Go source files under root for package-level
// import, const, and var declarations and emits GoldenQuery entries
// whose ground truth is the file containing each declaration. These
// target the "orphan" chunks that structural commit-based queries miss.
func GenerateOrphan(ctx context.Context, root string, opts OrphanOpts) (GoldenSet, OrphanGenCounts, error) {
	if opts.MaxPerKind <= 0 {
		opts.MaxPerKind = 50
	}

	head, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return GoldenSet{}, OrphanGenCounts{}, fmt.Errorf("eval: read HEAD: %w", err)
	}

	goFiles, err := collectGoFiles(root, opts.MaxFiles)
	if err != nil {
		return GoldenSet{}, OrphanGenCounts{}, fmt.Errorf("eval: walk: %w", err)
	}

	seen := map[string]bool{} // deduplicate query text
	var queries []GoldenQuery
	var counts OrphanGenCounts

	kindCounts := map[string]int{"import": 0, "const": 0, "var": 0}

	appendValSpecQueries := func(gd *ast.GenDecl, pos token.Position, relPath, kind string, queryFn func(string) string, counter *int) {
		if kindCounts[kind] >= opts.MaxPerKind {
			return
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if kindCounts[kind] >= opts.MaxPerKind {
					break
				}
				q := queryFn(name.Name)
				if q == "" || seen[q] {
					continue
				}
				seen[q] = true
				kindCounts[kind]++
				*counter++
				queries = append(queries, GoldenQuery{
					ID:            fmt.Sprintf("%s:%d:%s", relPath, pos.Line, name.Name),
					Query:         q,
					RelevantFiles: []string{relPath},
				})
			}
		}
	}

	fset := token.NewFileSet()
	for _, relPath := range goFiles {
		if err := ctx.Err(); err != nil {
			break
		}
		absPath := filepath.Join(root, relPath)
		f, err := parser.ParseFile(fset, absPath, nil, 0)
		if err != nil {
			continue
		}
		pkgName := f.Name.Name

		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			pos := fset.Position(gd.Pos())
			id := fmt.Sprintf("%s:%d", relPath, pos.Line)

			switch gd.Tok {
			case token.IMPORT:
				if kindCounts["import"] >= opts.MaxPerKind {
					continue
				}
				q := importQuery(relPath, gd, pkgName)
				if q == "" || seen[q] {
					continue
				}
				seen[q] = true
				kindCounts["import"]++
				counts.Imports++
				queries = append(queries, GoldenQuery{
					ID:            id,
					Query:         q,
					RelevantFiles: []string{relPath},
				})

			case token.CONST:
				appendValSpecQueries(gd, pos, relPath, "const", constQuery, &counts.Consts)

			case token.VAR:
				appendValSpecQueries(gd, pos, relPath, "var", varQuery, &counts.Vars)
			}
		}
	}

	return GoldenSet{
		Repo:    filepath.Base(root),
		Head:    head,
		Queries: queries,
	}, counts, nil
}

// importQuery builds a query for the import block of the file at relPath.
// It prefixes the source file stem so that "eval store import" targets the
// eval.go importer rather than the store package files themselves. Returns ""
// when no suitable non-stdlib import is found.
func importQuery(relPath string, gd *ast.GenDecl, pkgName string) string {
	var distinctive []string
	for _, spec := range gd.Specs {
		is, ok := spec.(*ast.ImportSpec)
		if !ok {
			continue
		}
		path := strings.Trim(is.Path.Value, `"`)
		if isStdlibImport(path) {
			continue
		}
		// Use the last path element as the package name.
		parts := strings.Split(path, "/")
		leaf := parts[len(parts)-1]
		// Skip version suffixes (v2, v3, ...) and empty leaves.
		if leaf == "" || (len(leaf) == 2 && leaf[0] == 'v' && leaf[1] >= '2') {
			if len(parts) > 1 {
				leaf = parts[len(parts)-2]
			}
		}
		leaf = strings.TrimLeft(leaf, "_")
		if leaf != "" && leaf != pkgName {
			distinctive = append(distinctive, leaf)
		}
	}
	if len(distinctive) == 0 {
		return ""
	}
	sort.Strings(distinctive)
	// Pick the most distinctive (longest) import name as the anchor.
	best := distinctive[0]
	for _, d := range distinctive {
		if len(d) > len(best) {
			best = d
		}
	}
	// Prefix the source file stem so the query targets the importer, not
	// the imported package (e.g. "eval store import" vs "store import").
	stem := strings.TrimSuffix(filepath.Base(relPath), ".go")
	if stem == strings.ToLower(best) {
		// Stem equals import leaf — pick next best to avoid redundant "X X import".
		var next string
		for _, d := range distinctive {
			if d != best {
				next = d
				break
			}
		}
		if next == "" {
			return ""
		}
		best = next
	}
	return stem + " " + strings.ToLower(best) + " import"
}

// constQuery produces a query for an exported or lowercase constant.
func constQuery(name string) string {
	// Skip auto-generated iota sequences and blank identifiers.
	if name == "_" || name == "iota" {
		return ""
	}
	words := splitIdent(name)
	if len(words) == 0 {
		return ""
	}
	return strings.Join(words, " ") + " constant"
}

// varQuery produces a query for a package-level variable.
func varQuery(name string) string {
	if name == "_" {
		return ""
	}
	words := splitIdent(name)
	if len(words) == 0 {
		return ""
	}
	// Classify error sentinels separately.
	if len(words) > 0 && words[0] == "err" {
		return strings.Join(words[1:], " ") + " error sentinel"
	}
	return strings.Join(words, " ") + " variable"
}

// splitIdent splits a camelCase / PascalCase / snake_case identifier
// into lowercase words. E.g. "MaxRetries" → ["max", "retries"].
func splitIdent(s string) []string {
	// First handle snake_case splits.
	parts := strings.Split(s, "_")
	var words []string
	for _, part := range parts {
		if part == "" {
			continue
		}
		// Split part on camelCase boundaries.
		runes := []rune(part)
		start := 0
		for i := 1; i < len(runes); i++ {
			if unicode.IsUpper(runes[i]) && unicode.IsLower(runes[i-1]) {
				words = append(words, strings.ToLower(string(runes[start:i])))
				start = i
			}
		}
		words = append(words, strings.ToLower(string(runes[start:])))
	}
	// Filter single-char or all-digit tokens.
	var out []string
	for _, w := range words {
		if len(w) >= 2 && !isAllDigits(w) {
			out = append(out, w)
		}
	}
	return out
}

// isAllDigits reports whether s consists entirely of ASCII digits.
func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isStdlibImport reports whether an import path looks like a Go stdlib
// package (no dot in the first path element).
func isStdlibImport(path string) bool {
	first := strings.SplitN(path, "/", 2)[0]
	return !strings.Contains(first, ".")
}

// collectGoFiles walks root and returns relative paths to non-test Go files,
// capped at maxFiles (0 = no cap). Vendor and hidden dirs are skipped.
func collectGoFiles(root string, maxFiles int) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, rel)
		if maxFiles > 0 && len(out) >= maxFiles {
			return filepath.SkipAll
		}
		return nil
	})
	return out, err
}
