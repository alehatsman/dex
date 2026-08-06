// Package resolve maps non-relative JavaScript/TypeScript import specifiers to
// project-relative, extension-free module-path candidates, using the same
// config the ecosystem's dependency tools consume: package.json workspace
// names + entry points, and tsconfig/jsconfig path aliases.
//
// It is deliberately NOT a type checker and does NO node_modules resolution.
// A specifier that matches neither a path alias nor a workspace package is
// external (a bare npm dependency) and yields no candidates. The caller probes
// the returned candidates against the set of indexed files and takes the first
// hit; nothing here touches the graph, the DB, or the filesystem after Load.
//
// See specs/module-resolution.md (issue #127, Phase 1).
package resolve

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Workspace holds the resolved workspace-package map and path-alias table for a
// repository root. Build one with Load; it is read-only afterwards.
type Workspace struct {
	// packages is sorted by name length descending so the longest (most
	// specific) package name wins a prefix match — "@bright/common/sub" must
	// prefer "@bright/common" over a hypothetical "@bright".
	packages []pkgEntry
	// aliases is sorted most-specific first (see aliasLess).
	aliases []aliasRule
}

// pkgEntry is one workspace package discovered from a package.json.
type pkgEntry struct {
	name    string   // "@bright/common"
	dir     string   // "packages/bright-common" (project-relative, slash)
	entries []string // ext-free candidates for the bare-package import
}

// aliasRule is one tsconfig/jsconfig path-alias mapping, with targets already
// joined to the config's baseUrl and made project-relative + extension-free.
type aliasRule struct {
	prefix  string   // text before '*' ("@/"), or the whole pattern when exact
	suffix  string   // text after '*' (usually "")
	star    bool     // pattern contained a '*'
	targets []string // replacement templates, each may contain a single '*'
}

// jsExts are stripped from candidates so they match the extension-free
// knownFiles key space. Order matters: ".d.ts" before ".ts".
var jsExts = []string{".d.ts", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts"}

// Load scans root for package.json and tsconfig.json/jsconfig.json, skipping
// node_modules, .git, and dot-directories. Malformed or missing config is
// skipped silently — Load never fails an index; a repo with no workspace config
// simply resolves nothing (every non-relative specifier stays external).
func Load(root string) *Workspace {
	w := &Workspace{}
	if root == "" {
		return w
	}
	byName := map[string]pkgEntry{}
	var tsconfigs []string

	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable entries are skipped, not fatal
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".git" ||
				(strings.HasPrefix(name, ".") && name != "." && p != root) {
				return filepath.SkipDir
			}
			return nil
		}
		switch d.Name() {
		case "package.json":
			if e, ok := loadPackageJSON(root, p); ok {
				// First one wins for a given name (shallower paths sort first via
				// WalkDir's lexical order within a dir, but across dirs order is
				// arbitrary; a duplicate name in a monorepo is already a bug).
				if _, dup := byName[e.name]; !dup {
					byName[e.name] = e
				}
			}
		case "tsconfig.json", "jsconfig.json":
			tsconfigs = append(tsconfigs, p)
		}
		return nil
	})

	for _, e := range byName {
		w.packages = append(w.packages, e)
	}
	sort.Slice(w.packages, func(i, j int) bool {
		if len(w.packages[i].name) != len(w.packages[j].name) {
			return len(w.packages[i].name) > len(w.packages[j].name)
		}
		return w.packages[i].name < w.packages[j].name
	})

	seen := map[string]struct{}{}
	for _, tc := range tsconfigs {
		for _, r := range loadTSConfigAliases(root, tc, map[string]struct{}{}) {
			key := r.prefix + "\x00" + r.suffix + "\x00" + strings.Join(r.targets, ",")
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			w.aliases = append(w.aliases, r)
		}
	}
	sort.SliceStable(w.aliases, func(i, j int) bool { return aliasLess(w.aliases[i], w.aliases[j]) })
	return w
}

