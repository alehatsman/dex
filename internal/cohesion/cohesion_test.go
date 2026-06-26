package cohesion

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeIfaceModule(t *testing.T) string {
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
	write("go.mod", "module example.com/co\n\ngo 1.21\n")
	write("a.go", `package main

type Greeter interface {
	Hello() string
	Bye() string
}

type Full struct{}

func (Full) Hello() string { return "hi" }
func (Full) Bye() string   { return "bye" }

type PtrFull struct{}

func (p *PtrFull) Hello() string { return "h" }
func (p *PtrFull) Bye() string   { return "b" }

type Partial struct{}

func (Partial) Hello() string { return "hi" } // missing Bye

type Mismatch struct{}

func (Mismatch) Hello() int  { return 0 } // wrong signature
func (Mismatch) Bye() string { return "" }

type Unrelated struct{}

func (Unrelated) Other() {}

func main() {}
`)
	return dir
}

func memberByType(res CohortResult, typ string) *Member {
	for i := range res.Members {
		if res.Members[i].Type == typ {
			return &res.Members[i]
		}
	}
	return nil
}

func TestImplementorsOf(t *testing.T) {
	root := writeIfaceModule(t)
	res, err := ImplementorsOf(context.Background(), root, "Greeter")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", res.Status, res.Hint)
	}

	// Full + PtrFull complete; Partial near-miss; Mismatch/Unrelated excluded.
	if m := memberByType(res, "*main.Full"); m == nil || m.Status != "complete" {
		t.Errorf("Full: %+v, want complete", m)
	}
	if m := memberByType(res, "*main.PtrFull"); m == nil || m.Status != "complete" {
		t.Errorf("PtrFull: %+v, want complete (pointer receivers)", m)
	}
	if m := memberByType(res, "*main.Partial"); m == nil || m.Status != "partial" {
		t.Errorf("Partial: %+v, want partial", m)
	} else if len(m.Missing) != 1 || m.Missing[0] != "Bye" {
		t.Errorf("Partial.Missing = %v, want [Bye]", m.Missing)
	}
	if m := memberByType(res, "*main.Mismatch"); m != nil {
		t.Errorf("Mismatch should be excluded (signature mismatch), got %+v", m)
	}
	if m := memberByType(res, "*main.Unrelated"); m != nil {
		t.Errorf("Unrelated should be excluded, got %+v", m)
	}
	if res.Complete != 2 || res.Partial != 1 {
		t.Errorf("counts: complete=%d partial=%d, want 2/1", res.Complete, res.Partial)
	}
	if m := memberByType(res, "*main.Full"); m != nil && m.Path != "a.go" {
		t.Errorf("Full path = %q, want a.go", m.Path)
	}
}

func TestImplementorsOf_NotFound(t *testing.T) {
	root := writeIfaceModule(t)
	res, _ := ImplementorsOf(context.Background(), root, "NoSuchIface")
	if res.Status != "not-found" {
		t.Errorf("status = %q, want not-found", res.Status)
	}
}

func TestImplementorsOf_NotFoundHint(t *testing.T) {
	root := writeIfaceModule(t)

	// Passing a struct name should give a hint mentioning "struct type".
	res, _ := ImplementorsOf(context.Background(), root, "Full")
	if res.Status != "not-found" {
		t.Fatalf("status = %q, want not-found", res.Status)
	}
	if !strings.Contains(res.Hint, "struct") {
		t.Errorf("hint for struct type = %q, want it to mention 'struct'", res.Hint)
	}

	// Unknown name should fall back to the dex find suggestion.
	res2, _ := ImplementorsOf(context.Background(), root, "NoSuchThing")
	if !strings.Contains(res2.Hint, "dex find") {
		t.Errorf("hint for unknown symbol = %q, want it to suggest 'dex find'", res2.Hint)
	}
}

func TestImplementorsOf_Unsupported(t *testing.T) {
	res, _ := ImplementorsOf(context.Background(), t.TempDir(), "Greeter") // no go.mod
	if res.Status != "unsupported-language" {
		t.Errorf("status = %q, want unsupported-language", res.Status)
	}
}
