package compress

import (
	"strings"
	"testing"
)

func TestSkeletonPass_ExportedFunc(t *testing.T) {
	src := []byte(`package foo

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a + b
}
`)
	scopes := []BodyScope{
		{Name: "Add", Kind: "function", Exported: true, StartLine: 4, EndLine: 6},
	}
	res := SkeletonPass(src, "foo.go", scopes)

	if !strings.Contains(res.Text, "@B1") {
		t.Errorf("expected @B1 handle; got:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "return a + b") {
		t.Error("body should be suppressed")
	}
	if len(res.Bodies) != 1 {
		t.Fatalf("want 1 body entry, got %d", len(res.Bodies))
	}
	if res.Bodies[0].StartLine != 4 || res.Bodies[0].EndLine != 6 {
		t.Errorf("body entry lines: got %d-%d, want 4-6", res.Bodies[0].StartLine, res.Bodies[0].EndLine)
	}
}

func TestSkeletonPass_ExportedStruct(t *testing.T) {
	src := []byte(`package foo

type Config struct {
	Host string
	Port int
}
`)
	scopes := []BodyScope{
		{Name: "Config", Kind: "struct", Exported: true, StartLine: 3, EndLine: 6},
	}
	res := SkeletonPass(src, "foo.go", scopes)

	if !strings.Contains(res.Text, "type Config struct") {
		t.Errorf("struct should be shown in full; got:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "@B") {
		t.Error("struct should not emit a body handle")
	}
	if len(res.Bodies) != 0 {
		t.Errorf("want 0 body entries for struct, got %d", len(res.Bodies))
	}
}

func TestSkeletonPass_UnexportedFunc(t *testing.T) {
	src := []byte(`package foo

func helper() string {
	return "x"
}
`)
	scopes := []BodyScope{
		{Name: "helper", Kind: "function", Exported: false, StartLine: 3, EndLine: 5},
	}
	res := SkeletonPass(src, "foo.go", scopes)

	if strings.Contains(res.Text, "func helper") {
		t.Error("unexported function should not appear")
	}
	if !strings.Contains(res.Text, "1 unexported function(s) omitted") {
		t.Errorf("expected unexported summary; got:\n%s", res.Text)
	}
	if len(res.Bodies) != 0 {
		t.Errorf("want 0 body entries for unexported func, got %d", len(res.Bodies))
	}
}

func TestSkeletonPass_MultiLineSig(t *testing.T) {
	src := []byte(`package foo

func LongFunc(
	a int,
	b string,
) error {
	return nil
}
`)
	scopes := []BodyScope{
		{Name: "LongFunc", Kind: "function", Exported: true, StartLine: 3, EndLine: 9},
	}
	res := SkeletonPass(src, "foo.go", scopes)

	if !strings.Contains(res.Text, "func LongFunc(") {
		t.Errorf("expected signature start; got:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "@B1") {
		t.Errorf("expected handle @B1; got:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "return nil") {
		t.Error("body should be suppressed")
	}
	if len(res.Bodies) != 1 {
		t.Fatalf("want 1 body entry, got %d", len(res.Bodies))
	}
}

func TestSkeletonFindOpenBrace(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  int // 0-based index
	}{
		{
			name:  "brace on same line",
			lines: []string{`func Foo() {`, `  return`, `}`},
			want:  0,
		},
		{
			name:  "brace on separate line",
			lines: []string{`func Bar(`, `  a int,`, `) error {`, `  return nil`, `}`},
			want:  2,
		},
		{
			name:  "skip string literal brace",
			lines: []string{`func Foo(s string) {`, `  _ = "{"`, `}`},
			want:  0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := make([][]byte, len(c.lines))
			for i, l := range c.lines {
				lines[i] = []byte(l)
			}
			got := skeletonFindOpenBrace(lines, 0, len(lines)-1)
			if got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}
