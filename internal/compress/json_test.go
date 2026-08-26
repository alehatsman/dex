package compress

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCompactJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{
			name: "pretty object",
			in:   "{\n  \"a\": 1,\n  \"b\": [\n    2,\n    3\n  ]\n}\n",
			want: `{"a":1,"b":[2,3]}`,
			ok:   true,
		},
		{
			name: "string-internal whitespace preserved",
			in:   `{ "msg": "hello   world\ttab" }`,
			want: `{"msg":"hello   world\ttab"}`,
			ok:   true,
		},
		{
			name: "escaped quote inside string",
			in:   `{ "q": "she said \"hi\" now" }`,
			want: `{"q":"she said \"hi\" now"}`,
			ok:   true,
		},
		{
			name: "escaped backslash before quote",
			in:   `{ "path": "a\\" , "b": 2 }`,
			want: `{"path":"a\\","b":2}`,
			ok:   true,
		},
		{
			name: "newline inside string literal kept",
			in:   `{ "s": "line1\nline2" }`,
			want: `{"s":"line1\nline2"}`,
			ok:   true,
		},
		{
			name: "concatenated stream",
			in:   "{\n  \"a\": 1\n}\n{\n  \"b\": 2\n}\n",
			want: `{"a":1}{"b":2}`,
			ok:   true,
		},
		{
			name: "already compact returns false",
			in:   `{"a":1,"b":2}`,
			want: `{"a":1,"b":2}`,
			ok:   false,
		},
		{
			name: "empty",
			in:   ``,
			want: ``,
			ok:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := CompactJSON(c.in)
			if got != c.want || ok != c.ok {
				t.Errorf("CompactJSON(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestCompactJSONLossless verifies the compacted output parses to a value
// deeply equal to the original — the core lossless guarantee.
func TestCompactJSONLossless(t *testing.T) {
	inputs := []string{
		"{\n  \"name\": \"dex\",\n  \"nums\": [1, 2, 3],\n  \"nested\": {\"x\": true, \"y\": null},\n  \"unicode\": \"héllo 世界 \\u00e9\"\n}\n",
		`[ {"k": "v with  spaces"}, { "n": -1.5e10 }, "  padded string  " ]`,
		`{ "empty_obj": {}, "empty_arr": [], "esc": "tab\there, nl\nhere" }`,
	}
	for _, in := range inputs {
		compact, _ := CompactJSON(in)
		var a, b any
		if err := json.Unmarshal([]byte(in), &a); err != nil {
			t.Fatalf("input not valid JSON: %v", err)
		}
		if err := json.Unmarshal([]byte(compact), &b); err != nil {
			t.Fatalf("compact not valid JSON: %v\ncompact=%q", err, compact)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("lossy compaction:\n in=%q\nout=%q", in, compact)
		}
	}
}

func TestCompactJSONL(t *testing.T) {
	// Pretty records, one per line, must keep their newline separators.
	in := `{ "a": 1 }
{ "b": 2 }
{ "c": 3 }`
	want := "{\"a\":1}\n{\"b\":2}\n{\"c\":3}"
	got, ok := CompactJSONL(in)
	if !ok || got != want {
		t.Errorf("CompactJSONL = (%q, %v), want (%q, true)", got, ok, want)
	}

	// Blank lines preserved.
	in2 := "{ \"a\": 1 }\n\n{ \"b\": 2 }\n"
	want2 := "{\"a\":1}\n\n{\"b\":2}\n"
	got2, ok2 := CompactJSONL(in2)
	if !ok2 || got2 != want2 {
		t.Errorf("CompactJSONL(blanks) = (%q, %v), want (%q, true)", got2, ok2, want2)
	}

	// Already-compact JSONL yields no change.
	in3 := "{\"a\":1}\n{\"b\":2}"
	if got3, ok3 := CompactJSONL(in3); ok3 || got3 != in3 {
		t.Errorf("CompactJSONL(compact) = (%q, %v), want (%q, false)", got3, ok3, in3)
	}
}

// TestCompactJSONLScalarSafety guards the reason JSONL keeps newlines: bare
// scalars on separate lines must not merge into one ambiguous token.
func TestCompactJSONLScalarSafety(t *testing.T) {
	in := "1\n2\n3"
	got, _ := CompactJSONL(in)
	if got != "1\n2\n3" {
		t.Errorf("CompactJSONL merged scalars: got %q", got)
	}
}

// TestCompactJSONLeavesNonJSONUntouched covers #669: output that merely starts
// with '[' or '{' but isn't valid JSON (log lines, etc.) must pass through
// untouched — stripping its whitespace as if it were JSON structure corrupts it.
func TestCompactJSONLeavesNonJSONUntouched(t *testing.T) {
	nonJSON := []string{
		"[INFO] server started on port 8080",
		"[WARN] disk 80% full, cleaning up now",
		"{ pid 1234 running } not json here",
		`{"a":1} and then some trailing prose`,
	}
	for _, in := range nonJSON {
		if got, ok := CompactJSON(in); ok || got != in {
			t.Errorf("CompactJSON(%q) = (%q, %v); want it left untouched (input, false)", in, got, ok)
		}
	}
	// Sanity: genuine JSON still compacts.
	if got, ok := CompactJSON("{ \"a\": 1 }"); !ok || got != `{"a":1}` {
		t.Errorf("valid JSON should still compact, got (%q, %v)", got, ok)
	}
}
