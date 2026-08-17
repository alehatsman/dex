package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractReferents(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []referent // subset that must be present (kind+raw)
		deny []string   // raw strings that must NOT be extracted
	}{
		{
			name: "path with line",
			body: "the taper lives in internal/store/store_knowledge.go:332 today",
			want: []referent{{Kind: kindPathLine, Raw: "internal/store/store_knowledge.go:332", Path: "internal/store/store_knowledge.go", Line: 332}},
		},
		{
			name: "bare path",
			body: "see internal/mcp/server.go for the handler",
			want: []referent{{Kind: kindPath, Raw: "internal/mcp/server.go", Path: "internal/mcp/server.go"}},
		},
		{
			name: "path:line does not double count as bare path",
			body: "cmd/dex/env.go:75 defines the flag",
			want: []referent{{Kind: kindPathLine, Raw: "cmd/dex/env.go:75", Path: "cmd/dex/env.go", Line: 75}},
			deny: []string{"cmd/dex/env.go"}, // suppressed by masking
		},
		{
			name: "method receiver symbol",
			body: "(*Store).FindSymbol resolves it",
			want: []referent{{Kind: kindSymbol, Raw: "(*Store).FindSymbol", Name: "FindSymbol"}},
		},
		{
			name: "dotted package symbol",
			body: "call store.CodeFilePaths once per recall",
			want: []referent{{Kind: kindSymbol, Raw: "store.CodeFilePaths", Name: "CodeFilePaths"}},
		},
		{
			name: "backtick identifier is a symbol",
			body: "the `RecencyFactor` term dominates",
			want: []referent{{Kind: kindSymbol, Raw: "`RecencyFactor`", Name: "RecencyFactor"}},
		},
		{
			name: "bare CamelCase word is NOT a symbol",
			body: "Salience is computed on read from Confidence",
			deny: []string{"Salience", "Confidence"},
		},
		{
			name: "prose colon-number is NOT a pathline",
			body: "step 3: 42 facts remained after gc",
			deny: []string{"3:42", "3: 42"},
		},
		{
			name: "single-segment filename is not a bare path",
			body: "the tasks.yml preset threads GO_TAGS",
			deny: []string{"tasks.yml"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractReferents(tc.body)
			for _, w := range tc.want {
				found := false
				for _, g := range got {
					if g.Kind == w.Kind && g.Raw == w.Raw {
						found = true
						if w.Path != "" && g.Path != w.Path {
							t.Errorf("referent %q path = %q, want %q", w.Raw, g.Path, w.Path)
						}
						if w.Line != 0 && g.Line != w.Line {
							t.Errorf("referent %q line = %d, want %d", w.Raw, g.Line, w.Line)
						}
						if w.Name != "" && g.Name != w.Name {
							t.Errorf("referent %q name = %q, want %q", w.Raw, g.Name, w.Name)
						}
					}
				}
				if !found {
					t.Errorf("want referent %s/%q not extracted from %q; got %+v", w.Kind, w.Raw, tc.body, got)
				}
			}
			for _, d := range tc.deny {
				for _, g := range got {
					if g.Raw == d {
						t.Errorf("must NOT extract %q from %q, but did (%s)", d, tc.body, g.Kind)
					}
				}
			}
		})
	}
}

