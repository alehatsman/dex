package refactor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeModule lays down a tiny self-contained Go module and returns its root.
func writeModule(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/rt\n\ngo 1.21\n")
	write("a.go", `package main

func Greet(name string) string { return "hi " + name }

type Server struct{}

func (s *Server) Run() string { return Greet("x") }

type Worker struct{}

func (w *Worker) Run() string { return "work" }
`)
	write("b.go", `package main

func use() string { return Greet("y") + (&Server{}).Run() }

func main() { _ = use() }
`)
	return dir
}

// assertEditsCoverOldName checks every edit's byte span in the current file
// content equals want — proving the offsets are precise.
func assertEditsCoverOldName(t *testing.T, root string, res RenameResult, want string) {
	t.Helper()
	cache := map[string][]byte{}
	for _, e := range res.Edits {
		b, ok := cache[e.Path]
		if !ok {
			var err error
			b, err = os.ReadFile(filepath.Join(root, e.Path))
			if err != nil {
				t.Fatal(err)
			}
			cache[e.Path] = b
		}
		if e.StartByte < 0 || e.EndByte > len(b) || e.StartByte >= e.EndByte {
			t.Errorf("edit %+v has out-of-range span (file %d bytes)", e, len(b))
			continue
		}
		if got := string(b[e.StartByte:e.EndByte]); got != want {
			t.Errorf("edit %s:%d span = %q, want %q", e.Path, e.Line, got, want)
		}
	}
}

func TestPlanRename_FuncAcrossFiles(t *testing.T) {
	root := writeModule(t)
	res, err := PlanRename(context.Background(), root, "Greet", "Welcome", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", res.Status, res.Hint)
	}
	// def in a.go + use in a.go (Run) + use in b.go = 3 edits across 2 files.
	if res.Files != 2 {
		t.Errorf("files = %d, want 2", res.Files)
	}
	if len(res.Edits) != 3 {
		t.Errorf("edits = %d, want 3: %+v", len(res.Edits), res.Edits)
	}
	assertEditsCoverOldName(t, root, res, "Greet")
	if res.Etag == "" {
		t.Error("expected an etag")
	}
}

func TestPlanRename_MethodIsTypeResolved(t *testing.T) {
	root := writeModule(t)
	res, err := PlanRename(context.Background(), root, "(*Server).Run", "Start", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", res.Status, res.Hint)
	}
	// Only Server.Run (def in a.go + use in b.go) — never Worker.Run.
	if len(res.Edits) != 2 {
		t.Fatalf("edits = %d, want 2: %+v", len(res.Edits), res.Edits)
	}
	assertEditsCoverOldName(t, root, res, "Run")
	// Worker.Run lives on a.go line 13; ensure no edit lands there.
	a, _ := os.ReadFile(filepath.Join(root, "a.go"))
	workerRunOff := strings.Index(string(a), "func (w *Worker) Run")
	for _, e := range res.Edits {
		if e.Path == "a.go" && e.StartByte > workerRunOff {
			t.Errorf("edit at byte %d touches Worker.Run region (offset %d) — not type-resolved", e.StartByte, workerRunOff)
		}
	}
}

func TestPlanRename_UnsupportedLanguage(t *testing.T) {
	dir := t.TempDir() // no go.mod
	res, _ := PlanRename(context.Background(), dir, "Foo", "Bar", "")
	if res.Status != "unsupported-language" {
		t.Errorf("status = %q, want unsupported-language", res.Status)
	}
}

func TestPlanRename_NotFoundAndInvalid(t *testing.T) {
	root := writeModule(t)
	if res, _ := PlanRename(context.Background(), root, "NoSuchSymbol", "X", ""); res.Status != "not-found" {
		t.Errorf("not-found: status = %q", res.Status)
	}
	if res, _ := PlanRename(context.Background(), root, "Greet", "123bad", ""); res.Status != "error" {
		t.Errorf("invalid ident: status = %q, want error", res.Status)
	}
	if res, _ := PlanRename(context.Background(), root, "Greet", "func", ""); res.Status != "error" {
		t.Errorf("keyword target: status = %q, want error", res.Status)
	}
}

func TestPlanRename_StaleEtag(t *testing.T) {
	root := writeModule(t)
	res, _ := PlanRename(context.Background(), root, "Greet", "Welcome", "deadbeefdeadbeef")
	if res.Status != "stale" {
		t.Fatalf("status = %q, want stale", res.Status)
	}
	// The fresh etag should be accepted on a second pass.
	res2, _ := PlanRename(context.Background(), root, "Greet", "Welcome", res.Etag)
	if res2.Status != "ok" {
		t.Errorf("re-plan with fresh etag: status = %q, want ok", res2.Status)
	}
}

func TestPlanRename_ConflictWarning(t *testing.T) {
	root := writeModule(t)
	// "use" already exists at package scope; renaming Greet→use should warn.
	res, _ := PlanRename(context.Background(), root, "Greet", "use", "")
	if res.Status != "ok" {
		t.Fatalf("status = %q, want ok", res.Status)
	}
	if len(res.Warnings) == 0 {
		t.Error("expected a conflict warning when renaming to an existing scope name")
	}
}
