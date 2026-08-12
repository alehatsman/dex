package mcp

import (
	"reflect"
	"testing"
)

// canonicalTestCommand prefers the tasks.yml/Makefile/package.json test command
// and falls back to the language default — the #146 rework that stops verify
// guessing bare `go test` in tag-requiring repos.
func TestCanonicalTestCommand(t *testing.T) {
	// tasks.yml test target → mooncake task test (dex's own case)
	tasksRepo := t.TempDir()
	writeRepoFile(t, tasksRepo, "tasks.yml", "tasks:\n  test: goq/test\n")
	if got := canonicalTestCommand(tasksRepo); got != "mooncake task test" {
		t.Errorf("tasks.yml canonical = %q, want mooncake task test", got)
	}

	// bare go.mod, no runner → language fallback go test ./...
	goRepo := t.TempDir()
	writeRepoFile(t, goRepo, "go.mod", "module x\n\ngo 1.26\n")
	if got := canonicalTestCommand(goRepo); got != "go test ./..." {
		t.Errorf("go.mod canonical = %q, want go test ./...", got)
	}

	// nothing detected → empty (verify uses its go-test default)
	if got := canonicalTestCommand(t.TempDir()); got != "" {
		t.Errorf("empty repo canonical = %q, want \"\"", got)
	}
}

func TestImpactFiles(t *testing.T) {
	imp := ImpactOutput{
		Targets:    []TargetMatch{{Path: "a.go"}},
		Nodes:      []ImpactNode{{Path: "b.go"}},
		TestsToRun: []string{"a_test.go"},
	}
	got := impactFiles(imp)
	want := []string{"a.go", "b.go", "a_test.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("impactFiles = %v, want %v", got, want)
	}
}
