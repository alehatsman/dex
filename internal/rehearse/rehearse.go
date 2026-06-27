// Package rehearse type-checks a hypothetical edit via go/packages Overlay.
// It never writes files: the caller supplies edits, the engine applies them
// in-memory, re-type-checks the module, and returns new diagnostics +
// affected files + sibling tests. Read-only by design (#730).
package rehearse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// EditTriple is one in-memory file splice — the same shape refactor emits,
// so a planned rename can be rehearsed before applying.
type EditTriple struct {
	Path        string `json:"path"`       // project-relative, slash-separated
	StartByte   int    `json:"start_byte"` // 0-based, inclusive
	EndByte     int    `json:"end_byte"`   // 0-based, exclusive
	Replacement string `json:"replacement"`
}

// WholeFile replaces a file's entire content in the overlay. Takes precedence
// over EditTriple entries for the same path.
type WholeFile struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

// Input drives Rehearse.
type Input struct {
	Edits []EditTriple `json:"edits,omitempty"`
	Files []WholeFile  `json:"files,omitempty"`
}

// Diagnostic is one type error the hypothetical introduces.
type Diagnostic struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Message string `json:"message"`
}

// Result carries the rehearsal outcome.
type Result struct {
	Status      string       `json:"status"` // ok | error | unsupported-language | no-edits
	Hint        string       `json:"hint,omitempty"`
	Compiles    bool         `json:"compiles"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	BrokenFiles []string     `json:"broken_files,omitempty"`
	TestsToRun  []string     `json:"tests_to_run,omitempty"`
	OverlayEtag string       `json:"overlay_etag,omitempty"`
}

// Rehearse applies the hypothetical in-memory and reports new type errors.
// It never writes projectRoot — the overlay lives only in memory.
func Rehearse(ctx context.Context, projectRoot string, in Input) (Result, error) {
	if !hasGoMod(projectRoot) {
		return Result{
			Status: "unsupported-language",
			Hint:   "rehearse v1 is Go-only and requires a go.mod at the project root",
		}, nil
	}
	if len(in.Edits) == 0 && len(in.Files) == 0 {
		return Result{Status: "no-edits", Hint: "supply at least one edit or file"}, nil
	}

	overlay, etag, err := buildOverlay(projectRoot, in)
	if err != nil {
		return Result{Status: "error", Hint: fmt.Sprintf("build overlay: %v", err)}, nil
	}

	// Baseline: type-check the real tree to diff out pre-existing errors.
	baseDiags, err := typeCheckDiags(ctx, projectRoot, nil)
	if err != nil {
		return Result{Status: "error", Hint: fmt.Sprintf("baseline type-check: %v", err)}, nil
	}

	// Hypothetical: type-check the in-memory overlay.
	hypoDiags, err := typeCheckDiags(ctx, projectRoot, overlay)
	if err != nil {
		return Result{Status: "error", Hint: fmt.Sprintf("hypothetical type-check: %v", err)}, nil
	}

	newDiags := diffDiags(baseDiags, hypoDiags)
	brokenFiles := uniquePaths(newDiags)

	return Result{
		Status:      "ok",
		Compiles:    len(newDiags) == 0,
		Diagnostics: newDiags,
		BrokenFiles: brokenFiles,
		TestsToRun:  siblingTests(brokenFiles),
		OverlayEtag: etag,
	}, nil
}

// buildOverlay constructs the go/packages Overlay map.
// overlayAbs resolves a caller-supplied path to an absolute path under root.
// It handles:
//   - relative paths:              "internal/foo.go"
//   - project-relative with slash: "/internal/foo.go"  (#767)
//   - fully absolute under root:   "/home/user/proj/internal/foo.go"  (#792)
func overlayAbs(root, p string) string {
	if filepath.IsAbs(p) && strings.HasPrefix(p, root+string(filepath.Separator)) {
		p = p[len(root)+1:]
	}
	return filepath.Join(root, filepath.FromSlash(strings.TrimLeft(p, "/")))
}

// WholeFile entries take precedence; EditTriples are spliced into the real file.
func buildOverlay(root string, in Input) (map[string][]byte, string, error) {
	overlay := make(map[string][]byte)

	// Whole-file replacements first.
	for _, wf := range in.Files {
		abs := overlayAbs(root, wf.Path)
		overlay[abs] = []byte(wf.Contents)
	}

	// Splice edits grouped by file, applied highest-offset-first.
	byFile := map[string][]EditTriple{}
	for _, e := range in.Edits {
		abs := overlayAbs(root, e.Path)
		if _, already := overlay[abs]; already {
			continue // WholeFile wins
		}
		byFile[abs] = append(byFile[abs], e)
	}
	for abs, edits := range byFile {
		src, err := os.ReadFile(abs)
		if err != nil {
			return nil, "", fmt.Errorf("read %s: %w", abs, err)
		}
		sort.Slice(edits, func(i, j int) bool {
			return edits[i].StartByte > edits[j].StartByte
		})
		b := append([]byte(nil), src...) // copy
		for _, e := range edits {
			if e.StartByte < 0 || e.EndByte > len(b) || e.StartByte > e.EndByte {
				return nil, "", fmt.Errorf("edit %s [%d:%d] out of range (file len %d)",
					e.Path, e.StartByte, e.EndByte, len(b))
			}
			rep := []byte(e.Replacement)
			b = append(b[:e.StartByte], append(rep, b[e.EndByte:]...)...)
		}
		overlay[abs] = b
	}

	h := sha256.New()
	paths := make([]string, 0, len(overlay))
	for p := range overlay {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		_, _ = fmt.Fprintf(h, "%s\n", p)
		h.Write(overlay[p])
	}
	etag := hex.EncodeToString(h.Sum(nil))[:12]
	return overlay, etag, nil
}

// typeCheckDiags loads the module with go/packages and returns type errors.
// overlay=nil means the real working tree.
func typeCheckDiags(ctx context.Context, root string, overlay map[string][]byte) ([]Diagnostic, error) {
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps,
		Dir:     root,
		Context: ctx,
		Fset:    fset,
		Overlay: overlay,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, err
	}

	var diags []Diagnostic
	for _, pkg := range pkgs {
		// pkg.TypeErrors is populated when NeedTypes is set (x/tools v0.42).
		for _, e := range pkg.TypeErrors {
			pos := fset.Position(e.Pos)
			rel, _ := filepath.Rel(root, pos.Filename)
			diags = append(diags, Diagnostic{
				Path:    filepath.ToSlash(rel),
				Line:    pos.Line,
				Col:     pos.Column,
				Message: e.Msg,
			})
		}
	}
	return diags, nil
}

func diagKey(d Diagnostic) string {
	return fmt.Sprintf("%s:%d:%d:%s", d.Path, d.Line, d.Col, d.Message)
}

// diffDiags returns errors in hypo not in base (new errors introduced by the edit).
func diffDiags(base, hypo []Diagnostic) []Diagnostic {
	baseSet := make(map[string]bool, len(base))
	for _, d := range base {
		baseSet[diagKey(d)] = true
	}
	var out []Diagnostic
	for _, d := range hypo {
		if !baseSet[diagKey(d)] {
			out = append(out, d)
		}
	}
	return out
}

func uniquePaths(diags []Diagnostic) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range diags {
		if !seen[d.Path] {
			seen[d.Path] = true
			out = append(out, d.Path)
		}
	}
	sort.Strings(out)
	return out
}

func siblingTests(files []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		if len(f) < 4 || f[len(f)-3:] != ".go" {
			continue
		}
		var tf string
		if len(f) >= 8 && f[len(f)-8:] == "_test.go" {
			tf = f
		} else {
			tf = f[:len(f)-3] + "_test.go"
		}
		if !seen[tf] {
			seen[tf] = true
			out = append(out, tf)
		}
	}
	sort.Strings(out)
	return out
}

func hasGoMod(root string) bool {
	_, err := os.Stat(filepath.Join(root, "go.mod"))
	return err == nil
}
