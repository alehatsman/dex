package rehearse_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alehatsman/dex/internal/rehearse"
)

func TestRehearse_NoGoMod(t *testing.T) {
	dir := t.TempDir()
	res, err := rehearse.Rehearse(context.Background(), dir, rehearse.Input{
		Files: []rehearse.WholeFile{{Path: "foo.go", Contents: "package foo\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "unsupported-language" {
		t.Errorf("expected unsupported-language, got %q", res.Status)
	}
}

func TestRehearse_NoEdits(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\ngo 1.21\n"), 0o644)
	res, err := rehearse.Rehearse(context.Background(), dir, rehearse.Input{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "no-edits" {
		t.Errorf("expected no-edits, got %q", res.Status)
	}
}

// TestRehearse_DiskUntouched confirms that after a rehearsal the working tree
// is byte-identical to before — the overlay is strictly in-memory.
func TestRehearse_DiskUntouched(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\ngo 1.21\n"), 0o644)
	const originalSrc = "package x\n\nfunc Hello() string { return \"hello\" }\n"
	fooPath := filepath.Join(dir, "foo.go")
	_ = os.WriteFile(fooPath, []byte(originalSrc), 0o644)

	// Rehearse a whole-file replacement that breaks compilation.
	_, err := rehearse.Rehearse(context.Background(), dir, rehearse.Input{
		Files: []rehearse.WholeFile{
			{Path: "foo.go", Contents: "package x\n\nfunc Hello() string { return 42 }\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(fooPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != originalSrc {
		t.Errorf("disk file was modified: got\n%s\nwant\n%s", got, originalSrc)
	}
}

// TestRehearse_CompilationError confirms a type-breaking hypothetical is detected.
func TestRehearse_CompilationError(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\ngo 1.21\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package x\n\nfunc Hello() string { return \"hello\" }\n"), 0o644)

	res, err := rehearse.Rehearse(context.Background(), dir, rehearse.Input{
		Files: []rehearse.WholeFile{
			// Return an int where a string is expected — introduces a type error.
			{Path: "foo.go", Contents: "package x\n\nfunc Hello() string { return 42 }\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("expected status ok, got %q: %s", res.Status, res.Hint)
	}
	if res.Compiles {
		t.Error("expected Compiles=false for a broken hypothetical")
	}
	if len(res.Diagnostics) == 0 {
		t.Error("expected at least one diagnostic")
	}
	if len(res.BrokenFiles) == 0 {
		t.Error("expected at least one broken file")
	}
}

// TestRehearse_AbsolutePath confirms that a fully-absolute path under the
// project root is resolved correctly (#792).
func TestRehearse_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\ngo 1.21\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package x\n\nfunc Hello() string { return \"hello\" }\n"), 0o644)

	// Pass the fully-absolute path — the overlay must apply and the type error
	// must be detected (not silently ignored).
	abs := filepath.Join(dir, "foo.go")
	res, err := rehearse.Rehearse(context.Background(), dir, rehearse.Input{
		Files: []rehearse.WholeFile{
			{Path: abs, Contents: "package x\n\nfunc Hello() string { return 42 }\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("expected status ok, got %q: %s", res.Status, res.Hint)
	}
	if res.Compiles {
		t.Error("expected Compiles=false — overlay with absolute path must apply")
	}
}

// TestRehearse_CleanHypothetical confirms a valid replacement compiles.
func TestRehearse_CleanHypothetical(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\ngo 1.21\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package x\n\nfunc Hello() string { return \"hello\" }\n"), 0o644)

	res, err := rehearse.Rehearse(context.Background(), dir, rehearse.Input{
		Files: []rehearse.WholeFile{
			{Path: "foo.go", Contents: "package x\n\nfunc Greeting() string { return \"hi\" }\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("expected status ok, got %q: %s", res.Status, res.Hint)
	}
	if !res.Compiles {
		t.Errorf("expected Compiles=true for a valid hypothetical, diagnostics: %v", res.Diagnostics)
	}
}
