package retrieve

import "testing"

// ─── ResolveIntent ─────────────────────────────────────────────────────────

func TestResolveIntent(t *testing.T) {
	cases := []struct {
		name     string
		question string
		intent   string
		want     string
	}{
		// Explicit Intent wins.
		{"explicit callers", "fix the bug", IntentCallers, IntentCallers},
		{"explicit upper", "fix the bug", "ARCHITECTURE", IntentArchitecture},
		{"explicit auto falls through", "fix the rerank pool", "auto", IntentEditingContext},
		{"invalid intent falls through", "callers of Foo", "frobnicate", IntentCallers},

		// Keyword regex.
		{"callers", "callers of (*Store).Search", "", IntentCallers},
		{"who calls", "who calls Search", "", IntentCallers},
		{"callees", "what does Search call", "", IntentCallees},
		{"architecture", "how does indexing work", "", IntentArchitecture},
		{"overview", "give me an overview of the indexer", "", IntentArchitecture},
		{"packages", "show the package topology", "", IntentPackageTopology},

		// Orient (#135): whole-repo orientation → the orient bundle.
		{"orient understand repo", "understand this repo", "", IntentOrient},
		{"orient overview codebase", "give me an overview of the codebase", "", IntentOrient},
		{"orient how structured", "how is this project structured", "", IntentOrient},
		{"orient walk me through", "walk me through the codebase", "", IntentOrient},
		{"orient subjectless", "orient me", "", IntentOrient},
		{"orient where start", "where do i start", "", IntentOrient},
		{"orient what does repo do", "what does this repo do", "", IntentOrient},
		{"explicit orient", "some question", "orient", IntentOrient},
		// Narrowness guards — a specific component subject stays architecture,
		// not orient. The repo|codebase|project noun is the trigger, not the verb.
		{"orient not: component how", "how does the watcher work", "", IntentArchitecture},
		{"orient not: component structure", "how is the store organized", "", IntentArchitecture},
		{"orient not: package overview", "overview of the graph package", "", IntentArchitecture},
		{"orient not: code path", "explain the code path for auth", "", IntentBehaviorSearch},
		{"editing", "fix the rerank pool overflow", "", IntentEditingContext},

		// Bare identifier query → symbol_lookup.
		{"bare qualified", "(*Store).Search", "", IntentSymbolLookup},
		{"bare pascal", "OpenWith", "", IntentSymbolLookup},
		{"bare camel", "inlineContent", "", IntentSymbolLookup},

		// Default: behavior_search.
		{"plain question", "where do we open the SQLite store", "", IntentBehaviorSearch},

		// Priority: callers beats editing when both present.
		{"callers beats editing", "fix the callers of Search", "", IntentCallers},

		// change/update no longer trigger editing_context — they're too
		// noisy on questions like "when X changes" / "update the timestamp".
		{"change is behavior_search", "where does the cache invalidate when chunks change", "", IntentBehaviorSearch},
		{"update is behavior_search", "what triggers an update to last_indexed", "", IntentBehaviorSearch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := ResolveIntent(tc.question, tc.intent)
			if got != tc.want {
				t.Errorf("ResolveIntent(%q, %q) = %q, want %q", tc.question, tc.intent, got, tc.want)
			}
		})
	}
}

// ─── ExtractIdentifiers ────────────────────────────────────────────────────

func TestExtractIdentifiers(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"callers of (*Store).Search", []string{"(*Store).Search"}},
		{"where is OpenWith defined", []string{"OpenWith"}},
		{"the user_role table and old_users column", []string{"user_role", "old_users"}},
		{"plain english only", nil},
		{"a Foo Bar duplicate Foo", []string{"Foo", "Bar"}},
		// camelCase — Go unexported names should be picked up.
		{"inlineContent", []string{"inlineContent"}},
		{"where is markDirty called", []string{"markDirty"}},
		// A camelCase token inside a qualified form must not double-add
		// (the qualified span masks sub-token matches).
		{"(*Store).searchRaw", []string{"(*Store).searchRaw"}},
		// Single-word lowercase fallback — `rerank` is a valid Go
		// identifier (unexported method on (*Store)) that matches none
		// of the regex passes. The fallback should pick it up.
		{"rerank", []string{"rerank"}},
		{"index", []string{"index"}},
		// Multi-word lowercase phrases should NOT trigger the fallback —
		// "plain english only" is correctly treated as natural language.
		{"plain english only", nil},
		// Too short (1-2 chars) doesn't qualify for the fallback.
		{"go", nil},
		// Punctuation/whitespace in single-word case → not an identifier.
		{"go!", nil},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := ExtractIdentifiers(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("idx %d: got %q, want %q (full: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}
