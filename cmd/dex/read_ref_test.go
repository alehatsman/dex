package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitInitCommit(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", file},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", msg},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = hermeticGitReadEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestGitShowFileTimeTravel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init", "-q")
	cmd.Env = hermeticGitReadEnv()
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	gitInitCommit(t, dir, "f.go", "package p\nfunc V1() {}\n", "v1")
	gitInitCommit(t, dir, "f.go", "package p\nfunc V2() {}\n", "v2")

	ctx := context.Background()
	path := filepath.Join(dir, "f.go")

	// HEAD~1 is v1, HEAD is v2.
	old, err := gitShowFile(ctx, path, "HEAD~1")
	if err != nil {
		t.Fatalf("gitShowFile HEAD~1: %v", err)
	}
	if !strings.Contains(string(old), "V1") || strings.Contains(string(old), "V2") {
		t.Errorf("HEAD~1 content = %q, want the V1 version", old)
	}
	cur, err := gitShowFile(ctx, path, "HEAD")
	if err != nil {
		t.Fatalf("gitShowFile HEAD: %v", err)
	}
	if !strings.Contains(string(cur), "V2") {
		t.Errorf("HEAD content = %q, want the V2 version", cur)
	}
}

func TestGitShowFileRejectsOptionRef(t *testing.T) {
	_, err := gitShowFile(context.Background(), "f.go", "-x")
	if err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Fatalf("option-like ref must be rejected, got err=%v", err)
	}
}

func TestGitShowFileBadRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init", "-q")
	cmd.Env = hermeticGitReadEnv()
	_ = cmd.Run()
	gitInitCommit(t, dir, "f.go", "package p\n", "v1")
	if _, err := gitShowFile(context.Background(), filepath.Join(dir, "f.go"), "nonexistent-ref"); err == nil {
		t.Fatal("expected an error for a nonexistent ref")
	}
}
