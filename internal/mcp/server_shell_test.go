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
	// verbatimCompact deduplicates consecutive runs: [3x], [2x], [2x], [3x].
	if !strings.Contains(joined, "[3x]") && !strings.Contains(joined, "[2x]") {
		t.Fatalf("expected [Nx] run annotation, got:\n%s", joined)
	}
	// Timestamps replaced with [TS].
	if strings.Contains(joined, "2024-01-01") {
		t.Fatalf("expected timestamps replaced with [TS], got:\n%s", joined)
	}
}

func TestCompressLogDedupUUID(t *testing.T) {
	// Lines that differ only in UUID and log-level should dedup.
	lines := []string{
		"INFO  req 550e8400-e29b-41d4-a716-446655440000 started",
		"INFO  req 6ba7b810-9dad-11d1-80b4-00c04fd430c8 started",
		"INFO  req 7c9e6679-7425-40de-944b-e07fc1f90ae7 started",
		"INFO  req a8098c1a-f86e-11da-bd1a-00112444be1e started",
		"WARN  req 550e8400-e29b-41d4-a716-111111110000 started",
		"DEBUG req 550e8400-e29b-41d4-a716-222222220000 started",
		"INFO  req 550e8400-e29b-41d4-a716-333333330000 started",
		"INFO  req 550e8400-e29b-41d4-a716-444444440000 started",
		"INFO  req 550e8400-e29b-41d4-a716-555555550000 started",
		"INFO  req 550e8400-e29b-41d4-a716-666666660000 started",
		"INFO  req 550e8400-e29b-41d4-a716-777777770000 started",
	}
	out := compressLogDedup(lines)
	if out == nil {
		t.Fatal("expected dedup to apply")
	}
	if len(out) >= len(lines) {
		t.Fatalf("expected fewer lines after UUID+loglevel dedup: got %d, want < %d\n%s",
			len(out), len(lines), strings.Join(out, "\n"))
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "x]") {
		t.Fatalf("expected run annotation, got:\n%s", joined)
	}
}

func TestNormalizeLineForDedup(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			// timestamp → [TS], log level stripped
			"2024-01-01T10:00:00Z INFO request started",
			"[TS] request started",
		},
		{
			// UUID → [UUID], log level stripped
			"INFO  req 550e8400-e29b-41d4-a716-446655440000 started",
			"req [UUID] started",
		},
		{
			// bare hex hash → [HASH], log level stripped
			"ERROR connection failed: abc123def456abc123def456abc123def456abc123def456abc123def456abc1",
			"connection failed: [HASH]",
		},
	}
	for _, c := range cases {
		got := normalizeLineForDedup(c.in)
		if got != c.want {
			t.Errorf("normalizeLineForDedup(%q)\n got:  %q\n want: %q", c.in, got, c.want)
		}
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
	if err := shellValidate("cat <<EOF > file.py\nprint('x')\nEOF"); err == nil {
		t.Fatal("expected error for heredoc file write")
	}
	if err := shellValidate("cat <<EOF\nhello\nEOF"); err != nil {
		t.Fatalf("heredoc without redirect should be allowed: %v", err)
	}
}

func TestClassifyCommand(t *testing.T) {
	passthrough := []string{
		"az login --use-device-code",
		"gh auth login",
		"npm run dev",
		"cargo run",
		"docker compose up",
		"kubectl logs -f pod/foo",
		"flask run",
		"psql",           // bare REPL → passthrough
		"mysql",          // bare REPL → passthrough
		"redis-cli",      // bare REPL → passthrough
	}
	verbatim := []string{
		"curl https://api.example.com/v1/users",
		"jq . file.json",
		"cat src/main.go",
		"terraform show",
		"kubectl get pods -o json",
		"docker inspect container123",
		"git log --oneline -10",
		// test runners — full output required (#82)
		"go test ./...",
		"cargo test --workspace",
		"pytest tests/",
		"RUST_BACKTRACE=1 cargo test",
		"jest --coverage",
		// git write commands — confirmation must be verbatim (#81/#123)
		"git push origin main",
		"git pull --rebase",
		"git merge feature-branch",
		"git commit -m 'fix'",
		// one-shot DB queries (not the REPL)
		"psql -c \"SELECT 1\"",
		"mysql -e 'SHOW TABLES'",
		// cloud + API queries
		"aws s3 ls",
		"gh api repos/owner/repo",
		"docker ps",
	}
	compress := []string{
		"cargo build",
		"npm install",
		"make build",
		"ruff check src/",
		"grep -r pattern src/",
		"golangci-lint run ./...",
	}
	for _, cmd := range passthrough {
		if got := classifyCommand(cmd); got != policyPassthrough {
			t.Errorf("expected passthrough for %q, got %v", cmd, got)
		}
	}
	for _, cmd := range verbatim {
		if got := classifyCommand(cmd); got != policyVerbatim {
			t.Errorf("expected verbatim for %q, got %v", cmd, got)
		}
	}
	for _, cmd := range compress {
		if got := classifyCommand(cmd); got != policyCompress {
			t.Errorf("expected compress for %q, got %v", cmd, got)
		}
	}
}

