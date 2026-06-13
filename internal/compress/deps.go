package compress

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// DepsFileArg extracts the filename argument from `cat file.json`-style commands.
func DepsFileArg(cmd string) string {
	parts := strings.Fields(cmd)
	if len(parts) < 2 {
		return ""
	}
	return filepath.Base(parts[len(parts)-1])
}

// IsDepsFilename returns true for known dependency manifest file names.
func IsDepsFilename(base string) bool {
	switch base {
	case "package.json", "go.mod", "go.sum", "Cargo.toml", "Cargo.lock",
		"requirements.txt", "requirements-dev.txt", "pyproject.toml",
		"Pipfile", "Gemfile", "Gemfile.lock", "pom.xml", "build.gradle",
		"build.gradle.kts", "composer.json", "package-lock.json", "yarn.lock",
		"pnpm-lock.yaml", "bun.lockb":
		return true
	}
	return false
}

// CompressDepsFile returns a compact summary for known dependency manifests.
// Returns ("", false) when the file is not a recognised manifest.
func CompressDepsFile(path string, data []byte) (string, bool) {
	base := filepath.Base(path)
	switch base {
	case "package.json":
		return CompressPkgJSON(data)
	case "go.mod":
		return CompressGoMod(data)
	case "Cargo.toml":
		return CompressCargoToml(data)
	case "requirements.txt", "requirements-dev.txt":
		return CompressRequirementsTxt(base, data)
	case "pyproject.toml":
		return CompressPyprojectToml(data)
	case "Gemfile":
		return CompressGemfile(data)
	default:
		return "", false
	}
}

// ── package.json ─────────────────────────────────────────────────────────────

type pkgJSON struct {
	Name             string            `json:"name"`
	Version          string            `json:"version"`
	Dependencies     map[string]string `json:"dependencies"`
	DevDependencies  map[string]string `json:"devDependencies"`
	PeerDependencies map[string]string `json:"peerDependencies"`
	Scripts          map[string]string `json:"scripts"`
}

func CompressPkgJSON(data []byte) (string, bool) {
	var p pkgJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return "", false
	}
	var b strings.Builder
	label := p.Name
	if p.Version != "" {
		label += "@" + p.Version
	}
	fmt.Fprintf(&b, "Node (%s):\n", label)
	if len(p.Dependencies) > 0 {
		fmt.Fprintf(&b, "  deps (%d): %s\n", len(p.Dependencies), joinMapKeys(p.Dependencies, 12))
	}
	if len(p.DevDependencies) > 0 {
		fmt.Fprintf(&b, "  devDeps (%d): %s\n", len(p.DevDependencies), joinMapKeys(p.DevDependencies, 12))
	}
	if len(p.PeerDependencies) > 0 {
		fmt.Fprintf(&b, "  peerDeps (%d): %s\n", len(p.PeerDependencies), joinMapKeys(p.PeerDependencies, 8))
	}
	if len(p.Scripts) > 0 {
		fmt.Fprintf(&b, "  scripts (%d): %s\n", len(p.Scripts), joinMapKeys(p.Scripts, 8))
	}
	return strings.TrimRight(b.String(), "\n"), true
}

// ── go.mod ───────────────────────────────────────────────────────────────────

func CompressGoMod(data []byte) (string, bool) {
	lines := strings.Split(string(data), "\n")
	var module, goVer string
	var deps, indirect []string
	inRequire := false

	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "module ") {
			module = strings.TrimPrefix(t, "module ")
		} else if strings.HasPrefix(t, "go ") && goVer == "" {
			goVer = strings.TrimPrefix(t, "go ")
		} else if t == "require (" {
			inRequire = true
		} else if t == ")" {
			inRequire = false
		} else if inRequire && t != "" {
			// "github.com/foo/bar v1.2.3 // indirect"
			parts := strings.Fields(t)
			if len(parts) >= 2 {
				pkg := ShortPkg(parts[0])
				isIndirect := len(parts) >= 4 && parts[2] == "//" && parts[3] == "indirect"
				if isIndirect {
					indirect = append(indirect, pkg)
				} else {
					deps = append(deps, pkg)
				}
			}
		} else if strings.HasPrefix(t, "require ") {
			// single-line: require github.com/foo v1
			rest := strings.TrimPrefix(t, "require ")
			parts := strings.Fields(rest)
			if len(parts) >= 2 {
				deps = append(deps, ShortPkg(parts[0]))
			}
		}
	}

	if module == "" {
		return "", false
	}
	var b strings.Builder
	label := module
	if goVer != "" {
		label += " (go " + goVer + ")"
	}
	fmt.Fprintf(&b, "Go (%s):\n", label)
	if len(deps) > 0 {
		fmt.Fprintf(&b, "  deps (%d): %s\n", len(deps), joinSlice(deps, 12))
	}
	if len(indirect) > 0 {
		fmt.Fprintf(&b, "  indirect (%d): %s\n", len(indirect), joinSlice(indirect, 8))
	}
	return strings.TrimRight(b.String(), "\n"), true
}

// ShortPkg returns the last two path segments of a Go module path.
func ShortPkg(pkg string) string {
	parts := strings.Split(pkg, "/")
	if len(parts) <= 2 {
		return pkg
	}
	return strings.Join(parts[len(parts)-2:], "/")
}

// ── Cargo.toml ───────────────────────────────────────────────────────────────

