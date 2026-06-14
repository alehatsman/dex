package main

import (
	"testing"

	"github.com/alehatsman/dex/internal/retrieve"
)

// TestValidExpandMode locks the CLI `ask --expand` validator (#503) to the
// expansion vocabulary the MCP `ask` tool exposes. Empty defers to the server
// default; off/on/full are the levels; input is trimmed and case-folded to
// mirror retrieve.ResolveExpandMode.
func TestValidExpandMode(t *testing.T) {
	valid := []string{"", "off", "on", "full", "FULL", "  on  ", "On"}
	for _, v := range valid {
		if !validExpandMode(v) {
			t.Errorf("validExpandMode(%q) = false, want true", v)
		}
	}
	invalid := []string{"yes", "1", "expand", "of", "fully", "on full"}
	for _, v := range invalid {
		if validExpandMode(v) {
			t.Errorf("validExpandMode(%q) = true, want false", v)
		}
	}
}

// TestValidExpandModeParity guards against drift: every level the CLI accepts
// (other than the empty defer) must be a level retrieve.ResolveExpandMode
// recognises as a real mode (not the silent off fallback), and vice versa.
func TestValidExpandModeParity(t *testing.T) {
	// retrieve resolves these to their named modes; an unrecognised value
	// would clamp to ExpandOff with an empty server default.
	cases := map[string]retrieve.ExpandMode{
		"off":  retrieve.ExpandOff,
		"on":   retrieve.ExpandOn,
		"full": retrieve.ExpandFull,
	}
	for v, want := range cases {
		if !validExpandMode(v) {
			t.Errorf("CLI rejects %q but retrieve accepts it", v)
		}
		if got := retrieve.ResolveExpandMode(v, ""); got != want {
			t.Errorf("ResolveExpandMode(%q,\"\") = %v, want %v", v, got, want)
		}
	}
}
