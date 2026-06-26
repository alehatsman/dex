package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitCommit(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", file},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", msg},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestReadAtRefTimeTravel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	gitCommit(t, dir, "f.go", "package p\nfunc V1() {}\n", "v1")
	gitCommit(t, dir, "f.go", "package p\nfunc V2() {}\n", "v2")

	ctx := context.Background()
	path := filepath.Join(dir, "f.go")

	old, err := ReadAtRef(ctx, path, "HEAD~1")
	if err != nil {
		t.Fatalf("ReadAtRef HEAD~1: %v", err)
	}
	if !strings.Contains(string(old), "V1") || strings.Contains(string(old), "V2") {
		t.Errorf("HEAD~1 = %q, want the V1 version", old)
	}
	cur, err := ReadAtRef(ctx, path, "HEAD")
	if err != nil {
		t.Fatalf("ReadAtRef HEAD: %v", err)
	}
	if !strings.Contains(string(cur), "V2") {
		t.Errorf("HEAD = %q, want the V2 version", cur)
	}
}

func TestReadAtRefRejectsOptionRef(t *testing.T) {
	_, err := ReadAtRef(context.Background(), "f.go", "-x")
	if err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Fatalf("option-like ref must be rejected, got err=%v", err)
	}
}

func TestReadAtRefBadRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	_ = exec.Command("git", "-C", dir, "init", "-q").Run()
	gitCommit(t, dir, "f.go", "package p\n", "v1")
	if _, err := ReadAtRef(context.Background(), filepath.Join(dir, "f.go"), "nonexistent-ref"); err == nil {
		t.Fatal("expected an error for a nonexistent ref")
	}
}