func TestDeadReferentNote(t *testing.T) {
	// Indexed world: server.go exists (200 lines), knowledge.go exists (100 lines).
	paths := map[string]int{
		"internal/mcp/server.go":            200,
		"internal/store/store_knowledge.go": 100,
	}
	exts := indexedExts(paths) // {.go}
	liveSyms := map[string]bool{"FindSymbol": true}
	symbolLive := func(name string) bool { return liveSyms[name] }

	cases := []struct {
		name     string
		refs     []referent
		wantDead bool
		wantSub  string // substring the note must contain when dead
	}{
		{
			name:     "live path is not flagged",
			refs:     []referent{{Kind: kindPath, Raw: "internal/mcp/server.go", Path: "internal/mcp/server.go"}},
			wantDead: false,
		},
		{
			name:     "missing file is flagged",
			refs:     []referent{{Kind: kindPath, Raw: "internal/mcp/gone.go", Path: "internal/mcp/gone.go"}},
			wantDead: true,
			wantSub:  "file no longer indexed",
		},
		{
			name:     "line past EOF is flagged",
			refs:     []referent{{Kind: kindPathLine, Raw: "internal/store/store_knowledge.go:999", Path: "internal/store/store_knowledge.go", Line: 999}},
			wantDead: true,
			wantSub:  "file now ends at line 100",
		},
		{
			name:     "in-bounds line is not flagged",
			refs:     []referent{{Kind: kindPathLine, Raw: "internal/store/store_knowledge.go:42", Path: "internal/store/store_knowledge.go", Line: 42}},
			wantDead: false,
		},
		{
			name:     "unindexed extension is not judged",
			refs:     []referent{{Kind: kindPath, Raw: "config/thing.yml", Path: "config/thing.yml"}},
			wantDead: false,
		},
		{
			name:     "live symbol is not flagged",
			refs:     []referent{{Kind: kindSymbol, Raw: "(*Store).FindSymbol", Name: "FindSymbol"}},
			wantDead: false,
		},
		{
			name:     "dead symbol is flagged",
			refs:     []referent{{Kind: kindSymbol, Raw: "OldGone", Name: "OldGone"}},
			wantDead: true,
			wantSub:  "symbol not found",
		},
		{
			name: "one live anchor keeps the fact grounded (all-fail, not any-fail)",
			refs: []referent{
				{Kind: kindPath, Raw: "internal/mcp/gone.go", Path: "internal/mcp/gone.go"},
				{Kind: kindPath, Raw: "internal/mcp/server.go", Path: "internal/mcp/server.go"},
			},
			wantDead: false,
		},
		{
			name: "dead path but live symbol → grounded (different kinds, any live kind saves it)",
			refs: []referent{
				{Kind: kindPath, Raw: "internal/mcp/gone.go", Path: "internal/mcp/gone.go"},
				{Kind: kindSymbol, Raw: "FindSymbol", Name: "FindSymbol"},
			},
			wantDead: false,
		},
		{
			name:     "no referents → not flagged",
			refs:     nil,
			wantDead: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note := deadReferentNote(tc.refs, paths, exts, symbolLive)
			if tc.wantDead && note == "" {
				t.Fatalf("expected dead note, got none")
			}
			if !tc.wantDead && note != "" {
				t.Fatalf("expected no note, got %q", note)
			}
			if tc.wantDead && tc.wantSub != "" && !strings.Contains(note, tc.wantSub) {
				t.Errorf("note %q missing substring %q", note, tc.wantSub)
			}
		})
	}
}

// TestKnowledgeLivenessWiring drives the whole recall path against a real
// indexed store: a fact naming live code is returned clean, while a fact naming
// a since-vanished path/symbol comes back flagged needs_verification (#167).
func TestKnowledgeLivenessWiring(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "pkg", "alpha.go"), `package pkg

// Alpha is the live symbol.
func Alpha() string { return "alpha" }
`)
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	add := func(body string) {
		_, _, err := s.knowledge(ctx, nil, KnowledgeInput{Action: "add", Body: body, ProjectRoot: root})
		if err != nil {
			t.Fatalf("add %q: %v", body, err)
		}
	}
	const liveBody = "the `Alpha` helper in pkg/alpha.go returns a string"
	const deadBody = "the entrypoint moved to internal/gone/missing.go:99 via (*Ghost).Vanish"
	add(liveBody)
	add(deadBody)

	_, out, err := s.knowledge(ctx, nil, KnowledgeInput{Action: "list", ProjectRoot: root, K: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var sawLive, sawDead bool
	for _, f := range out.Facts {
		switch f.Body {
		case liveBody:
			sawLive = true
			if f.NeedsVerification {
				t.Errorf("live fact flagged needs_verification: note=%q", f.VerificationNote)
			}
		case deadBody:
			sawDead = true
			if !f.NeedsVerification {
				t.Errorf("dead-referent fact NOT flagged: %+v", f)
			}
			if !strings.Contains(f.VerificationNote, "missing.go:99") {
				t.Errorf("note %q should name the dead path", f.VerificationNote)
			}
		}
	}
	if !sawLive || !sawDead {
		t.Fatalf("recall did not return both facts (live=%v dead=%v, n=%d)", sawLive, sawDead, len(out.Facts))
	}
}
