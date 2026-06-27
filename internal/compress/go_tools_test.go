package compress

import (
	"strings"
	"testing"
)

func TestCompressGoTest_PassLines(t *testing.T) {
	lines := []string{
		"=== RUN   TestFoo",
		"=== RUN   TestBar",
		"ok  \tgithub.com/example/pkg\t0.123s",
	}
	out := CompressGoTest(lines)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "=== RUN") {
		t.Error("RUN lines should be suppressed")
	}
	if !strings.Contains(joined, "ok  \tgithub.com/example/pkg") {
		t.Error("pass line should be retained")
	}
}

func TestCompressGoTest_FailureRetained(t *testing.T) {
	lines := []string{
		"=== RUN   TestBad",
		"--- FAIL: TestBad (0.00s)",
		"    foo_test.go:12: expected 1, got 2",
		"FAIL",
		"exit status 1",
	}
	out := CompressGoTest(lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "FAIL: TestBad") {
		t.Error("failure header should be retained")
	}
	if !strings.Contains(joined, "expected 1") {
		t.Error("failure diagnostic should be retained")
	}
	if !strings.Contains(joined, "exit status") {
		t.Error("exit status should be retained")
	}
}

func TestCompressGoTest_EmptyInput(t *testing.T) {
	out := CompressGoTest(nil)
	if out != nil {
		t.Errorf("nil input should return nil, got %v", out)
	}
}

func TestCompressGoTest_AllSuppressed_ReturnsOriginal(t *testing.T) {
	lines := []string{"=== RUN   TestX", "=== RUN   TestY"}
	out := CompressGoTest(lines)
	if len(out) != len(lines) {
		t.Errorf("when nothing survives, original should be returned; got %d lines", len(out))
	}
}

func TestIsIndentedLine(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"    indented", true},
		{"\tindented", true},
		{"not indented", false},
		{"", false},
	}
	for _, c := range cases {
		got := isIndentedLine(c.in)
		if got != c.want {
			t.Errorf("isIndentedLine(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCompressGoBuild_DropsProgressLines(t *testing.T) {
	lines := []string{
		"# github.com/example/pkg",
		"./foo.go:10:5: undefined: Bar",
	}
	out := CompressGoBuild(lines)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "# github.com") {
		t.Error("package progress line should be dropped")
	}
	if !strings.Contains(joined, "undefined: Bar") {
		t.Error("error line should be retained")
	}
}

func TestCompressGoBuild_EmptyAfterFilter_ReturnsOriginal(t *testing.T) {
	lines := []string{"# pkg/a", "# pkg/b"}
	out := CompressGoBuild(lines)
	if len(out) != len(lines) {
		t.Errorf("all-noise input should return original; got %d lines", len(out))
	}
}