// Candidates returns project-relative, extension-free module-path candidates for
// a non-relative specifier, most-specific first. An empty result means the
// specifier is external (a bare npm dependency) — the caller treats it as such.
// Relative specifiers (leading ".") are the extractor's job and return nil here.
func (w *Workspace) Candidates(specifier string) []string {
	if w == nil || specifier == "" || strings.HasPrefix(specifier, ".") {
		return nil
	}
	var out []string
	add := func(c string) {
		c = cleanCandidate(c)
		if c == "" || slices.Contains(out, c) {
			return
		}
		out = append(out, c)
	}

	// 1. Path aliases (most specific first) — an intentional redirect wins over
	//    a workspace-name match.
	for _, a := range w.aliases {
		for _, c := range a.apply(specifier) {
			add(c)
		}
	}

	// 2. Workspace package names (longest first).
	for _, p := range w.packages {
		if specifier == p.name {
			for _, e := range p.entries {
				add(e)
			}
			continue
		}
		if strings.HasPrefix(specifier, p.name+"/") {
			sub := specifier[len(p.name)+1:]
			add(p.dir + "/" + sub)
			add(p.dir + "/src/" + sub)
		}
	}
	return out
}

// apply returns the candidate paths this alias produces for a specifier, or nil
// if the specifier doesn't match the alias pattern.
func (a aliasRule) apply(specifier string) []string {
	if !a.star {
		if specifier != a.prefix {
			return nil
		}
		return append([]string(nil), a.targets...)
	}
	if !strings.HasPrefix(specifier, a.prefix) || !strings.HasSuffix(specifier, a.suffix) ||
		len(specifier) < len(a.prefix)+len(a.suffix) {
		return nil
	}
	star := specifier[len(a.prefix) : len(specifier)-len(a.suffix)]
	out := make([]string, 0, len(a.targets))
	for _, t := range a.targets {
		out = append(out, strings.Replace(t, "*", star, 1))
	}
	return out
}

// aliasLess orders alias rules most-specific first: exact patterns before glob
// patterns, then by longer prefix, then longer suffix, then lexical.
func aliasLess(x, y aliasRule) bool {
	if x.star != y.star {
		return !x.star // exact (star=false) sorts first
	}
	if len(x.prefix) != len(y.prefix) {
		return len(x.prefix) > len(y.prefix)
	}
	if len(x.suffix) != len(y.suffix) {
		return len(x.suffix) > len(y.suffix)
	}
	return x.prefix < y.prefix
}

// --- config loading ---------------------------------------------------------

type packageJSON struct {
	Name    string          `json:"name"`
	Main    string          `json:"main"`
	Module  string          `json:"module"`
	Types   string          `json:"types"`
	Typings string          `json:"typings"`
	Exports json.RawMessage `json:"exports"`
}

func loadPackageJSON(root, p string) (pkgEntry, bool) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return pkgEntry{}, false
	}
	var pj packageJSON
	if err := json.Unmarshal(stripJSONC(raw), &pj); err != nil || pj.Name == "" {
		return pkgEntry{}, false
	}
	dir := relSlash(root, filepath.Dir(p))
	e := pkgEntry{name: pj.Name, dir: dir}
	// Entry candidates for the bare-package import, most-authoritative first.
	for _, entry := range []string{
		exportsMain(pj.Exports), pj.Module, pj.Main, pj.Types, pj.Typings,
	} {
		if entry != "" {
			e.entries = appendUniq(e.entries, cleanCandidate(joinRel(dir, entry)))
		}
	}
	// Conventional fallbacks so a package with no explicit entry still resolves;
	// src-layout first (dominant in these monorepos), then a root index.
	for _, fb := range []string{"src/index", "index", "src/main", "lib/index"} {
		e.entries = appendUniq(e.entries, cleanCandidate(dir+"/"+fb))
	}
	return e, true
}

// exportsMain best-effort extracts the main entry from a package.json "exports"
// field: a bare string, or the "." subpath, following condition objects down to
// the first string leaf (import/module/default/require order).
func exportsMain(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	dot, ok := m["."]
	if !ok {
		// No "." subpath: the object may itself be a conditions map.
		dot = raw
	}
	return firstStringLeaf(dot, 0)
}

