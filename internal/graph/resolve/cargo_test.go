package resolve

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCargoWorkspace lays out a minimal Cargo workspace under dir: a root
// Cargo.toml with the given members block and one crate dir per (dir, name).
func writeCargoWorkspace(t *testing.T, dir, rootToml string, crates map[string]string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(rootToml), 0o644); err != nil {
		t.Fatal(err)
	}
	for cdir, name := range crates {
		full := filepath.Join(dir, filepath.FromSlash(cdir))
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "[package]\n"
		if name != "" {
			body += "name = \"" + name + "\"\n"
		} else {
			body += "name.workspace = true\n"
		}
		if err := os.WriteFile(filepath.Join(full, "Cargo.toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIsCargoWorkspaceRoot(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want bool
	}{
		{"workspace table", "[workspace]\nmembers = [\"a\"]\n", true},
		{"workspace subtable header only", "[workspace.package]\nversion = \"1\"\n[workspace]\n", true},
		{"package only (single crate)", "[package]\nname = \"x\"\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(tc.toml), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := IsCargoWorkspaceRoot(dir); got != tc.want {
				t.Errorf("IsCargoWorkspaceRoot = %v, want %v", got, tc.want)
			}
		})
	}
	if IsCargoWorkspaceRoot("") {
		t.Error(`IsCargoWorkspaceRoot("") = true, want false`)
	}
	// A Cargo.toml with no [workspace] must not be missed for a non-Cargo repo.
	if IsCargoWorkspaceRoot(t.TempDir()) {
		t.Error("IsCargoWorkspaceRoot on dir without Cargo.toml = true, want false")
	}
}

func TestLoadCargoExplicitMembers(t *testing.T) {
	dir := t.TempDir()
	root := "[workspace]\nmembers = [\n  \"crates/core-lib\",\n  \"crates/app\",\n]\n"
	writeCargoWorkspace(t, dir, root, map[string]string{
		"crates/core-lib": "core-lib",
		"crates/app":      "app",
	})
	w := LoadCargo(dir)
	if w == nil {
		t.Fatal("LoadCargo = nil, want workspace")
	}

	// CrateForFile: hyphen → underscore, longest member dir wins.
	for _, tc := range []struct {
		path, crate, member string
		ok                  bool
	}{
		{"crates/core-lib/src/lib.rs", "core_lib", "crates/core-lib", true},
		{"crates/app/src/main.rs", "app", "crates/app", true},
		{"README.md", "", "", false},
		{"crates/other/src/lib.rs", "", "", false},
	} {
		crate, member, ok := w.CrateForFile(tc.path)
		if ok != tc.ok || crate != tc.crate || member != tc.member {
			t.Errorf("CrateForFile(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.path, crate, member, ok, tc.crate, tc.member, tc.ok)
		}
	}

	// ProjectOf: first `::` segment when it names a known crate.
	for _, tc := range []struct{ pkg, want string }{
		{"core_lib", "core_lib"},
		{"core_lib::util", "core_lib"},
		{"app", "app"},
		{"serde::de", ""}, // external crate — not a workspace member
		{"", ""},
	} {
		if got := w.ProjectOf(tc.pkg); got != tc.want {
			t.Errorf("ProjectOf(%q) = %q, want %q", tc.pkg, got, tc.want)
		}
	}
}

func TestLoadCargoGlobAndInheritedName(t *testing.T) {
	dir := t.TempDir()
	root := "[workspace]\nmembers = [\"crates/*\"]\n"
	writeCargoWorkspace(t, dir, root, map[string]string{
		"crates/alpha": "",        // name.workspace = true → basename fallback
		"crates/beta":  "beta-rs", // explicit name, hyphen → underscore
	})
	// A non-crate dir under the glob (no Cargo.toml) must be ignored.
	if err := os.MkdirAll(filepath.Join(dir, "crates", "notacrate"), 0o755); err != nil {
		t.Fatal(err)
	}

	w := LoadCargo(dir)
	if w == nil {
		t.Fatal("LoadCargo = nil, want workspace")
	}
	if _, _, ok := w.CrateForFile("crates/alpha/src/lib.rs"); !ok {
		t.Error("glob member crates/alpha not resolved")
	}
	if got := w.ProjectOf("alpha::mod"); got != "alpha" {
		t.Errorf("inherited-name crate: ProjectOf = %q, want alpha (dir basename)", got)
	}
	if got := w.ProjectOf("beta_rs"); got != "beta_rs" {
		t.Errorf("explicit-name crate: ProjectOf = %q, want beta_rs", got)
	}
	if _, _, ok := w.CrateForFile("crates/notacrate/x.rs"); ok {
		t.Error("dir without Cargo.toml treated as crate")
	}
}

func TestProjectOfForRoot(t *testing.T) {
	// Cargo workspace → cargo mapper.
	cargo := t.TempDir()
	writeCargoWorkspace(t, cargo, "[workspace]\nmembers = [\"crates/app\"]\n",
		map[string]string{"crates/app": "app"})
	if m, ok := ProjectOfForRoot(cargo); !ok || m == nil || m("app::x") != "app" {
		t.Errorf("ProjectOfForRoot(cargo) did not yield a cargo mapper")
	}

	// Non-workspace (single crate) → nil, false.
	single := t.TempDir()
	if err := os.WriteFile(filepath.Join(single, "Cargo.toml"), []byte("[package]\nname = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, ok := ProjectOfForRoot(single); ok || m != nil {
		t.Errorf("ProjectOfForRoot(single-crate) = (_, %v), want (nil, false)", ok)
	}

	// JS/TS workspace still routes to the resolve mapper.
	js := t.TempDir()
	if err := os.WriteFile(filepath.Join(js, "package.json"), []byte(`{"workspaces":["packages/*"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ProjectOfForRoot(js); !ok {
		t.Error("ProjectOfForRoot(js workspace) = (_, false), want true")
	}
}
