package retrieve

import (
	"reflect"
	"testing"
)

func TestParseExpansion(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want QueryExpansion
	}{
		{
			name: "clean json",
			raw:  `{"keywords":["auth","login"],"identifiers":["checkToken"],"hyde":"Validates a bearer token."}`,
			want: QueryExpansion{Keywords: []string{"auth", "login"}, Identifiers: []string{"checkToken"}, Hyde: "Validates a bearer token."},
		},
		{
			name: "wrapped in think block",
			raw:  "<think>the user wants auth code\nlet me list terms</think>\n{\"keywords\":[\"auth\"],\"identifiers\":[],\"hyde\":\"\"}",
			want: QueryExpansion{Keywords: []string{"auth"}},
		},
		{
			name: "fenced markdown and prose",
			raw:  "Sure! Here you go:\n```json\n{\"keywords\":[\"retry\",\"backoff\"],\"identifiers\":[\"withRetry\"]}\n```",
			want: QueryExpansion{Keywords: []string{"retry", "backoff"}, Identifiers: []string{"withRetry"}},
		},
		{
			name: "unclosed think block, no json",
			raw:  "<think>still thinking, never answered",
			want: QueryExpansion{},
		},
		{
			name: "dedup case-insensitive and drop empties",
			raw:  `{"keywords":["Auth","auth","  ","login"],"identifiers":["X","x"]}`,
			want: QueryExpansion{Keywords: []string{"Auth", "login"}, Identifiers: []string{"X"}},
		},
		{
			name: "garbage",
			raw:  "I cannot help with that.",
			want: QueryExpansion{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseExpansion(tt.raw)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseExpansion(%q)\n got = %+v\nwant = %+v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseExpansionCaps(t *testing.T) {
	// 10 keywords -> capped at 8, 8 identifiers -> capped at 6.
	raw := `{"keywords":["a","b","c","d","e","f","g","h","i","j"],` +
		`"identifiers":["A","B","C","D","E","F","G","H"]}`
	got := parseExpansion(raw)
	if len(got.Keywords) != 8 {
		t.Errorf("keywords len = %d, want 8", len(got.Keywords))
	}
	if len(got.Identifiers) != 6 {
		t.Errorf("identifiers len = %d, want 6", len(got.Identifiers))
	}
}

func TestResolveExpandMode(t *testing.T) {
	tests := []struct {
		req, def string
		want     ExpandMode
	}{
		{"", "", ExpandOff},
		{"", "on", ExpandOn},
		{"", "full", ExpandFull},
		{"on", "off", ExpandOn},
		{"off", "on", ExpandOff}, // request overrides server default
		{"FULL", "", ExpandFull},
		{"bogus", "on", ExpandOff}, // unrecognised → off, never silent GPU
		{"  on  ", "", ExpandOn},
	}
	for _, tt := range tests {
		if got := ResolveExpandMode(tt.req, tt.def); got != tt.want {
			t.Errorf("ResolveExpandMode(%q,%q) = %q, want %q", tt.req, tt.def, got, tt.want)
		}
	}
}

func TestExpandedText(t *testing.T) {
	q := "how does auth work"
	exp := QueryExpansion{Keywords: []string{"login", "token"}, Identifiers: []string{"checkAuth"}, Hyde: "It checks a token."}

	// FTS folds keywords+identifiers after the raw question.
	if got, want := ExpandedFTSText(q, exp), "how does auth work login token checkAuth"; got != want {
		t.Errorf("ExpandedFTSText = %q, want %q", got, want)
	}
	// No expansion → raw question untouched.
	if got := ExpandedFTSText(q, QueryExpansion{}); got != q {
		t.Errorf("ExpandedFTSText(empty) = %q, want %q", got, q)
	}
	// Embed text appends HyDE.
	if got, want := ExpandedEmbedText(q, exp), "how does auth work\n\nIt checks a token."; got != want {
		t.Errorf("ExpandedEmbedText = %q, want %q", got, want)
	}
	// No HyDE → raw question untouched (no extra embed drift).
	if got := ExpandedEmbedText(q, QueryExpansion{Keywords: []string{"x"}}); got != q {
		t.Errorf("ExpandedEmbedText(no hyde) = %q, want %q", got, q)
	}
}

func TestAppendExpansionIdentifiers(t *testing.T) {
	got := AppendExpansionIdentifiers([]string{"Foo", "Bar"}, []string{"bar", "Baz", "  ", "baz"})
	want := []string{"Foo", "Bar", "Baz"} // bar dup-skipped, blanks dropped, Baz once
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
