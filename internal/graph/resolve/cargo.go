package resolve

// Cargo workspace model (#162). The JS/TS half of this package resolves module
// specifiers; this half answers the two Cargo facts the Rust graph needs — which
// crate owns a file, and which crate a `crate::mod` package path belongs to — so
// package_topology can roll a Rust module DAG up to crates the same way it rolls
// a JS module DAG up to workspace projects. Dependency-free, line-oriented
// Cargo.toml reading in the spirit of this package's jsonc.go: we need only
// [workspace].members and each member's [package].name, not a full TOML parser.
//
// See specs/cargo-topology.md.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CargoWorkspace maps workspace member dirs to crate identifiers. Build one with
// LoadCargo; read-only afterwards. A nil *CargoWorkspace answers every query
// negatively, so callers need not nil-check.
type CargoWorkspace struct {
	// members is sorted by dir length descending so the longest (most specific)
	// member dir wins a prefix match, mirroring Workspace.packages.
	members []cargoMember
	// crates is the set of crate identifiers (hyphen→underscore) for ProjectOf.
	crates map[string]struct{}
}

// cargoMember is one resolved workspace member.
type cargoMember struct {
	dir   string // project-relative, slash form ("crates/foo-core")
	crate string // Rust crate identifier ("foo_core")
}

// IsCargoWorkspaceRoot reports whether root/Cargo.toml declares a [workspace]
// table. Like IsWorkspaceRoot this is a ROOT-only check: it is what separates a
// genuine Cargo workspace from a lone crate (or a Go repo with a buried
// Cargo.toml fixture), so the crate rollup gates on it rather than on LoadCargo
// happening to find members.
func IsCargoWorkspaceRoot(root string) bool {
	if root == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(root, "Cargo.toml"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if tomlSectionHeader(line) == "workspace" {
			return true
		}
	}
	return false
}

// LoadCargo parses root Cargo.toml [workspace].members (explicit paths and
// trailing "/*" globs) and each member's [package].name into a CargoWorkspace.
// Returns nil when root has no readable [workspace] Cargo.toml, so a non-Cargo
// repo yields the no-op workspace.
func LoadCargo(root string) *CargoWorkspace {
	data, err := os.ReadFile(filepath.Join(root, "Cargo.toml"))
	if err != nil {
		return nil
	}
	memberGlobs := cargoMembers(string(data))
	if len(memberGlobs) == 0 {
		return nil
	}
	w := &CargoWorkspace{crates: map[string]struct{}{}}
	for _, dir := range expandMemberDirs(root, memberGlobs) {
		name := cargoPackageName(root, dir)
		if name == "" {
			name = dirBase(dir) // workspace-inherited name → dir basename
		}
		crate := crateIdent(name)
		if crate == "" {
			continue
		}
		w.members = append(w.members, cargoMember{dir: dir, crate: crate})
		w.crates[crate] = struct{}{}
	}
	if len(w.members) == 0 {
		return nil
	}
	sort.Slice(w.members, func(i, j int) bool {
		return len(w.members[i].dir) > len(w.members[j].dir)
	})
	return w
}

// CrateForFile maps a project-root-relative .rs path (slash form) to the crate
// that owns it and that crate's member dir. ok is false for a file under no
// workspace member (the caller then falls back to single-crate handling).
func (w *CargoWorkspace) CrateForFile(relPath string) (crate, memberDir string, ok bool) {
	if w == nil {
		return "", "", false
	}
	p := filepath.ToSlash(relPath)
	for _, m := range w.members { // longest dir first
		if p == m.dir || strings.HasPrefix(p, m.dir+"/") {
			return m.crate, m.dir, true
		}
	}
	return "", "", false
}

// ProjectOf maps a Rust package path ("crate::mod::sub") to its crate — the
// first "::" segment when it names a known workspace crate, else "". This is the
// mapper BuildProjectGraph applies to package paths for the crate rollup.
func (w *CargoWorkspace) ProjectOf(pkgPath string) string {
	if w == nil || pkgPath == "" {
		return ""
	}
	head := pkgPath
	if i := strings.Index(pkgPath, "::"); i >= 0 {
		head = pkgPath[:i]
	}
	if _, ok := w.crates[head]; ok {
		return head
	}
	return ""
}

