package eval

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGenerateOrphan_basic(t *testing.T) {
	unsetGitEnv(t)
	dir := gitInitRepo(t)

	// File with exported const and a non-stdlib import.
	write(t, dir, "config/config.go", `package config

import "gopkg.in/yaml.v3"

const DefaultTimeout = 30

type Config struct{}

func Load(path string) (*Config, error) {
	var c Config
	_ = yaml.NewDecoder(nil)
	return &c, nil
}
`)

	// File with error sentinel vars.
	write(t, dir, "store/store.go", `package store

import "errors"

var errNotFound = errors.New("not found")
var errConflict = errors.New("conflict")
`)

	// Test file — must be excluded.
	write(t, dir, "store/store_test.go", `package store

const testK = 5
`)

	gitCommitAll(t, dir, "initial commit")

	ctx := context.Background()
	gs, counts, err := GenerateOrphan(ctx, dir, OrphanOpts{MaxPerKind: 50})
	if err != nil {
		t.Fatal(err)
	}

	// Should have produced some imports, consts, vars.
	if counts.Imports == 0 {
		t.Error("want at least one import query; got 0")
	}
	if counts.Consts == 0 {
		t.Error("want at least one const query; got 0")
	}
	if counts.Vars == 0 {
		t.Error("want at least one var query; got 0")
	}

	// The const query should reference config/config.go.
	var constQ *GoldenQuery
	for i := range gs.Queries {
		q := &gs.Queries[i]
		if strings.Contains(q.Query, "default") && strings.Contains(q.Query, "timeout") {
			constQ = q
		}
	}
	if constQ == nil {
		var qs []string
		for _, q := range gs.Queries {
			qs = append(qs, q.Query)
		}
		t.Fatalf("no query for DefaultTimeout constant; got: %v", qs)
	}
	wantFile := filepath.Join("config", "config.go")
	if !slices.Contains(constQ.RelevantFiles, wantFile) {
		t.Errorf("DefaultTimeout query relevant_files = %v, want to contain %q", constQ.RelevantFiles, wantFile)
	}

	// The var queries should reference store/store.go.
	var foundNotFound bool
	storeFile := filepath.Join("store", "store.go")
	for _, q := range gs.Queries {
		if strings.Contains(q.Query, "not found") && strings.Contains(q.Query, "error") {
			if slices.Contains(q.RelevantFiles, storeFile) {
				foundNotFound = true
			}
		}
	}
	if !foundNotFound {
		t.Errorf("no error-sentinel query for errNotFound pointing to %q", storeFile)
	}

	// Test files must be excluded.
	for _, q := range gs.Queries {
		for _, f := range q.RelevantFiles {
			if strings.HasSuffix(f, "_test.go") {
				t.Errorf("test file leaked into golden set: %q", f)
			}
		}
	}

	// All relevant_files must exist as paths relative to dir.
	for _, q := range gs.Queries {
		for _, f := range q.RelevantFiles {
			if filepath.IsAbs(f) {
				t.Errorf("query %q has absolute path %q — want relative", q.ID, f)
			}
		}
	}

	// Query texts must be unique.
	seen := map[string]bool{}
	for _, q := range gs.Queries {
		if seen[q.Query] {
			t.Errorf("duplicate query text: %q", q.Query)
		}
		seen[q.Query] = true
	}
}

func TestGenerateOrphan_maxPerKind(t *testing.T) {
	unsetGitEnv(t)
	dir := gitInitRepo(t)

	// Write a file with many consts.
	var sb strings.Builder
	sb.WriteString("package big\n\nconst (\n")
	for i := 0; i < 20; i++ {
		sb.WriteString("\tMaxValueAlpha")
		for j := 0; j < i; j++ {
			sb.WriteRune('Z')
		}
		sb.WriteString(" = 1\n")
	}
	sb.WriteString(")\n")
	write(t, dir, "big/big.go", sb.String())
	gitCommitAll(t, dir, "add big consts")

	_, counts, err := GenerateOrphan(context.Background(), dir, OrphanOpts{MaxPerKind: 5})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Consts > 5 {
		t.Errorf("want ≤5 const queries (MaxPerKind=5), got %d", counts.Consts)
	}
}

func TestSplitIdent(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"MaxRetries", []string{"max", "retries"}},
		{"defaultTimeout", []string{"default", "timeout"}},
		{"err_not_found", []string{"err", "not", "found"}},
		{"K", []string{}}, // single-char, filtered
		{"_", []string{}}, // blank
		{"errNotFound", []string{"err", "not", "found"}},
	}
	for _, c := range cases {
		got := splitIdent(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("splitIdent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
