package mcp

import (
	"strings"
	"testing"
)

// ── compression pattern tests ─────────────────────────────────────────────────

func TestCompressGrep(t *testing.T) {
	var lines []string
	for i := 1; i <= 15; i++ {
		lines = append(lines, "src/foo.go:"+string(rune('0'+i%10))+": match content "+string(rune('0'+i%10)))
	}
	for i := 1; i <= 5; i++ {
		lines = append(lines, "src/bar.go:"+string(rune('0'+i%10))+": other match")
	}
	out := compressGrep(lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "20 matches in 2F:") {
		t.Fatalf("expected header '20 matches in 2F:', got:\n%s", joined)
	}
	if !strings.Contains(joined, "src/foo.go (15):") {
		t.Fatalf("expected foo.go grouped, got:\n%s", joined)
	}
}

func TestCompressGrepNoMatches(t *testing.T) {
	lines := []string{"no file path here", "just text", "more text"}
	out := compressGrep(lines)
	// should return original when nothing parseable
	if strings.Join(out, "\n") != strings.Join(lines, "\n") {
		t.Fatal("expected passthrough for non-grep output")
	}
}

func TestCompressFind(t *testing.T) {
	lines := []string{
		"./src/main.go", "./src/util.go", "./src/helper.go",
		"./cmd/main.go", "./cmd/serve.go",
		"./internal/store/store.go",
	}
	out := compressFind(lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "6F") {
		t.Fatalf("expected '6F' in header, got:\n%s", joined)
	}
	if !strings.Contains(joined, "src/") {
		t.Fatalf("expected src/ dir, got:\n%s", joined)
	}
}

func TestCompressEslint(t *testing.T) {
	lines := []string{
		"/project/src/app.ts",
		"   1:10  error  'foo' is not defined  no-undef",
		"   2:5   error  Missing semicolon      semi",
		"   3:1   warning  Unexpected var       no-var",
		"/project/src/util.ts",
		"   5:3   error  'bar' is not defined  no-undef",
		"",
		"✖ 4 problems (3 errors, 1 warning)",
	}
	out := compressEslint(lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "3 errors") {
		t.Fatalf("expected error count, got:\n%s", joined)
	}
	if !strings.Contains(joined, "no-undef") {
		t.Fatalf("expected rule name, got:\n%s", joined)
	}
}

func TestCompressRuff(t *testing.T) {
	var lines []string
	for i := 1; i <= 40; i++ {
		lines = append(lines, "src/foo.py:"+string(rune('0'+i%10))+":1: E501 line too long")
	}
	for i := 1; i <= 10; i++ {
		lines = append(lines, "src/bar.py:"+string(rune('0'+i%10))+":1: F401 unused import")
	}
	out := compressRuff("ruff check src/", lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "50 issues") {
		t.Fatalf("expected issue count, got:\n%s", joined)
	}
	if !strings.Contains(joined, "E501") {
		t.Fatalf("expected rule in output, got:\n%s", joined)
	}
}

func TestCompressRuffClean(t *testing.T) {
	lines := []string{"All checks passed!"}
	out := compressRuff("ruff check", lines)
	if strings.Join(out, "\n") != "clean" {
		t.Fatalf("expected 'clean', got: %q", strings.Join(out, "\n"))
	}
}

func TestCompressMypy(t *testing.T) {
	lines := []string{
		"src/app.py:10: error: Incompatible return value type  [return-value]",
		"src/util.py:5: error: Argument 1 to 'foo' has incompatible type  [arg-type]",
		"src/util.py:8: error: Cannot access member 'x'  [attr-defined]",
		"Found 3 errors in 2 files (checked 5 source files)",
	}
	out := compressMypy(lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "3 errors in 2 files") {
		t.Fatalf("expected summary, got:\n%s", joined)
	}
	if !strings.Contains(joined, "return-value") {
		t.Fatalf("expected error code, got:\n%s", joined)
	}
}

func TestCompressPytest(t *testing.T) {
	lines := []string{
		"collected 10 items",
		"",
		"PASSED tests/test_foo.py::test_one",
		"PASSED tests/test_foo.py::test_two",
		"FAILED tests/test_bar.py::test_bad - AssertionError",
		"FAILED tests/test_bar.py::test_worse",
		"",
		"short test summary info",
		"FAILED tests/test_bar.py::test_bad",
		"FAILED tests/test_bar.py::test_worse",
		"====== 2 failed, 2 passed in 0.42s ======",
	}
	out := compressPytest(lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "2 failed") {
		t.Fatalf("expected summary line, got:\n%s", joined)
	}
	if !strings.Contains(joined, "test_bad") {
		t.Fatalf("expected failed test name, got:\n%s", joined)
	}
}

func TestCompressTsc(t *testing.T) {
	lines := []string{
		"src/app.ts(10,5): error TS2322: Type 'string' is not assignable to type 'number'.",
		"src/util.ts(20,3): error TS2339: Property 'foo' does not exist on type 'Bar'.",
		"Found 2 errors.",
	}
	out := compressTsc(lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "2 errors in 2 files") {
		t.Fatalf("expected error summary, got:\n%s", joined)
	}
	if !strings.Contains(joined, "TS2322") {
		t.Fatalf("expected TS error code, got:\n%s", joined)
	}
}