// ProjectOfForRoot returns root's package→project mapper and whether root is any
// recognized workspace root (JS/TS or Cargo). The mapper takes a package path
// and returns its owning workspace project, "" when none. nil,false for a
// non-workspace repo. Both crate-rollup call sites use this so neither has to
// branch on workspace kind.
func ProjectOfForRoot(root string) (func(string) string, bool) {
	if IsWorkspaceRoot(root) {
		return Load(root).ProjectOf, true
	}
	if IsCargoWorkspaceRoot(root) {
		if w := LoadCargo(root); w != nil {
			return w.ProjectOf, true
		}
	}
	return nil, false
}

// ---- Cargo.toml scanning (minimal, line-oriented) --------------------------

// tomlSectionHeader returns the table name of a line that is a bare section
// header ("[workspace]" → "workspace", "[workspace.package]" → "workspace"), or
// "" for any other line. Only the top-level table name is returned, so callers
// can match a table and its sub-tables uniformly.
func tomlSectionHeader(line string) string {
	s := strings.TrimSpace(line)
	if len(s) < 2 || s[0] != '[' || strings.HasPrefix(s, "[[") {
		return ""
	}
	end := strings.IndexByte(s, ']')
	if end < 0 {
		return ""
	}
	name := s[1:end]
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}

// cargoMembers extracts the [workspace].members string list. members may span
// multiple lines; we capture every double-quoted string from the "members = ["
// opener through its closing "]".
func cargoMembers(toml string) []string {
	var out []string
	section, inArray := "", false
	for _, line := range strings.Split(toml, "\n") {
		if h := tomlSectionHeader(line); h != "" {
			section, inArray = h, false
			continue
		}
		trimmed := strings.TrimSpace(line)
		if !inArray {
			if section != "workspace" {
				continue
			}
			key, rest, ok := splitTOMLKey(trimmed)
			if !ok || key != "members" {
				continue
			}
			inArray = true
			out = append(out, quotedStrings(rest)...)
		} else {
			out = append(out, quotedStrings(trimmed)...)
		}
		if inArray && strings.Contains(trimmed, "]") {
			inArray = false
		}
	}
	return out
}

// cargoPackageName reads <root>/<dir>/Cargo.toml and returns its [package].name,
// or "" when absent / workspace-inherited (name.workspace = true).
func cargoPackageName(root, dir string) string {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), "Cargo.toml"))
	if err != nil {
		return ""
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		if h := tomlSectionHeader(line); h != "" {
			section = h
			continue
		}
		if section != "package" {
			continue
		}
		key, rest, ok := splitTOMLKey(strings.TrimSpace(line))
		if !ok || key != "name" {
			continue
		}
		if qs := quotedStrings(rest); len(qs) > 0 {
			return qs[0]
		}
	}
	return ""
}

// expandMemberDirs resolves member glob patterns to concrete project-relative
// dirs. Only a trailing "/*" glob is supported (expanded to immediate subdirs
// that contain a Cargo.toml); every other entry is taken verbatim.
func expandMemberDirs(root string, patterns []string) []string {
	var out []string
	for _, pat := range patterns {
		pat = strings.Trim(filepath.ToSlash(pat), "/")
		if pat == "" {
			continue
		}
		if strings.HasSuffix(pat, "/*") {
			base := strings.TrimSuffix(pat, "/*")
			ents, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(base)))
			if err != nil {
				continue
			}
			for _, ent := range ents {
				if !ent.IsDir() {
					continue
				}
				dir := base + "/" + ent.Name()
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir), "Cargo.toml")); err == nil {
					out = append(out, dir)
				}
			}
			continue
		}
		out = append(out, pat)
	}
	return out
}

// dirBase returns the last slash-separated segment of a project-relative dir.
func dirBase(dir string) string {
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		return dir[i+1:]
	}
	return dir
}

// crateIdent turns a Cargo package name into its Rust crate identifier: Cargo
// normalizes '-' to '_' in the name a `use` statement sees.
func crateIdent(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), "-", "_")
}

// splitTOMLKey splits a "key = value" line into (key, value, ok), ignoring
// dotted keys' tail (so "name.workspace = true" yields key "name"). Returns
// ok=false for a line with no '='.
func splitTOMLKey(line string) (key, value string, ok bool) {
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:eq])
	if i := strings.IndexByte(key, '.'); i >= 0 {
		key = key[:i]
	}
	return key, line[eq+1:], true
}

// quotedStrings returns every double-quoted substring in s, in order. Good
// enough for Cargo.toml member paths and package names, which never embed an
// escaped quote in practice.
func quotedStrings(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '"')
		if i < 0 {
			return out
		}
		s = s[i+1:]
		j := strings.IndexByte(s, '"')
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+1:]
	}
}
