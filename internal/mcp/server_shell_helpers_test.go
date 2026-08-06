package mcp

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
)

// These cover the pure helpers extracted from shellRun (#122): now that the
// saved-pct math, cwd containment, and exit-code mapping are standalone, they
// are unit-testable without spawning a process or building a Server.

func TestCompressedShellOutput(t *testing.T) {
	tests := []struct {
		name      string
		orig, out int
		wantSaved int
	}{
		{"half dropped", 100, 50, 50},
		{"nothing dropped", 40, 40, 0},
		{"all dropped", 10, 0, 100},
		{"zero orig avoids div-by-zero", 0, 0, 0},
		{"grew (negative saved)", 10, 12, -20},
		{"truncated to int", 3, 1, 66},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compressedShellOutput("body", 7, tt.orig, tt.out)
			if got.SavedPct != tt.wantSaved {
				t.Errorf("SavedPct = %d, want %d", got.SavedPct, tt.wantSaved)
			}
			if got.Output != "body" || got.ExitCode != 7 {
				t.Errorf("payload not passed through: %+v", got)
			}
			if got.OriginalLines != tt.orig || got.OutputLines != tt.out {
				t.Errorf("line counts = (%d,%d), want (%d,%d)", got.OriginalLines, got.OutputLines, tt.orig, tt.out)
			}
		})
	}
}

func TestResolveShellCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg")

	t.Run("defaults to project root", func(t *testing.T) {
		got, err := resolveShellCwd(ShellInput{ProjectRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if got != root {
			t.Errorf("cwd = %q, want %q", got, root)
		}
	})

	t.Run("contained subdir is allowed and absolutized", func(t *testing.T) {
		got, err := resolveShellCwd(ShellInput{ProjectRoot: root, Cwd: sub})
		if err != nil {
			t.Fatal(err)
		}
		if got != sub {
			t.Errorf("cwd = %q, want %q", got, sub)
		}
	})

	t.Run("escape outside root is rejected", func(t *testing.T) {
		_, err := resolveShellCwd(ShellInput{ProjectRoot: sub, Cwd: root})
		if err == nil {
			t.Fatal("expected containment error, got nil")
		}
	})

	t.Run("no project root leaves cwd unchecked", func(t *testing.T) {
		got, err := resolveShellCwd(ShellInput{Cwd: "/anywhere"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "/anywhere" {
			t.Errorf("cwd = %q, want /anywhere", got)
		}
	})
}

func TestShellExitCode(t *testing.T) {
	t.Run("nil error is zero", func(t *testing.T) {
		if code := shellExitCode(context.Background(), nil); code != 0 {
			t.Errorf("code = %d, want 0", code)
		}
	})

	t.Run("cancelled context maps to 124", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Even with a real ExitError, a cancelled ctx must win.
		if code := shellExitCode(ctx, errors.New("killed")); code != 124 {
			t.Errorf("code = %d, want 124", code)
		}
	})

	t.Run("exit error yields its code", func(t *testing.T) {
		runErr := exec.Command("sh", "-c", "exit 3").Run()
		if code := shellExitCode(context.Background(), runErr); code != 3 {
			t.Errorf("code = %d, want 3", code)
		}
	})

	t.Run("non-exit error is generic 1", func(t *testing.T) {
		if code := shellExitCode(context.Background(), errors.New("boom")); code != 1 {
			t.Errorf("code = %d, want 1", code)
		}
	})
}
