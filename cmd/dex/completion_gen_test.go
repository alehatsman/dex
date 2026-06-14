package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGeneratedCompletionsCoverRegistry asserts every advertised verb — and its
// flags, choices, and subcommands — appears in all three generated scripts.
// This is the drift guard: add a flag to the registry and forget to wire it,
// and the script still carries it; this test fails if generation ever stops
// covering the registry.
func TestGeneratedCompletionsCoverRegistry(t *testing.T) {
	scripts := map[string]string{
		"bash": bashCompletionScript(),
		"zsh":  zshCompletionScript(),
		"fish": fishCompletionScript(),
	}
	for shell, script := range scripts {
		for _, v := range verbs {
			if v.group == groupHidden {
				if strings.Contains(script, v.name) {
					t.Errorf("%s: hidden verb %q leaked into completion", shell, v.name)
				}
				continue
			}
			if !strings.Contains(script, v.name) {
				t.Errorf("%s: verb %q missing from completion", shell, v.name)
			}
			for _, f := range v.flags {
				if !strings.Contains(script, flagToken(shell, f.name)) {
					t.Errorf("%s: verb %q flag %q missing from completion", shell, v.name, f.name)
				}
				for _, c := range f.choices {
					if !strings.Contains(script, c) {
						t.Errorf("%s: verb %q flag %q choice %q missing", shell, v.name, f.name, c)
					}
				}
			}
			for _, s := range v.subs {
				if !strings.Contains(script, s.name) {
					t.Errorf("%s: verb %q subcommand %q missing", shell, v.name, s.name)
				}
			}
		}
	}
}

// flagToken returns how a flag name is expected to appear in a given shell's
// generated script. fish rewrites flags into its own form (`--intent` → `-l
// intent`, `-v` → `-s v`); bash and zsh carry the flag verbatim.
func flagToken(shell, name string) string {
	if shell != "fish" {
		return name
	}
	if strings.HasPrefix(name, "--") {
		return "-l " + strings.TrimPrefix(name, "--")
	}
	return "-s " + strings.TrimPrefix(name, "-")
}

// TestGeneratedCompletionsSyntax runs `bash -n` / `zsh -n` over the generated
// scripts where those shells exist, catching malformed generated output.
func TestGeneratedCompletionsSyntax(t *testing.T) {
	cases := []struct {
		shell  string
		script string
	}{
		{"bash", bashCompletionScript()},
		{"zsh", zshCompletionScript()},
	}
	for _, tc := range cases {
		path, err := exec.LookPath(tc.shell)
		if err != nil {
			t.Logf("%s not installed — skipping syntax check", tc.shell)
			continue
		}
		f := filepath.Join(t.TempDir(), "dex-completion-"+tc.shell)
		if err := os.WriteFile(f, []byte(tc.script), 0o644); err != nil {
			t.Fatalf("write temp script: %v", err)
		}
		out, err := exec.Command(path, "-n", f).CombinedOutput()
		if err != nil {
			t.Errorf("%s -n rejected generated script: %v\n%s", tc.shell, err, out)
		}
	}
}
