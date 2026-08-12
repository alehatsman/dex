package mcp

import "testing"

// #155 P3 emit: smells/clones project into the shared gate finding schema so
// `dex smells|clones --format jsonl` is gate-pluggable.

func TestSmellsGateFindings(t *testing.T) {
	out := SmellsOutput{
		LongFunctions: []SmellHit{{QualifiedName: "pkg.Big", Path: "a.go", StartLine: 10, Lines: 200}},
		DeadExports:   []SmellHit{{QualifiedName: "pkg.Unused", Path: "b.go", StartLine: 5}},
		GodFiles:      []GodFileHit{{Path: "c.go", SymbolCount: 42}},
		GodNodes:      []SmellHit{{QualifiedName: "pkg.Hub", Path: "d.go", StartLine: 3}},
	}
	fs := out.GateFindings()
	if len(fs) != 4 {
		t.Fatalf("want 4 findings, got %d: %+v", len(fs), fs)
	}
	byRule := map[string]GateFinding{}
	for _, f := range fs {
		if f.Tool != "smells" {
			t.Errorf("tool = %q, want smells", f.Tool)
		}
		if f.Level != "warning" {
			t.Errorf("%s level = %q, want warning", f.Rule, f.Level)
		}
		if f.Fingerprint == "" || f.Path == "" {
			t.Errorf("%s: path/fingerprint must be set for standalone emit: %+v", f.Rule, f)
		}
		byRule[f.Rule] = f
	}
	for _, want := range []string{"long-function", "dead-export", "god-file", "god-node"} {
		if _, ok := byRule[want]; !ok {
			t.Errorf("missing rule %q", want)
		}
	}
	if got := byRule["god-file"]; got.Line != 1 || got.Path != "c.go" {
		t.Errorf("god-file = %+v, want line 1 @ c.go", got)
	}
	if got := byRule["long-function"]; got.Fingerprint != "long-function:a.go:10" {
		t.Errorf("fingerprint = %q, want long-function:a.go:10", got.Fingerprint)
	}
}

func TestClonesGateFindings(t *testing.T) {
	out := ClonesOutput{
		Clusters: []CloneClusterOut{{
			Size:       2,
			Similarity: 0.95,
			Members: []CloneMemberOut{
				{Path: "x.go", StartLine: 20, Name: "Foo"},
				{Path: "y.go", StartLine: 40, Name: "Bar"},
			},
		}},
	}
	fs := out.GateFindings()
	if len(fs) != 2 { // one finding per member block
		t.Fatalf("want 2 findings, got %d: %+v", len(fs), fs)
	}
	for _, f := range fs {
		if f.Tool != "clones" || f.Rule != "clone" || f.Level != "warning" {
			t.Errorf("unexpected shape: %+v", f)
		}
	}
	if fs[0].Path != "x.go" || fs[0].Line != 20 || fs[0].Fingerprint != "clone:x.go:20" {
		t.Errorf("member 0 = %+v, want x.go:20", fs[0])
	}
}