func CompressCargoToml(data []byte) (string, bool) {
	lines := strings.Split(string(data), "\n")
	var name, version string
	var deps, devDeps []string
	section := ""

	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			section = strings.Trim(t, "[]")
			continue
		}
		if strings.Contains(t, "=") && !strings.HasPrefix(t, "#") {
			key, _, ok := strings.Cut(t, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			switch section {
			case "package":
				val := tomlStringVal(t)
				switch key {
				case "name":
					name = val
				case "version":
					version = val
				}
			case "dependencies":
				if key != "" {
					deps = append(deps, key)
				}
			case "dev-dependencies":
				if key != "" {
					devDeps = append(devDeps, key)
				}
			}
		}
	}

	if name == "" {
		return "", false
	}
	var b strings.Builder
	label := name
	if version != "" {
		label += "@" + version
	}
	fmt.Fprintf(&b, "Rust (%s):\n", label)
	if len(deps) > 0 {
		fmt.Fprintf(&b, "  deps (%d): %s\n", len(deps), joinSlice(deps, 12))
	}
	if len(devDeps) > 0 {
		fmt.Fprintf(&b, "  devDeps (%d): %s\n", len(devDeps), joinSlice(devDeps, 8))
	}
	return strings.TrimRight(b.String(), "\n"), true
}

// tomlStringVal extracts the string value from a TOML line like `key = "value"`.
func tomlStringVal(line string) string {
	_, after, ok := strings.Cut(line, "=")
	if !ok {
		return ""
	}
	v := strings.TrimSpace(after)
	v = strings.Trim(v, `"'`)
	return v
}

// ── requirements.txt ─────────────────────────────────────────────────────────

func CompressRequirementsTxt(base string, data []byte) (string, bool) {
	lines := strings.Split(string(data), "\n")
	var pkgs []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "-") {
			continue
		}
		// strip version specifier: fastapi>=0.100 → fastapi
		name := strings.FieldsFunc(t, func(r rune) bool {
			return r == '=' || r == '>' || r == '<' || r == '!' || r == '['
		})[0]
		pkgs = append(pkgs, name)
	}
	if len(pkgs) == 0 {
		return "", false
	}
	return fmt.Sprintf("Python (%s):\n  deps (%d): %s", base, len(pkgs), joinSlice(pkgs, 14)), true
}

// ── pyproject.toml ───────────────────────────────────────────────────────────

func CompressPyprojectToml(data []byte) (string, bool) {
	lines := strings.Split(string(data), "\n")
	var name, version string
	var deps, devDeps []string
	section := ""
	inDepsArray := false

	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			section = strings.Trim(t, "[]")
			inDepsArray = false
			continue
		}
		switch section {
		case "project":
			if strings.HasPrefix(t, "name") {
				name = tomlStringVal(t)
			} else if strings.HasPrefix(t, "version") {
				version = tomlStringVal(t)
			} else if strings.HasPrefix(t, "dependencies") {
				inDepsArray = true
			}
		case "tool.poetry.dependencies", "tool.poetry.dev-dependencies",
			"project.optional-dependencies":
			if strings.Contains(t, "=") && !strings.HasPrefix(t, "#") {
				key, _, ok := strings.Cut(t, "=")
				if ok {
					k := strings.TrimSpace(key)
					if k != "python" && k != "" {
						deps = append(deps, k)
					}
				}
			}
		}
		if inDepsArray {
			// "fastapi>=0.100", or ]
			if t == "]" {
				inDepsArray = false
				continue
			}
			dep := strings.Trim(t, `"', `)
			if dep == "" || dep == "[" {
				continue
			}
			pkg := strings.FieldsFunc(dep, func(r rune) bool {
				return r == '>' || r == '<' || r == '=' || r == '!' || r == '['
			})[0]
			deps = append(deps, strings.TrimSpace(pkg))
		}
	}

	if name == "" && len(deps) == 0 {
		return "", false
	}
	var b strings.Builder
	label := name
	if version != "" {
		label += "@" + version
	}
	fmt.Fprintf(&b, "Python (%s):\n", label)
	if len(deps) > 0 {
		fmt.Fprintf(&b, "  deps (%d): %s\n", len(deps), joinSlice(deps, 14))
	}
	if len(devDeps) > 0 {
		fmt.Fprintf(&b, "  devDeps (%d): %s\n", len(devDeps), joinSlice(devDeps, 8))
	}
	return strings.TrimRight(b.String(), "\n"), true
}

// ── Gemfile ──────────────────────────────────────────────────────────────────

func CompressGemfile(data []byte) (string, bool) {
	lines := strings.Split(string(data), "\n")
	var gems []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "gem ") {
			continue
		}
		// gem 'rails', '~> 7.0'  or  gem "devise"
		rest := strings.TrimPrefix(t, "gem ")
		parts := strings.SplitN(rest, ",", 2)
		name := strings.Trim(parts[0], `"' `)
		if name != "" {
			gems = append(gems, name)
		}
	}
	if len(gems) == 0 {
		return "", false
	}
	return fmt.Sprintf("Ruby (Gemfile):\n  gems (%d): %s", len(gems), joinSlice(gems, 12)), true
}

// ── helpers ───────────────────────────────────────────────────────────────────

// joinSlice joins up to max items with ", ..." suffix if truncated.
func joinSlice(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(", ...+%d", len(items)-max)
}

// joinMapKeys joins up to max keys from a map with deterministic output.
func joinMapKeys(m map[string]string, max int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// sort for determinism
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return joinSlice(keys, max)
}
