package main

import (
	"flag"
	"testing"
)

// TestExplicitKError locks #858: an explicitly-passed non-positive --k must
// error, while an *omitted* --k (whose flag.Int zero value is indistinguishable
// from an explicit 0 without fs.Visit) must not — every lane's own
// `if k <= 0 { k = default }` still owns that case.
func TestExplicitKError(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"k omitted", nil, false},
		{"k omitted, other flags set", []string{"--kind=search"}, false},
		{"explicit k=0", []string{"--k=0"}, true},
		{"explicit k=-1", []string{"--k=-1"}, true},
		{"explicit positive k", []string{"--k=5"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("query", flag.ContinueOnError)
			kind := fs.String("kind", "", "")
			k := fs.Int("k", 0, "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("fs.Parse(%v): %v", tc.args, err)
			}
			_ = kind
			err := explicitKError(fs, *k)
			if (err != nil) != tc.wantErr {
				t.Errorf("explicitKError(k=%d, args=%v) = %v, wantErr %v", *k, tc.args, err, tc.wantErr)
			}
		})
	}
}

// TestResolveQueryRoot locks #858: kind=deps must not have its sole positional
// swallowed as the project-root arg the way every other kind's does — that's
// exactly the ambiguity `dex help all` documents escape hatches for
// (--project-root, a file inside the package, the full import path), and
// silently mis-resolving it produces a confusing "no index for <subdirectory>"
// instead of using the target the user actually typed.
func TestResolveQueryRoot(t *testing.T) {
	// "." is a real directory relative to any cwd, so splitProjectArg's
	// os.Stat(...).IsDir() branch actually fires the way a real relative
	// package directory (`cmd/dex`, `internal/gitenv`) would.
	const realDir = "."

	cases := []struct {
		name            string
		kind            string
		projectRootFlag string
		rest            []string
		wantRoot        string
		wantRemaining   []string
	}{
		{
			name: "non-deps kind still peels a leading directory positional",
			kind: "search", rest: []string{realDir, "query", "words"},
			wantRoot: realDir, wantRemaining: []string{"query", "words"},
		},
		{
			name: "deps kind keeps its positional instead of peeling it as root",
			kind: "deps", rest: []string{realDir},
			wantRoot: ".", wantRemaining: []string{realDir},
		},
		{
			name: "explicit --project-root wins for deps too",
			kind: "deps", projectRootFlag: "/some/explicit/root", rest: []string{realDir},
			wantRoot: "/some/explicit/root", wantRemaining: []string{realDir},
		},
		{
			name: "explicit --project-root wins for a non-deps kind",
			kind: "search", projectRootFlag: "/some/explicit/root", rest: []string{realDir, "words"},
			wantRoot: "/some/explicit/root", wantRemaining: []string{realDir, "words"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, remaining := resolveQueryRoot(tc.kind, tc.projectRootFlag, tc.rest)
			if root != tc.wantRoot {
				t.Errorf("root = %q, want %q", root, tc.wantRoot)
			}
			if !equalStrings(remaining, tc.wantRemaining) {
				t.Errorf("remaining = %v, want %v", remaining, tc.wantRemaining)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