func TestContainsAuthFlow(t *testing.T) {
	yes := []string{
		"To sign in, use a web browser to open https://microsoft.com/devicelogin and enter the code ABCD1234",
		`{"device_code":"abc","user_code":"ABCD-1234","verification_uri":"https://example.com"}`,
		"! First copy your one-time code: ABCD-1234\nPress Enter to open github.com",
		"Go to https://accounts.google.com/auth\nEnter verification code: ",
	}
	no := []string{
		"Compiling lean-ctx v2.0\nFinished release",
		"On branch main\nnothing to commit",
		"added 150 packages in 3s",
	}
	for _, s := range yes {
		if !containsAuthFlow(s) {
			t.Errorf("expected auth flow detected in: %q", s)
		}
	}
	for _, s := range no {
		if containsAuthFlow(s) {
			t.Errorf("expected no auth flow in: %q", s)
		}
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

// ── playwright ────────────────────────────────────────────────────────────────

func TestCompressPlaywright_Summary(t *testing.T) {
	lines := []string{
		"Running 42 tests using 4 workers",
		"",
		"  1) example › login page › should redirect unauthenticated user",
		"  2) example › login page › should show error on bad password",
		"",
		"  42 passed (30s)",
		"  2 failed",
		"  1 skipped",
		"  Finished in 30123ms",
	}
	out := compressPlaywright("playwright test", lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "42") || !strings.Contains(joined, "passed") {
		t.Fatalf("expected pass count in output:\n%s", joined)
	}
	if !strings.Contains(joined, "failed:") {
		t.Fatalf("expected failed section:\n%s", joined)
	}
	if !strings.Contains(joined, "should redirect") {
		t.Fatalf("expected failed test name:\n%s", joined)
	}
}

func TestCompressPlaywright_AllPassed(t *testing.T) {
	lines := []string{
		"Running 10 tests using 2 workers",
		"  10 passed (5s)",
		"  0 failed",
		"  Finished in 5000ms",
	}
	out := compressPlaywright("playwright test", lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "10") || !strings.Contains(joined, "passed") {
		t.Fatalf("expected pass summary:\n%s", joined)
	}
	if strings.Contains(joined, "failed:") {
		t.Fatalf("should not have failed section when 0 failures:\n%s", joined)
	}
}

func TestCompressCypress(t *testing.T) {
	lines := []string{
		"  3 passing (2s)",
		"  1 failing",
		"  2 pending",
	}
	out := compressPlaywright("cypress run", lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "passing") && !strings.Contains(joined, "passed") {
		t.Fatalf("expected pass count:\n%s", joined)
	}
}

// ── next build ────────────────────────────────────────────────────────────────

func TestCompressNextBuild_Routes(t *testing.T) {
	lines := []string{
		"   Creating an optimized production build ...",
		" ✓ Compiled successfully",
		"",
		"Route (app)                              Size     First Load JS",
		"┌ ○ /                                    5.2 kB          89 kB",
		"├ ○ /about                               1.1 kB          85 kB",
		"├ ● /blog/[slug]                         2.3 kB          86 kB",
		"└ ○ /contact                             980 B           84 kB",
		"",
		"○  (Static)   prerendered as static content",
		"● (SSG)       prerendered as static HTML (uses getStaticProps)",
		"",
		"Compiled in 12.4s",
	}
	out := compressNextBuild("next build", lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "routes:") {
		t.Fatalf("expected routes section:\n%s", joined)
	}
	if !strings.Contains(joined, "/about") {
		t.Fatalf("expected route paths:\n%s", joined)
	}
}

func TestCompressNextBuild_Error(t *testing.T) {
	lines := []string{
		"   Creating an optimized production build ...",
		"Failed to compile.",
		"",
		"./src/app/page.tsx",
		"Type error: Cannot find module '@/components/Hero'",
	}
	out := compressNextBuild("next build", lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "BUILD ERROR") {
		t.Fatalf("expected BUILD ERROR header:\n%s", joined)
	}
}

func TestCompressViteBuild(t *testing.T) {
	lines := []string{
		"vite v5.0.0 building for production...",
		"✓ 1523 modules transformed.",
		"dist/index.html                  0.46 kB │ gzip:  0.30 kB",
		"dist/assets/index-DiwrgTda.css  29.03 kB │ gzip:  5.11 kB",
		"dist/assets/index-Ce5dlfks.js  144.52 kB │ gzip: 46.14 kB",
		"✓ built in 2.35s",
	}
	out := compressNextBuild("vite build", lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "built") {
		t.Fatalf("expected built header:\n%s", joined)
	}
	if !strings.Contains(joined, "chunks:") {
		t.Fatalf("expected chunks section:\n%s", joined)
	}
}
