package compress

import (
	"strings"
	"testing"
)

func TestApplyTokenReductions_Global(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "space-padded arrow",
			in:   "func foo() error { return nil -> err }",
			want: "func foo() error { return nil->err }",
		},
		{
			name: "space-padded fat arrow",
			in:   "items.map(x => x.id)",
			want: "items.map(x=>x.id)",
		},
		{
			name: "triple blank lines collapsed",
			in:   "a\n\n\nb",
			want: "a\n\nb",
		},
		{
			name: "no change when already minimal",
			in:   "func foo() {}",
			want: "func foo() {}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyTokenReductions(tt.in, ".go")
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyTokenReductions_Rust(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "pub(crate)",
			in:   "pub(crate) fn new() -> Self {}",
			want: "pub fn new()->Self {}",
		},
		{
			name: "pub(super)",
			in:   "pub(super) struct Foo {}",
			want: "pub struct Foo {}",
		},
		{
			name: "std HashMap",
			in:   "use std::collections::HashMap;",
			want: "use HashMap;",
		},
		{
			name: "std HashSet",
			in:   "let s: std::collections::HashSet<u32>;",
			want: "let s: HashSet<u32>;",
		},
		{
			name: "Arc and Mutex",
			in:   "std::sync::Arc<std::sync::Mutex<i32>>",
			want: "Arc<Mutex<i32>>",
		},
		{
			name: "PathBuf",
			in:   "let p: std::path::PathBuf = std::path::PathBuf::new();",
			want: "let p: PathBuf = PathBuf::new();",
		},
		{
			name: "io Result",
			in:   "fn read() -> std::io::Result<Vec<u8>> {}",
			want: "fn read()->io::Result<Vec<u8>> {}",
		},
		{
			name: "not applied to non-rust ext",
			in:   "pub(crate) fn foo() {}",
			want: "pub(crate) fn foo() {}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ext := ".rs"
			if tt.name == "not applied to non-rust ext" {
				ext = ".go"
			}
			got := ApplyTokenReductions(tt.in, ext)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyTokenReductions_JSTS(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ext  string
		want string
	}{
		{
			name: "function keyword",
			in:   "export function greet(name: string) {}",
			ext:  ".ts",
			want: "export fn greet(name: string) {}",
		},
		{
			name: "boolean type",
			in:   "let x: boolean = true;",
			ext:  ".ts",
			want: "let x: bool = true;",
		},
		{
			name: "export default",
			in:   "export default class Foo {}",
			ext:  ".js",
			want: "export class Foo {}",
		},
		{
			name: "tsx extension",
			in:   "function App(): JSX.Element {}",
			ext:  ".tsx",
			want: "fn App(): JSX.Element {}",
		},
		{
			name: "jsx extension",
			in:   "function render() {}",
			ext:  ".jsx",
			want: "fn render() {}",
		},
		{
			name: "not applied to go files",
			in:   "function foo() {}",
			ext:  ".go",
			want: "function foo() {}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplyTokenReductions(tt.in, tt.ext)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyTokenReductions_NoIncrease(t *testing.T) {
	inputs := []struct {
		content string
		ext     string
	}{
		{"func main() {}", ".go"},
		{"pub fn main() {}", ".rs"},
		{"const x = 1;", ".ts"},
		{"", ".go"},
	}
	for _, tc := range inputs {
		got := ApplyTokenReductions(tc.content, tc.ext)
		if len(got) > len(tc.content) {
			t.Errorf("ext=%s: output longer than input: %q -> %q", tc.ext, tc.content, got)
		}
	}
}

func TestAggressiveCompress_AppliesTokenReductions(t *testing.T) {
	// Rust file with pub(crate) and std qualified paths should have them reduced.
	input := strings.Repeat("pub(crate) fn helper() -> std::io::Result<()> {\n", 30)
	result := AggressiveCompress(input, ".rs")
	if strings.Contains(result, "pub(crate)") {
		t.Error("expected pub(crate) to be reduced in aggressive mode")
	}
	if strings.Contains(result, "std::io::Result") {
		t.Error("expected std::io::Result to be reduced in aggressive mode")
	}
}
