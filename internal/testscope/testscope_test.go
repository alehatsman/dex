package testscope

import (
	"reflect"
	"testing"
)

func TestGoPackagesForFiles(t *testing.T) {
	got := GoPackagesForFiles([]string{
		"internal/mcp/server.go",
		"internal/mcp/server_test.go", // same package → deduped
		"internal/store/store.go",
		"main.go",       // root package
		"README.md",     // non-go → dropped
		"docs/tools.md", // non-go → dropped
	})
	want := []string{".", "./internal/mcp", "./internal/store"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GoPackagesForFiles = %v, want %v", got, want)
	}
}

func TestGoPackagesForFiles_NoGo(t *testing.T) {
	if got := GoPackagesForFiles([]string{"README.md", "Makefile"}); len(got) != 0 {
		t.Errorf("non-go files must yield no packages, got %v", got)
	}
}

func TestSiblingTestFiles(t *testing.T) {
	got := SiblingTestFiles([]string{
		"internal/mcp/server.go",      // → server_test.go
		"internal/mcp/server_test.go", // already a test → itself, deduped with above
		"x/y.go",                      // → y_test.go
		"notes.md",                    // dropped
	})
	want := []string{"internal/mcp/server_test.go", "x/y_test.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SiblingTestFiles = %v, want %v", got, want)
	}
}

func TestSynthVerifyCommand(t *testing.T) {
	pkgs := []string{"./internal/mcp", "./internal/store"}

	// explicit override with placeholder
	if got := SynthVerifyCommand("go test -tags sqlite_fts5 {{packages}}", pkgs); got != "go test -tags sqlite_fts5 ./internal/mcp ./internal/store" {
		t.Errorf("override+placeholder = %q", got)
	}
	// override without placeholder → packages appended
	if got := SynthVerifyCommand("gotestsum --", pkgs); got != "gotestsum -- ./internal/mcp ./internal/store" {
		t.Errorf("override no-placeholder = %q", got)
	}
	// env fallback
	t.Setenv("DEX_VERIFY_CMD", "go test -race {{packages}}")
	if got := SynthVerifyCommand("", pkgs); got != "go test -race ./internal/mcp ./internal/store" {
		t.Errorf("env fallback = %q", got)
	}
	// default when nothing set
	t.Setenv("DEX_VERIFY_CMD", "")
	if got := SynthVerifyCommand("", pkgs); got != "go test ./internal/mcp ./internal/store" {
		t.Errorf("default = %q", got)
	}
}
