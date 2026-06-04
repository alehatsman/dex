package mcp

import (
	"strings"
	"testing"
)

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
