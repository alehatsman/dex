package chunk

import (
	"context"
	"testing"
)

// Cases with no Path exercise the textual fallback (textExtractive): no
// extension means no grammar, so treeExtractive bails.
func TestExtractiveSummaryTextualFallback(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "go func with doc comment",
			content: `// Greet prints a greeting to the named user.
// It returns an error if the name is empty.
func Greet(name string) error {
	if name == "" {
		return errors.New("empty")
	}
	return nil
}`,
			want: `// Greet prints a greeting to the named user.
// It returns an error if the name is empty.
func Greet(name string) error {
	if name == "" {`,
		},
		{
			name: "wrapped multi-line signature",
			content: `func Combine(
	a int,
	b int,
) (int, error) {
	return a + b, nil
}`,
			want: `func Combine(
	a int,
	b int,
) (int, error) {
	return a + b, nil`,
		},
		{
			name: "allman brace skipped as first body line",
			content: `void run()
{
	doWork();
}`,
			want: `void run()
	doWork();`,
		},
		{
			name: "decorator absorbed into signature",
			content: `@app.route("/x")
def view():
    return render()`,
			want: `@app.route("/x")
def view():
    return render()`,
		},
		{
			name: "ruby method without brace opener",
			content: `def total(items)
  items.sum
end`,
			want: `def total(items)
  items.sum`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractiveSummary(context.Background(), Chunk{Content: tt.content})
			if got != tt.want {
				t.Errorf("mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tt.want)
			}
		})
	}
}

// Cases with a Path exercise the tree-sitter path (treeExtractive): the
// signature boundary is the declaration's `body` field, so the opening
// brace lands on the first-body-line, not the signature.
func TestExtractiveSummaryTreeSitter(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		content string
		want    string
	}{
		{
			name: "go func with doc comment",
			path: "x.go",
			content: `// Greet prints a greeting to the named user.
// It returns an error if the name is empty.
func Greet(name string) error {
	if name == "" {
		return errors.New("empty")
	}
	return nil
}`,
			want: `// Greet prints a greeting to the named user.
// It returns an error if the name is empty.
func Greet(name string) error
if name == "" {`,
		},
		{
			name: "go func no doc",
			path: "x.go",
			content: `func add(a, b int) int {
	return a + b
}`,
			want: `func add(a, b int) int
return a + b`,
		},
		{
			name: "string literal with brace does not derail signature",
			path: "x.go",
			content: `func tag() string {
	return "{unclosed"
}`,
			want: `func tag() string
return "{unclosed"`,
		},
		{
			name: "python def with docstring",
			path: "x.py",
			content: `def handle(req):
    """Handle one request."""
    return process(req)`,
			want: `def handle(req):
"""Handle one request."""`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractiveSummary(context.Background(), Chunk{Path: tt.path, Content: tt.content})
			if got != tt.want {
				t.Errorf("mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tt.want)
			}
		})
	}
}

// A summary must never be longer than the source it distills.
func TestExtractiveSummaryNeverExceedsContent(t *testing.T) {
	content := `// doc
func F() {
	a()
	b()
	c()
}`
	got := ExtractiveSummary(context.Background(), Chunk{Path: "x.go", Content: content})
	if len(got) > len(content) {
		t.Fatalf("summary longer than content: %d > %d", len(got), len(content))
	}
}