func TestCompressLogDedup(t *testing.T) {
	lines := []string{
		"2024-01-01T10:00:00Z INFO starting server",
		"2024-01-01T10:00:01Z INFO starting server",
		"2024-01-01T10:00:02Z INFO starting server",
		"2024-01-01T10:00:03Z INFO request received",
		"2024-01-01T10:00:04Z INFO request received",
		"2024-01-01T10:00:05Z ERROR connection failed",
		"2024-01-01T10:00:06Z INFO starting server",
		"2024-01-01T10:00:07Z INFO starting server",
		"2024-01-01T10:00:08Z INFO done",
		"2024-01-01T10:00:09Z INFO done",
		"2024-01-01T10:00:10Z INFO done",
	}
	out := compressLogDedup(lines)
	if out == nil {
		t.Fatal("expected dedup to apply")
	}
	if len(out) >= len(lines) {
		t.Fatalf("expected fewer lines after dedup: %d >= %d", len(out), len(lines))
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "x3") && !strings.Contains(joined, "x4") {
		t.Fatalf("expected repeat count annotation, got:\n%s", joined)
	}
}

func TestHasFileWriteRedirect(t *testing.T) {
	blocked := []string{
		"echo hello > output.txt",
		"printf 'data' >> log.txt",
		"echo x > file",
	}
	allowed := []string{
		"git status",
		"go test ./...",
		"echo hello",
		"cat file.txt",
		"ls > /dev/null",
		"cmd 2>/dev/null",
		`grep ">" file.txt`,
		"curl http://x.com > /dev/null",
	}
	for _, cmd := range blocked {
		if !hasFileWriteRedirect(cmd) {
			t.Errorf("expected blocked: %q", cmd)
		}
	}
	for _, cmd := range allowed {
		if hasFileWriteRedirect(cmd) {
			t.Errorf("expected allowed: %q", cmd)
		}
	}
}

func TestShellValidate(t *testing.T) {
	if err := shellValidate("git status"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := shellValidate("echo x > file.txt"); err == nil {
		t.Fatal("expected error for redirect")
	}
	if err := shellValidate("cmd | tee output.log"); err == nil {
		t.Fatal("expected error for tee")
	}
}

func TestStripANSI(t *testing.T) {
	input := "\x1b[32mok\x1b[0m \x1b[1mBold\x1b[0m"
	got := stripANSI(input)
	if strings.Contains(got, "\x1b") {
		t.Fatalf("ANSI not stripped: %q", got)
	}
	if !strings.Contains(got, "ok") || !strings.Contains(got, "Bold") {
		t.Fatalf("content lost: %q", got)
	}
}

func TestShellRun_Basic(t *testing.T) {
	s := &Server{}
	out, err := s.ShellRun(t.Context(), ShellInput{Command: "echo hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit code %d", out.ExitCode)
	}
	if !strings.Contains(out.Output, "hello") {
		t.Fatalf("output missing 'hello': %q", out.Output)
	}
}

func TestShellRun_NonZeroExit(t *testing.T) {
	s := &Server{}
	out, err := s.ShellRun(t.Context(), ShellInput{Command: "exit 42"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ExitCode != 42 {
		t.Fatalf("expected exit 42, got %d", out.ExitCode)
	}
}

func TestShellRun_Raw(t *testing.T) {
	s := &Server{}
	// Raw=true: compression fields should be zero
	out, err := s.ShellRun(t.Context(), ShellInput{Command: "echo raw", Raw: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.OriginalLines != 0 || out.OutputLines != 0 {
		t.Fatalf("expected no compression stats in raw mode, got orig=%d out=%d", out.OriginalLines, out.OutputLines)
	}
	if !strings.Contains(out.Output, "raw") {
		t.Fatalf("content missing: %q", out.Output)
	}
}

func TestShellRun_BlocksRedirect(t *testing.T) {
	s := &Server{}
	_, err := s.ShellRun(t.Context(), ShellInput{Command: "echo x > /tmp/dex_test_out.txt"})
	if err == nil {
		t.Fatal("expected error for file redirect")
	}
}

func TestCompressGoTest(t *testing.T) {
	fakeOutput := strings.Join([]string{
		"=== RUN   TestFoo",
		"=== RUN   TestBar",
		"--- PASS: TestFoo (0.00s)",
		"--- PASS: TestBar (0.00s)",
		"ok  \tgithub.com/example/pkg\t0.005s",
	}, "\n")
	compressed, origLines, _ := CompressText(fakeOutput, "go test ./...", 0)
	if origLines == 0 {
		t.Fatal("origLines should be > 0")
	}
	if strings.Contains(compressed, "=== RUN") {
		t.Fatal("=== RUN lines should be stripped")
	}
	if !strings.Contains(compressed, "ok") {
		t.Fatal("ok line should be preserved")
	}
}
