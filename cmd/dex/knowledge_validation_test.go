package main

import (
	"context"
	"strings"
	"testing"
)

func TestCanonicalArchetype(t *testing.T) {
	ok := map[string]string{
		"Architecture":   "Architecture",
		"gotcha":         "Gotcha",
		"DECISION":       "Decision",
		"  convention":   "Convention",
		"dependency":     "Dependency",
		"Pattern":        "Pattern",
		"fact":           "Fact",
		"Observation":    "Observation",
		"observation":    "Observation",
		"OBSERVATION":    "Observation",
		"hypothesis":     "Hypothesis",
		"Hypothesis":     "Hypothesis",
		"inference":      "Inference",
		"INFERENCE":      "Inference",
		"ReviewFinding":  "ReviewFinding",
		"reviewfinding":  "ReviewFinding",
		"review-finding": "ReviewFinding",
		"review_finding": "ReviewFinding",
		"verified-fact":  "VerifiedFact",
		"VerifiedFact":   "VerifiedFact",
		"verifiedfact":   "VerifiedFact",
		"verified_fact":  "VerifiedFact",
	}
	for in, want := range ok {
		got, valid := canonicalArchetype(in)
		if !valid || got != want {
			t.Errorf("canonicalArchetype(%q) = (%q, %v), want (%q, true)", in, got, valid, want)
		}
	}
	for _, bad := range []string{"", "Bogus", "note", "arch"} {
		if got, valid := canonicalArchetype(bad); valid {
			t.Errorf("canonicalArchetype(%q) = (%q, true), want invalid", bad, got)
		}
	}
}

// TestNotesAddRejectsBadFlags locks issue #520: notes add must reject an
// unknown --archetype and an explicitly out-of-range --confidence before
// touching the store, rather than silently coercing them. Validation runs
// before index resolution, so these need no index.
func TestNotesAddRejectsBadFlags(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		args []string
		want string // substring expected in the error
	}{
		{"unknown archetype", []string{"--archetype", "Bogus", "some body"}, "invalid --archetype"},
		{"negative confidence", []string{"--confidence=-0.5", "some body"}, "invalid --confidence"},
		{"confidence above 1", []string{"--confidence=5", "some body"}, "invalid --confidence"},
		{"explicit zero confidence", []string{"--confidence=0", "some body"}, "invalid --confidence"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := cmdKnowledgeAdd(ctx, c.args)
			if err == nil {
				t.Fatalf("cmdKnowledgeAdd(%v) = nil, want error %q", c.args, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), c.want)
			}
		})
	}
}

// TestNotesAddAcceptsValidFlags confirms the validation does not reject good
// input: a case-folded archetype and an unset confidence pass validation and
// fail only later (no index in this temp dir), never with an "invalid --"
// message.
func TestNotesAddAcceptsValidFlags(t *testing.T) {
	ctx := context.Background()
	// Run from a dir with no index so the command stops at index resolution,
	// after validation has already passed.
	dir := t.TempDir()
	t.Chdir(dir)

	for _, args := range [][]string{
		{"--archetype", "gotcha", "a valid fact"},
		{"--archetype", "Observation", "an observation"},
		{"--archetype", "ReviewFinding", "--scope", "internal/mcp/server.go", "[god-object] a review finding"},
		{"--archetype", "Decision", "--confidence=0.9", "another fact"},
		{"a fact with default archetype and confidence"},
	} {
		err := cmdKnowledgeAdd(ctx, args)
		if err != nil && strings.Contains(err.Error(), "invalid --") {
			t.Errorf("cmdKnowledgeAdd(%v) rejected valid input: %v", args, err)
		}
	}
}

// TestNotesProjectFlag locks #89: every notes verb accepts the project as
// `--project <dir>`, not only as the positional <path>. Previously `notes add
// --project <dir>` died with a cryptic "flag provided but not defined: -project".
// With no index at <dir> the verbs must stop at index resolution (a "no index"
// error naming the dir) — proving the flag parsed and routed — never with a
// flag-parse error.
func TestNotesProjectFlag(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// Run from a different cwd so only --project can point at dir.
	t.Chdir(t.TempDir())

	cases := []struct {
		name string
		run  func() error
	}{
		{"add", func() error { return cmdKnowledgeAdd(ctx, []string{"--project", dir, "--archetype", "Fact", "a body"}) }},
		{"list", func() error { return cmdKnowledgeQuery(ctx, []string{"--project", dir}) }},
		{"gc", func() error { return cmdKnowledgeGC(ctx, []string{"--project", dir}) }},
		{"review", func() error { return cmdKnowledgeReview(ctx, []string{"--project", dir}) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.run()
			if err == nil {
				t.Fatalf("notes %s --project %s = nil, want a no-index error", c.name, dir)
			}
			if strings.Contains(err.Error(), "not defined") {
				t.Errorf("notes %s rejected --project as an unknown flag: %v", c.name, err)
			}
			if !strings.Contains(err.Error(), "no index") {
				t.Errorf("notes %s error = %q, want it to stop at index resolution (--project routed)", c.name, err)
			}
		})
	}
}