func firstStringLeaf(raw json.RawMessage, depth int) string {
	if depth > 6 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, cond := range []string{"import", "module", "default", "require", "node"} {
		if v, ok := m[cond]; ok {
			if leaf := firstStringLeaf(v, depth+1); leaf != "" {
				return leaf
			}
		}
	}
	return ""
}

type tsConfig struct {
	Extends         string `json:"extends"`
	CompilerOptions struct {
		BaseURL string              `json:"baseUrl"`
		Paths   map[string][]string `json:"paths"`
	} `json:"compilerOptions"`
}

// loadTSConfigAliases reads one tsconfig/jsconfig, follows a relative "extends"
// chain (guarding cycles via seen), and returns alias rules with targets joined
// to baseUrl and made project-relative + extension-free.
func loadTSConfigAliases(root, p string, seen map[string]struct{}) []aliasRule {
	abs, _ := filepath.Abs(p)
	if _, dup := seen[abs]; dup {
		return nil
	}
	seen[abs] = struct{}{}

	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var tc tsConfig
	if err := json.Unmarshal(stripJSONC(raw), &tc); err != nil {
		return nil
	}
	dir := filepath.Dir(p)

	// Inherited aliases from a relative extends target; the child overrides on
	// pattern conflict (appended first, deduped by caller / Candidates order).
	var inherited []aliasRule
	if tc.Extends != "" && (strings.HasPrefix(tc.Extends, ".") || strings.HasPrefix(tc.Extends, "/")) {
		ext := tc.Extends
		if !strings.HasSuffix(ext, ".json") {
			ext += ".json"
		}
		inherited = loadTSConfigAliases(root, filepath.Join(dir, ext), seen)
	}

	// baseUrl defaults to the tsconfig's own directory (TS 4.1+ allows paths
	// without an explicit baseUrl, resolved relative to the config file).
	baseAbs := dir
	if tc.CompilerOptions.BaseURL != "" {
		baseAbs = filepath.Join(dir, tc.CompilerOptions.BaseURL)
	}
	baseRel := relSlash(root, baseAbs)

	var rules []aliasRule
	for pattern, targets := range tc.CompilerOptions.Paths {
		r := aliasRule{}
		if before, after, found := strings.Cut(pattern, "*"); found {
			r.star, r.prefix, r.suffix = true, before, after
		} else {
			r.prefix = pattern
		}
		for _, t := range targets {
			// Keep the target a raw template (its '*' intact); apply() does the
			// textual substitution and add() cleans only the concrete result, so
			// the star's surrounding separators survive.
			if tmpl := joinRel(baseRel, filepath.ToSlash(t)); tmpl != "" {
				r.targets = append(r.targets, tmpl)
			}
		}
		if len(r.targets) > 0 {
			rules = append(rules, r)
		}
	}
	// Child rules first so they win over inherited ones of equal specificity.
	return append(rules, inherited...)
}

// --- path helpers -----------------------------------------------------------

// cleanCandidate normalizes a project-relative path to forward slashes, strips a
// trailing "/index"-less known JS/TS extension, and drops a "./" prefix.
func cleanCandidate(p string) string {
	p = filepath.ToSlash(strings.TrimSpace(p))
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "." {
		return ""
	}
	// Preserve a trailing "*" (alias target template) across cleaning.
	star := strings.HasSuffix(p, "*")
	p = strings.TrimSuffix(p, "*")
	p = path.Clean(p)
	if p == "." {
		p = ""
	}
	for _, ext := range jsExts {
		if strings.HasSuffix(p, ext) {
			p = p[:len(p)-len(ext)]
			break
		}
	}
	if star {
		p += "*"
	}
	return p
}

// joinRel joins a base (project-relative) with a possibly-relative sub path.
func joinRel(base, sub string) string {
	sub = filepath.ToSlash(sub)
	if base == "" || base == "." {
		return sub
	}
	return base + "/" + sub
}

// relSlash returns target relative to root in forward-slash form; "" for root.
func relSlash(root, target string) string {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return filepath.ToSlash(target)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return ""
	}
	return rel
}

func appendUniq(s []string, v string) []string {
	if v == "" || slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}
