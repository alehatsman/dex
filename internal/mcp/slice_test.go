package mcp

import (
	"bytes"
	"testing"
)

func TestApplySliceEmpty(t *testing.T) {
	data := []byte("line1\nline2\nline3\n")
	out, hint, err := applySlice(data, "")
	if err != nil || hint != "" || !bytes.Equal(out, data) {
		t.Fatalf("empty spec: got (%q, %q, %v), want unchanged", out, hint, err)
	}
}

func TestApplySliceUnknown(t *testing.T) {
	_, _, err := applySlice([]byte("data"), "bogus:arg")
	if err == nil {
		t.Fatal("expected error for unknown spec")
	}
}

func TestApplySliceHead(t *testing.T) {
	data := []byte("a\nb\nc\nd\n")
	out, hint, err := applySlice(data, "head:2")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	want := "a\nb\n"
	if string(out) != want {
		t.Errorf("head: got %q, want %q", out, want)
	}
	if hint == "" {
		t.Error("head: expected non-empty hint")
	}
}

func TestApplySliceHeadBeyondEOF(t *testing.T) {
	data := []byte("a\nb\n")
	out, _, err := applySlice(data, "head:100")
	if err != nil {
		t.Fatalf("head beyond EOF: %v", err)
	}
	if string(out) != "a\nb\n" {
		t.Errorf("head beyond EOF: got %q, want %q", out, "a\nb\n")
	}
}

func TestApplySliceHeadZero(t *testing.T) {
	_, _, err := applySlice([]byte("a\n"), "head:0")
	if err == nil {
		t.Fatal("head:0 should error")
	}
}

func TestApplySliceTail(t *testing.T) {
	data := []byte("a\nb\nc\nd\n")
	out, hint, err := applySlice(data, "tail:2")
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	want := "c\nd\n"
	if string(out) != want {
		t.Errorf("tail: got %q, want %q", out, want)
	}
	if hint == "" {
		t.Error("tail: expected non-empty hint")
	}
}

func TestApplySliceTailBeyondEOF(t *testing.T) {
	data := []byte("a\nb\n")
	out, _, err := applySlice(data, "tail:100")
	if err != nil {
		t.Fatalf("tail beyond EOF: %v", err)
	}
	if string(out) != "a\nb\n" {
		t.Errorf("tail beyond EOF: got %q", out)
	}
}

func TestApplySliceRange(t *testing.T) {
	data := []byte("a\nb\nc\nd\ne\n")
	out, hint, err := applySlice(data, "range:2-4")
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	want := "b\nc\nd\n"
	if string(out) != want {
		t.Errorf("range: got %q, want %q", out, want)
	}
	if hint == "" {
		t.Error("range: expected non-empty hint")
	}
}

func TestApplySliceRangeBadFormat(t *testing.T) {
	_, _, err := applySlice([]byte("a\n"), "range:abc")
	if err == nil {
		t.Fatal("range: bad format should error")
	}
}

func TestApplySliceRangeEndLtStart(t *testing.T) {
	_, _, err := applySlice([]byte("a\nb\nc\n"), "range:3-1")
	if err == nil {
		t.Fatal("range: end < start should error")
	}
}

func TestApplySliceSearch(t *testing.T) {
	data := []byte("alpha\nbeta\ngamma\ndelta\nepsilon\n")
	out, hint, err := applySlice(data, "search:bet")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !bytes.Contains(out, []byte("beta")) {
		t.Errorf("search: expected 'beta' in output, got %q", out)
	}
	if hint == "" {
		t.Error("search: expected non-empty hint")
	}
}

func TestApplySliceSearchNoMatch(t *testing.T) {
	data := []byte("alpha\nbeta\n")
	out, hint, err := applySlice(data, "search:zzz")
	if err != nil {
		t.Fatalf("search no match: unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("search no match: expected empty output, got %q", out)
	}
	if hint == "" {
		t.Error("search no match: expected hint")
	}
}

func TestApplySliceSearchContext(t *testing.T) {
	// Context lines are emitted around matches.
	lines := "1\n2\n3\n4\nmatch\n6\n7\n8\n9\n"
	out, _, err := applySlice([]byte(lines), "search:match")
	if err != nil {
		t.Fatalf("search context: %v", err)
	}
	// Should contain context line "2" (3 before match at line 5).
	if !bytes.Contains(out, []byte("2")) {
		t.Errorf("search context: expected context lines in %q", out)
	}
}

func TestApplySliceSearchInvalidRegex(t *testing.T) {
	_, _, err := applySlice([]byte("data\n"), "search:[invalid")
	if err == nil {
		t.Fatal("search: invalid regex should error")
	}
}

func TestApplySliceSearchSeparator(t *testing.T) {
	// Two non-adjacent matches → separated by ---
	data := []byte("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nmatch1\nl\nm\nn\no\np\nq\nr\ns\nmatch2\nt\n")
	out, _, err := applySlice(data, "search:match")
	if err != nil {
		t.Fatalf("search separator: %v", err)
	}
	if !bytes.Contains(out, []byte("---")) {
		t.Errorf("search separator: expected --- between groups in %q", out)
	}
}

func TestApplySliceJSONPath(t *testing.T) {
	data := []byte(`{"a":{"b":42}}`)
	out, hint, err := applySlice(data, "json_path:$.a.b")
	if err != nil {
		t.Fatalf("json_path: %v", err)
	}
	if string(out) != "42" {
		t.Errorf("json_path: got %q, want %q", out, "42")
	}
	if hint == "" {
		t.Error("json_path: expected non-empty hint")
	}
}

func TestApplySliceJSONPathArray(t *testing.T) {
	data := []byte(`{"items":[10,20,30]}`)
	out, _, err := applySlice(data, "json_path:$.items[1]")
	if err != nil {
		t.Fatalf("json_path array: %v", err)
	}
	if string(out) != "20" {
		t.Errorf("json_path array: got %q, want %q", out, "20")
	}
}

func TestApplySliceJSONPathRoot(t *testing.T) {
	data := []byte(`{"x":1}`)
	out, _, err := applySlice(data, "json_path:$")
	if err != nil {
		t.Fatalf("json_path root: %v", err)
	}
	if !bytes.Contains(out, []byte(`"x"`)) {
		t.Errorf("json_path root: expected full doc in %q", out)
	}
}

func TestApplySliceJSONPathMissingKey(t *testing.T) {
	data := []byte(`{"a":1}`)
	_, _, err := applySlice(data, "json_path:$.b")
	if err == nil {
		t.Fatal("json_path missing key: expected error")
	}
}

func TestApplySliceJSONPathInvalidJSON(t *testing.T) {
	_, _, err := applySlice([]byte("not json"), "json_path:$.a")
	if err == nil {
		t.Fatal("json_path invalid JSON: expected error")
	}
}
