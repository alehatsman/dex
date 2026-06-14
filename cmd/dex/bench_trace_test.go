package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/alehatsman/dex/internal/eval/corpus"
)

func TestSetLabel(t *testing.T) {
	for in, want := range map[string]string{
		"benchmark/trace/dex-go.json": "dex-go",
		"/abs/path/flask.json":        "flask",
		"noext":                       "noext",
	} {
		if got := setLabel(in); got != want {
			t.Errorf("setLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitCSV(t *testing.T) {
	got := splitCSV(" a.json , ,b.json,")
	want := []string{"a.json", "b.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitCSV = %v, want %v", got, want)
	}
	if got := splitCSV(""); got != nil {
		t.Errorf("splitCSV(\"\") = %v, want nil", got)
	}
}

func TestResolveTraceSets(t *testing.T) {
	spec := corpus.RepoSpec{TraceSets: []string{"trace/a.json", "/abs/b.json"}}
	got := resolveTraceSets(spec, "/manifest/dir")
	want := []string{filepath.Join("/manifest/dir", "trace/a.json"), "/abs/b.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveTraceSets = %v, want %v", got, want)
	}
}
