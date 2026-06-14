package mcp

import (
	"os/exec"
	"strings"
	"testing"
)

func TestResolveShellInterpreter(t *testing.T) {
	t.Run("DEX_SHELL overrides", func(t *testing.T) {
		t.Setenv("DEX_SHELL", "/custom/myshell")
		if got := resolveShellInterpreter(); got != "/custom/myshell" {
			t.Errorf("got %q, want /custom/myshell", got)
		}
	})
	t.Run("prefers bash, falls back to sh", func(t *testing.T) {
		t.Setenv("DEX_SHELL", "")
		got := resolveShellInterpreter()
		if path, err := exec.LookPath("bash"); err == nil {
			if got != path {
				t.Errorf("with bash present, got %q, want %q", got, path)
			}
		} else if got != "sh" {
			t.Errorf("without bash, got %q, want sh", got)
		}
	})
}

// TestShellRun_UsesBash verifies the shell tool runs via bash so bash-only
// syntax works (#542) — `[[ ]]` is a bash builtin that POSIX sh/dash reject.
func TestShellRun_UsesBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	s := &Server{}
	out, err := s.ShellRun(t.Context(), ShellInput{Command: `[[ 1 == 1 ]] && echo bashok`, Raw: true})
	if err != nil {
		t.Fatalf("bash syntax errored: %v", err)
	}
	if !strings.Contains(out.Output, "bashok") {
		t.Fatalf("bash-only syntax did not run; output: %q", out.Output)
	}
	// `set -o pipefail` is a bash feature dash rejects — confirm it is accepted.
	out2, err := s.ShellRun(t.Context(), ShellInput{Command: `set -o pipefail; echo pfok`, Raw: true})
	if err != nil {
		t.Fatalf("pipefail errored: %v", err)
	}
	if !strings.Contains(out2.Output, "pfok") {
		t.Fatalf("pipefail not supported by interpreter; output: %q", out2.Output)
	}
}
