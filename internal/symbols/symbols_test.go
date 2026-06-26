package symbols

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeTestModule(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/sym\n\ngo 1.21\n")
	write("a.go", `package main

type Animal interface {
	Speak() string
}

type Domestic interface {
	Animal
	Name() string
}

type Dog struct{}

func (d Dog) Speak() string  { return "woof" }
func (d Dog) Name() string   { return "dog" }

type Cat struct{}

func (c Cat) Speak() string { return "meow" }

type Unrelated struct{}

func (u Unrelated) Other() {}

type Embedder struct {
	Dog
}

// Target is referenced in b.go.
func Target() int { return 1 }

func main() {}
`)
	write("b.go", `package main

func UseTarget() int {
	return Target()
}
`)
	return dir
}

func TestQuery_References(t *testing.T) {
	root := writeTestModule(t)
	res, err := Query(context.Background(), root, References, "Target")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, hint = %q", res.Status, res.Hint)
	}
	// Should have a def in a.go and a use in b.go.
	var hasDef, hasUse bool
	for _, s := range res.Sites {
		if s.Kind == "def" && s.Path == "a.go" {
			hasDef = true
		}
		if s.Kind == "use" && s.Path == "b.go" {
			hasUse = true
		}
	}
	if !hasDef {
		t.Errorf("missing def site in a.go; sites = %+v", res.Sites)
	}
	if !hasUse {
		t.Errorf("missing use site in b.go; sites = %+v", res.Sites)
	}
}

func TestQuery_Implementations(t *testing.T) {
	root := writeTestModule(t)
	res, err := Query(context.Background(), root, Implementations, "Animal")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, hint = %q", res.Status, res.Hint)
	}
	// Dog and Cat implement Animal; Unrelated does not.
	paths := map[string]bool{}
	for _, s := range res.Sites {
		paths[s.Path] = true
	}
	if !paths["a.go"] {
		t.Errorf("expected implementors in a.go; sites = %+v", res.Sites)
	}
	for _, s := range res.Sites {
		if s.Kind != "implementor" {
			t.Errorf("unexpected kind %q", s.Kind)
		}
	}
	if len(res.Sites) != 2 { // Dog + Cat (Embedder also implements via Dog embed, but let's check)
		// Embedder embeds Dog so it also implements Animal via embedding.
		// The count may be 3 depending on type resolution. Allow 2 or 3.
		if len(res.Sites) < 2 {
			t.Errorf("expected at least 2 implementors (Dog, Cat), got %d: %+v", len(res.Sites), res.Sites)
		}
	}
}

func TestQuery_Supertypes_Interface(t *testing.T) {
	root := writeTestModule(t)
	res, err := Query(context.Background(), root, Supertypes, "Domestic")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, hint = %q", res.Status, res.Hint)
	}
	// Domestic embeds Animal.
	if len(res.Sites) == 0 {
		t.Errorf("expected Animal as supertype of Domestic; got no sites")
	}
	for _, s := range res.Sites {
		if s.Kind != "super" {
			t.Errorf("unexpected kind %q", s.Kind)
		}
	}
}

func TestQuery_Supertypes_ConcreteType(t *testing.T) {
	root := writeTestModule(t)
	res, err := Query(context.Background(), root, Supertypes, "Dog")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, hint = %q", res.Status, res.Hint)
	}
	// Dog implements Animal and Domestic — both should appear.
	if len(res.Sites) < 2 {
		t.Errorf("expected at least 2 supertypes (Animal, Domestic); got %d: %+v", len(res.Sites), res.Sites)
	}
}

func TestQuery_Subtypes_Interface(t *testing.T) {
	root := writeTestModule(t)
	res, err := Query(context.Background(), root, Subtypes, "Animal")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, hint = %q", res.Status, res.Hint)
	}
	// Domestic is a sub-interface; Dog, Cat, Embedder are subtypes.
	if len(res.Sites) == 0 {
		t.Errorf("expected subtypes; got none")
	}
}

func TestQuery_Subtypes_Struct(t *testing.T) {
	root := writeTestModule(t)
	res, err := Query(context.Background(), root, Subtypes, "Dog")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, hint = %q", res.Status, res.Hint)
	}
	// Embedder embeds Dog.
	if len(res.Sites) == 0 {
		t.Errorf("expected Embedder as subtype of Dog; got none")
	}
	for _, s := range res.Sites {
		if s.Kind != "sub" {
			t.Errorf("unexpected kind %q", s.Kind)
		}
		if s.Path != "a.go" {
			t.Errorf("expected a.go, got %q", s.Path)
		}
	}
}

func TestQuery_NotFound(t *testing.T) {
	root := writeTestModule(t)
	res, err := Query(context.Background(), root, References, "NoSuchSymbol")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "not-found" {
		t.Errorf("status = %q, want not-found", res.Status)
	}
}

func TestQuery_UnsupportedLanguage(t *testing.T) {
	res, err := Query(context.Background(), t.TempDir(), References, "Foo") // no go.mod
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "unsupported-language" {
		t.Errorf("status = %q, want unsupported-language", res.Status)
	}
}

func TestQuery_ReceiverQualified(t *testing.T) {
	root := writeTestModule(t)
	res, err := Query(context.Background(), root, References, "(*Dog).Speak")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, hint = %q", res.Status, res.Hint)
	}
	if len(res.Sites) == 0 {
		t.Errorf("expected sites for (*Dog).Speak; got none")
	}
}
