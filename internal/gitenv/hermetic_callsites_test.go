package gitenv_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every production helper that shells `git` must pin a hermetic environment
// (cmd.Env = gitenv.Current()/Hermetic(...)), or an inherited GIT_DIR /
// GIT_WORK_TREE — e.g. a linked worktree's hook env — can redirect the call at
// the wrong repository. That exact bug flipped a shared repo's core.bare twice
// (#212/#341, project-dex-bare-repo-index-wipe-hazard). This test scans the
// module's source so a new un-scrubbed git helper is caught at CI time (#833).
//
// The check is two rules over each function that constructs exec.Command("git",
// …): it must assign a .Env field, and that assignment must not be a raw
// os.Environ() (which carries the injected vars straight through).
func TestEveryGitExecPinsHermeticEnv(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	var sites int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(root, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if !funcHasGitExec(fn.Body) {
				continue
			}
			sites++
			hasEnv, rawEnviron := envAssignment(fn.Body)
			rel, _ := filepath.Rel(root, path)
			where := rel + ":" + fnName(fn)
			if !hasEnv {
				t.Errorf("%s shells git but never sets cmd.Env — pin a hermetic "+
					"environment with `cmd.Env = gitenv.Current()`", where)
			}
			if rawEnviron {
				t.Errorf("%s sets cmd.Env from a raw os.Environ() — an injected "+
					"GIT_DIR/GIT_WORK_TREE leaks through; route it via "+
					"`gitenv.Current()` / `gitenv.Hermetic(...)`", where)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if sites == 0 {
		t.Fatal("scan matched no git exec sites — the AST matcher is broken")
	}
	t.Logf("checked %d git exec call sites", sites)
}

// funcHasGitExec reports whether body constructs exec.Command/CommandContext
// with a literal "git" program name (descends into nested closures).
func funcHasGitExec(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "exec" {
			return true
		}
		var prog ast.Expr
		switch sel.Sel.Name {
		case "Command":
			if len(call.Args) > 0 {
				prog = call.Args[0]
			}
		case "CommandContext":
			if len(call.Args) > 1 {
				prog = call.Args[1]
			}
		default:
			return true
		}
		if isStringLit(prog, "git") {
			found = true
		}
		return true
	})
	return found
}

// envAssignment reports whether body assigns a .Env field, and whether any such
// assignment's RHS is a raw os.Environ() / append(os.Environ(), …).
func envAssignment(body *ast.BlockStmt) (hasEnv, rawEnviron bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Env" {
				continue
			}
			hasEnv = true
			if i < len(assign.Rhs) && isRawEnviron(assign.Rhs[i]) {
				rawEnviron = true
			}
		}
		return true
	})
	return hasEnv, rawEnviron
}

// isRawEnviron matches os.Environ() and append(os.Environ(), …) — environments
// that still carry inherited GIT_* vars.
func isRawEnviron(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	if isSelectorCall(call, "os", "Environ") {
		return true
	}
	if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "append" {
		for _, a := range call.Args {
			if ac, ok := a.(*ast.CallExpr); ok && isSelectorCall(ac, "os", "Environ") {
				return true
			}
		}
	}
	return false
}

func isSelectorCall(call *ast.CallExpr, pkg, fn string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg && sel.Sel.Name == fn
}

func isStringLit(e ast.Expr, want string) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	return strings.Trim(lit.Value, "`\"") == want
}

func fnName(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		return "(recv)." + fn.Name.Name
	}
	return fn.Name.Name
}

// skipDir excludes .git, vendor, node_modules and any nested module (a non-root
// directory carrying its own go.mod, e.g. the skew bench fixture).
func skipDir(root, path, name string) bool {
	switch name {
	case ".git", "vendor", "node_modules", "testdata":
		return true
	}
	if path == root {
		return false
	}
	if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
		return true
	}
	return false
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test dir")
		}
		dir = parent
	}
}
