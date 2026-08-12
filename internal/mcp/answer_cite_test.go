// Copyright 2026 Aleh Atsman
//
// Regression tests for #526: the synthesized ask answer must not cite a
// file path absent from its own evidence bundle without a steering note.

package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/retrieve"
)

func TestValidateAnswerCitations(t *testing.T) {
	out := &ContextOutput{
		SuggestedReads: []SuggestedRead{{Path: "internal/watch/debounce.go"}},
		SemanticHits:   []SemHit{{Path: "internal/mcp/answer.go"}},
		Symbols:        []SymbolHit{{Path: "cmd/dex/main.go"}},
		Annotations:    map[string]PathMeta{"internal/store/store.go": {}},
	}

	tests := []struct {
		name   string
		answer string
		want   []string
	}{
		{
			name:   "grounded path passes",
			answer: "The debounce logic lives in internal/watch/debounce.go.",
			want:   nil,
		},
		{
			name:   "grounded path with line suffix passes",
			answer: "See internal/mcp/answer.go:42 for synthesis.",
			want:   nil,
		},
		{
			name:   "hallucinated path is flagged",
			answer: "The watcher is in internal/mcp/watch.go.",
			want:   []string{"internal/mcp/watch.go"},
		},
		{
			name:   "bare filename (no slash) is not flagged",
			answer: "It lives in watch.go near the top.",
			want:   nil,
		},
		{
			name:   "import path / domain is not flagged",
			answer: "It wraps github.com/fsnotify/fsnotify.go bindings.",
			want:   nil,
		},
		{
			name:   "duplicates collapse, first-seen order",
			answer: "Both a/b/x.go and a/b/x.go plus c/d/y.go are wrong.",
			want:   []string{"a/b/x.go", "c/d/y.go"},
		},
		{
			name:   "mixed grounded and hallucinated",
			answer: "Real: cmd/dex/main.go. Fake: pkg/fake/z.go.",
			want:   []string{"pkg/fake/z.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateAnswerCitations(tt.answer, out)
			if !equalStrings(got, tt.want) {
				t.Errorf("validateAnswerCitations(%q) = %v, want %v", tt.answer, got, tt.want)
			}
		})
	}
}

func TestValidateAnswerCitationsEmptyEvidence(t *testing.T) {
	// Nothing to validate against → never flag (avoids false positives when
	// the bundle is empty, e.g. a pure session/graph answer).
	got := validateAnswerCitations("cites internal/mcp/watch.go", &ContextOutput{})
	if got != nil {
		t.Errorf("with empty evidence, got %v, want nil", got)
	}
}

func TestSynthesizeAnswerAppendsNoteForHallucinatedPath(t *testing.T) {
	// The fake chat model invents a path not in the evidence; synthesizeAnswer
	// must append the deterministic steering note. End-to-end repro of #526.
	chatSrv := fakeChat(t, "The watcher is wired up in internal/mcp/watch.go.")
	defer chatSrv.Close()
	s := &Server{ChatClient: chat.New(chatSrv.URL, "fake-14b", 5*time.Second), ChatConfigured: true}

	out := &ContextOutput{
		SuggestedReads: []SuggestedRead{
			{Path: "internal/watch/watch.go", StartLine: 1, EndLine: 5, Content: "func watch() {}"},
		},
	}
	s.synthesizeAnswer(context.Background(), nil, retrieve.IntentBehaviorSearch, "where is the watcher", out)

	if !strings.Contains(out.Answer, "[dex] Unverified path(s)") {
		t.Fatalf("expected steering note in answer, got:\n%s", out.Answer)
	}
	if !strings.Contains(out.Answer, "internal/mcp/watch.go") {
		t.Errorf("expected the hallucinated path named in the note, got:\n%s", out.Answer)
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
