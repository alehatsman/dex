package mcp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/gotcha"
	"github.com/alehatsman/dex/internal/redact"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// lineCount counts text lines the same way CompressText does — ignoring a
// single trailing newline — so the ShellOutput line metrics stay consistent
// across compaction paths.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

// shellInterpreter resolves the interpreter the shell tool runs commands with.
// It prefers bash (resolved once on PATH) so the tool matches the dialect of
// the native Bash tool agents migrate from — `set -o pipefail`, `[[ ]]`,
// process substitution, arrays — and falls back to POSIX sh (always present)
// when bash is absent (#542). The DEX_SHELL env var overrides the choice.
var shellInterpreter = sync.OnceValue(resolveShellInterpreter)

func resolveShellInterpreter() string {
	if override := strings.TrimSpace(os.Getenv("DEX_SHELL")); override != "" {
		return override
	}
	if path, err := exec.LookPath("bash"); err == nil {
		return path
	}
	return "sh"
}

type ShellInput struct {
	Command     string `json:"command"`
	Cwd         string `json:"cwd,omitempty"          jsonschema:"working directory (default: server's cwd); must be under project_root when project_root is set"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; when set, cwd must resolve inside it"`
	Raw         bool   `json:"raw,omitempty"          jsonschema:"skip compression and return full output"`
	TimeoutSecs int    `json:"timeout_secs,omitempty" jsonschema:"per-call timeout in seconds (default 60, max 600); 0 uses the default"`
	// Description is accepted and ignored. The native Bash/exec tool in most
	// agent harnesses REQUIRES a description param, so LLMs reflexively attach
	// one to the first shell call too. With the SDK-generated schema set to
	// additionalProperties:false, that inert key would hard-fail the call and
	// cost a wasted round-trip before the model learns to drop it. Declaring
	// the field makes the schema accept it; the handler never reads it (#81).
	Description string `json:"description,omitempty" jsonschema:"ignored; accepted so agents that reflexively send a command description don't fail the call"`
}

type ShellOutput struct {
	Output        string `json:"output"`
	ExitCode      int    `json:"exit_code"`
	OriginalLines int    `json:"original_lines,omitempty"`
	OutputLines   int    `json:"output_lines,omitempty"`
	SavedPct      int    `json:"saved_pct,omitempty"`
	// GotchaCandidate is a low-confidence Gotcha staged from a recognized
	// failure signature when the command exits non-zero (#601). nil on success
	// or when no known pattern matched. The agent confirms it via `notes add`.
	GotchaCandidate *gotcha.Candidate `json:"gotcha_candidate,omitempty"`
}

const (
	shellTimeout    = 60 * time.Second  // default per-call timeout
	shellTimeoutMax = 600 * time.Second // upper bound on TimeoutSecs (#24)
)

// resolveShellTimeout maps the input TimeoutSecs to a concrete duration,
// clamping to [1s, shellTimeoutMax] and falling back to shellTimeout when the
// caller passes 0 or a negative value. Secs is clamped before the Duration
// multiply to prevent int64 overflow for large JSON-integer inputs.
func resolveShellTimeout(secs int) time.Duration {
	if secs <= 0 {
		return shellTimeout
	}
	const maxSecs = 600 // shellTimeoutMax / time.Second
	if secs > maxSecs {
		return shellTimeoutMax
	}
	return time.Duration(secs) * time.Second
}

// outputSizeMax is the maximum bytes buffered from a shell command. Output
// beyond this is silently discarded to prevent OOM when a command produces
// unbounded output (find /, yes, dd) within the 60-second timeout window.
const outputSizeMax = 8 * 1024 * 1024 // 8 MB

// limitedBuf is a bytes.Buffer capped at limit bytes.  Writes past the cap are
// accepted (return the full len to satisfy io.Writer) but discarded so the
// command keeps running normally until the timeout fires.
type limitedBuf struct {
	buf   bytes.Buffer
	limit int
}

func (l *limitedBuf) Write(p []byte) (int, error) {
	if remaining := l.limit - l.buf.Len(); remaining > 0 {
		take := p
		if len(take) > remaining {
			take = p[:remaining]
		}
		l.buf.Write(take) //nolint:errcheck // bytes.Buffer.Write never errors
	}
	return len(p), nil
}

func (l *limitedBuf) String() string { return l.buf.String() }

// shellWrappedEnv marks child processes spawned by the shell tool so a nested
// dex (or another compression wrapper that honors the convention) can detect
// the re-entry and skip a second compression pass on the same bytes (#25).
// On entry, if this is already set by a parent, the shell tool degrades to
// raw output for the same reason.
const shellWrappedEnv = "DEX_SHELL_WRAPPED"

// reAnsi matches the terminal escape sequences that leak into command output:
//   - CSI: ESC '[' params(0x30-3F) intermediates(0x20-2F) final(0x40-7E) —
//     covers SGR colour (m), cursor moves (A-H), erase (J/K), and private
//     modes like cursor hide/show (?25l/?25h), not just the SGR subset.
//   - OSC: ESC ']' … (BEL | ST) — window titles and hyperlinks.
//
// Both alternatives require the ESC byte, so plain text such as "[INFO]" is
// never touched (#670).
var reAnsi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// reTeeWord matches "tee" as a whole word so |tee (no space) and "xargs tee"
// are caught alongside the "tee file" prefix case (#716).
var reTeeWord = regexp.MustCompile(`\btee\b`)

func stripANSI(s string) string { return reAnsi.ReplaceAllString(s, "") }

// ── output policy ─────────────────────────────────────────────────────────────

type shellPolicy int

const (
	// policyCompress: apply CompressText (build/test/lint output).
	policyCompress shellPolicy = iota
	// policyMinimal: light cleanup that preserves structure (#616) — drop git
	// index-hash plumbing, collapse blank runs, dedup consecutive non-signal
	// lines, but keep every diff/error/count line. For git diff/log/show/blame
	// and dependency audits: wasteful verbatim, unsafe to compress aggressively.
	policyMinimal
	// policyVerbatim: strip ANSI, hard-cap lines, no pattern compression
	// (curl, jq, cat, structured data queries).
	policyVerbatim
	// policyPassthrough: return output completely unchanged — no ANSI strip,
	// no compression, no truncation (dev servers, auth flows, interactive REPLs).
	policyPassthrough
)

// classifyCommand returns the output policy for the given command.
// Priority: passthrough > verbatim > minimal > test-runner > compress.
func classifyCommand(command string) shellPolicy {
	if isPassthroughCommand(command) {
		return policyPassthrough
	}
	if isVerbatimCommand(command) {
		return policyVerbatim
	}
	// Structured-but-bulky output (git diff/log/show/blame, dependency audits):
	// safe to clean lightly, unsafe to compress aggressively (#616).
	if isMinimalCommand(command) {
		return policyMinimal
	}
	// Test runner output must be preserved verbatim — agents need the full
	// pass/fail breakdown, not a compressed summary that may lose failures.
	if isTestRunnerCommand(command) {
		return policyVerbatim
	}
	return policyCompress
}

// testRunnerPrefixes — commands known to produce test result output that must
// be preserved verbatim regardless of length.
var testRunnerPrefixes = []string{
	"cargo test", "cargo nextest", "nextest",
	"pytest", "python -m pytest", "python3 -m pytest",
	"go test", "gotestsum",
	"npm test", "npm run test", "pnpm test", "pnpm run test",
	"yarn test", "bun test", "deno test",
	"jest", "vitest", "mocha", "jasmine",
	"npx jest", "npx vitest", "npx mocha",
	"dotnet test", "mix test",
	"rspec", "bundle exec rspec", "phpunit",
	"./gradlew test", "gradle test", "mvn test", "ctest",
}

// isTestRunnerCommand returns true when the effective command (after stripping
// leading env-var assignments like RUST_BACKTRACE=1) is a known test runner.
func isTestRunnerCommand(command string) bool {
	cmd := strings.TrimSpace(command)
	// strip leading VAR=value prefixes
	for {
		first := strings.SplitN(cmd, " ", 2)
		if len(first) < 2 || !strings.Contains(first[0], "=") {
			break
		}
		cmd = strings.TrimSpace(first[1])
	}
	cl := strings.ToLower(cmd)
	for _, p := range testRunnerPrefixes {
		if cl == p || strings.HasPrefix(cl, p+" ") || strings.HasPrefix(cl, p+"\t") {
			return true
		}
	}
	return false
}

// buildToolPrefixes — commands that produce compiler/linter diagnostics.
// When these tools produce error output, compression must be skipped.
var buildToolPrefixes = []string{
	"go build", "go vet", "go generate", "golangci-lint",
	"tsc", "npx tsc",
	"eslint", "biome", "hadolint", "yamllint", "oxlint",
	"mypy", "pyright", "basedpyright",
	"ruff check",
	"cargo build", "cargo check", "cargo clippy", "rustc",
	"gcc ", "g++ ", "cc ", "clang ", "clang++ ",
	"cmake ", "ninja ",
	"make", "gmake",
	"dotnet build",
	"./gradlew build", "gradle build", "mvn compile", "mvn verify",
	"swift build", "zig build",
	"shellcheck", "rubocop", "mix compile", "mix credo",
}

var buildErrorMarkers = []string{
	"error:", "error[", "Error:", "FAILED", " failed",
	"cannot find", "undefined", "panicked at",
	"could not compile", "compilation failed", "BUILD FAILED",
}

// isBuildToolWithErrors returns true when the command is a known build/lint
// tool AND the output contains error indicators. In this case compression
// must be skipped — agents need the full diagnostic (file path, line, note).
func isBuildToolWithErrors(cmd, output string) bool {
	cl := strings.ToLower(strings.TrimSpace(cmd))
	// strip leading env-var prefixes
	for {
		first := strings.SplitN(cl, " ", 2)
		if len(first) < 2 || !strings.Contains(first[0], "=") {
			break
		}
		cl = strings.TrimSpace(first[1])
	}
	found := false
	for _, p := range buildToolPrefixes {
		if cl == strings.TrimRight(p, " ") || strings.HasPrefix(cl, p) {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	for _, marker := range buildErrorMarkers {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

// passthroughCommands is the canonical list of commands whose output must never
// be modified. Mirrors lean-ctx's BUILTIN_PASSTHROUGH list.
var passthroughPrefixes = []string{
	// auth flows
	"az login", "az account", "gh auth", "gh browse", "gh codespace",
	"gh ssh-key", "gh gpg-key", "gcloud auth", "gcloud init",
	"aws configure sso", "firebase login", "netlify login", "vercel login",
	"heroku login", "flyctl auth", "fly auth", "railway login",
	"supabase login", "wrangler login", "doppler login", "vault login",
	"oc login", "kubelogin",
	// JS/TS dev servers & watchers
	"next dev", "vite dev", "vite preview", "vitest", "nuxt dev",
	"astro dev", "webpack serve", "webpack-dev-server", "nodemon",
	"concurrently", "pm2", "gatsby develop", "expo start",
	"react-scripts start", "ng serve", "remix dev", "hugo server",
	"hugo serve", "jekyll serve", "bun dev", "ember serve",
	"npm run dev", "npm run start", "npm run serve", "npm run watch",
	"npm run preview", "npm run storybook", "npm start",
	"pnpm run dev", "pnpm run start", "pnpm run serve", "pnpm run watch",
	"pnpm run preview", "pnpm run storybook", "pnpm dev", "pnpm start",
	"pnpm preview", "yarn dev", "yarn start", "yarn serve", "yarn watch",
	"yarn preview", "yarn storybook", "bun run dev", "bun run start",
	"bun run serve", "bun run watch", "bun run preview", "bun start",
	"deno task dev", "deno task start", "deno task serve", "deno run --watch",
	// Docker
	"docker compose up", "docker-compose up",
	"docker compose logs", "docker-compose logs",
	"docker compose exec", "docker-compose exec",
	"docker compose run", "docker-compose run",
	"docker compose watch", "docker-compose watch",
	"docker logs", "docker attach", "docker stats", "docker events",
	"docker exec -it", "docker exec -ti", "docker run -it", "docker run -ti",
	// Kubernetes
	"kubectl logs", "kubectl exec -it", "kubectl exec -ti",
	"kubectl attach", "kubectl port-forward", "kubectl proxy",
	// System monitors & streaming
	"htop", "btop", "watch ", "tail -f",
	"journalctl -f", "journalctl --follow", "dmesg -w", "dmesg --follow",
	"strace", "tcpdump", "ping ", "ping6 ", "traceroute", "mtr ",
	"nmap ", "iperf ", "iperf3 ", "ss -l", "netstat -l", "lsof -i", "socat ",
	// Interactive REPLs & editors
	"psql", "mysql", "sqlite3", "redis-cli", "mongosh",
	"python3 -i", "python -i", "rails console", "rails c ",
	"more", "nvim", "micro ", "helix ", "emacs", "tmux", "screen",
	"telnet ", "ncat ",
	// Python servers & workers
	"flask run", "uvicorn ", "gunicorn ", "hypercorn ", "daphne ",
	"django-admin runserver", "manage.py runserver",
	"python manage.py runserver", "python -m http.server",
	"python3 -m http.server", "streamlit run", "gradio ",
	"celery worker", "celery -a", "celery -b", "dramatiq ", "rq worker",
	"watchmedo ", "ptw ", "pytest-watch",
	// Ruby / Rails
	"rails server", "puma ", "unicorn ", "thin start",
	"foreman start", "overmind start", "guard ", "sidekiq", "reside ",
	// PHP
	"php artisan serve", "php artisan queue:work", "php artisan queue:listen",
	"php artisan horizon", "php artisan tinker", "sail up",
	// Java / JVM
	"./gradlew bootrun", "gradlew bootrun", "gradle bootrun",
	"./gradlew run", "mvn spring-boot:run", "./mvnw spring-boot:run",
	"mvn quarkus:dev", "./mvnw quarkus:dev",
	"sbt run", "sbt ~compile", "lein run", "lein repl",
	// Go watchers
	"air ", "gin ", "realize start", "reflex ", "gowatch ",
	// .NET
	"dotnet run", "dotnet watch", "dotnet ef",
	// Elixir
	"mix phx.server", "iex -s mix",
	// Rust
	"cargo watch", "cargo run", "cargo leptos watch", "bacon ",
	// General watchers
	"make dev", "make serve", "make watch", "make run", "make start",
	"just dev", "just serve", "just watch", "just start", "just run",
	"task dev", "task serve", "task watch",
	"nix develop", "devenv up",
	// CI/CD & infra
	"act ", "skaffold dev", "tilt up", "garden dev", "telepresence ",
	// Load testing
	"wrk ", "hey ", "vegeta ", "k6 run", "artillery run",
}

// passthroughContains are substrings that make any command passthrough.
var passthroughContains = []string{"--use-device-code"}

// passthroughNot are prefixes that look like passthrough commands (e.g. bare
// psql) but are actually one-shot queries when flags like -c/-e are present.
// Checked before the passthrough prefix scan so they fall through to verbatim.
var passthroughNot = []string{
	"psql -c", "psql --command",
	"mysql -e", "mysql --execute",
	"mariadb -e",
	"redis-cli --eval",
}

func isPassthroughCommand(command string) bool {
	cl := strings.ToLower(strings.TrimSpace(command))
	for _, exc := range passthroughNot {
		if strings.HasPrefix(cl, exc) {
			return false
		}
	}
	for _, sub := range passthroughContains {
		if strings.Contains(cl, sub) {
			return true
		}
	}
	for _, p := range passthroughPrefixes {
		if p[len(p)-1] == ' ' {
			// trailing-space prefix: match exact or starts-with
			if cl == strings.TrimRight(p, " ") || strings.HasPrefix(cl, p) {
				return true
			}
		} else if cl == p || strings.HasPrefix(cl, p+" ") || strings.HasPrefix(cl, p+"\t") {
			return true
		}
	}
	return false
}

// verbatimPrefixes — commands whose output is structured data that must be
// preserved (curl, jq, cat, database queries, etc.).
var verbatimPrefixes = []string{
	// HTTP clients
	"curl ", "wget ", "http ", "xh ", "curlie ", "grpcurl ",
	// Data format tools
	"jq ", "jq\t", "yq ", "yq\t", "fx ", "gron ", "mlr ", "dasel ",
	"csvlook ", "csvcut ", "csvjson ", "in2csv ", "xq ",
	// File viewers (non-streaming)
	"cat ", "bat ", "batcat ", "pygmentize ",
	"head ", "xxd ", "hexdump ", "od ", "strings ", "file ",
	// Binary / crypto inspection
	"openssl ", "gpg ", "age ", "ssh-keygen ", "certutil ",
	// DNS / network inspection
	"dig ", "nslookup ", "host ", "whois ", "drill ", "resolvectl ",
	// Infra inspection (read-only)
	"terraform output", "terraform show", "terraform state show",
	"terraform state list", "terraform state pull",
	"tofu output", "tofu show", "tofu state",
	"docker inspect", "docker ps", "docker images",
	"podman inspect", "podman ps", "podman images",
	"kubectl get ", "kubectl describe ", "kubectl explain ",
	"helm get ", "helm list", "helm ls", "helm template",
	// Cloud CLI queries (passthrough handles login/auth subcommands first)
	"aws ", "gcloud ", "az ",
	// CLI API data — structured JSON responses
	"gh api", "gh --json", "glab api",
	// Package manager info (read-only, not install). Audits move to the minimal
	// tier (#616) — their tables are bulky but must keep CVE/severity lines.
	"npm list", "npm outdated", "npm info",
	"cargo metadata", "cargo tree",
	"go list ", "go version", "go env",
	"brew list", "brew info", "brew outdated",
	"apt list", "apt show", "dpkg -l",
	"pip list", "pip show", "pip freeze",
	// Config / metadata viewers
	"git config", "git remote", "git rev-parse", "git ls-files",
	"git ls-tree", "git for-each-ref", "git cat-file", "git name-rev",
	"git describe", "git shortlog",
	"kubectl config view", "kubectl config get-contexts",
	// Git write commands — output carries confirmation/rejection messages
	// that agents must read verbatim (merge conflicts, push rejections, etc.)
	"git push", "git pull", "git merge", "git commit", "git rebase",
	"git cherry-pick", "git reset", "git stash",
	// System queries
	"stat ", "wc ", "id ", "whoami", "hostname", "uname",
	"lscpu", "lsblk", "base64 ", "sha256sum ", "sha1sum ", "md5sum ",
	"readlink ", "realpath ", "which ", "type ",
	"ip addr", "ip link", "ip route", "ifconfig", "ss -", "netstat ",
	"df ", "du ", "free ", "uptime", "lsof ",
	// Language one-liners (produce data, not interactive)
	"python -c", "python3 -c", "node -e", "ruby -e", "perl -e", "php -r",
	// Version / help (always short, never worth compressing)
	"--version", "-v ", "--help", "-h ",
	// Environment dumps
	"env", "printenv", "set ",
	// Archive listing
	"tar -t", "tar --list", "unzip -l", "zip -sf", "lsar ",
	// One-shot database queries (bare psql/mysql are passthrough REPLs)
	"psql -c", "psql --command", "mysql -e", "mysql --execute",
	"mariadb -e", "sqlite3 ", "mongosh --eval", "redis-cli --eval",
	// Git data: log/show/diff/blame move to the minimal tier (#616) — bulky but
	// every +/- diff line must survive. stash list/tag/branch stay verbatim
	// (short, no diff content to lightly trim).
	"git stash list", "git tag", "git branch",
	// Non-streaming log viewers
	"journalctl --no-pager", "journalctl -u", "journalctl -b",
	// Clipboard read
	"pbpaste", "wl-paste", "xclip -o", "xsel -o",
}

func isVerbatimCommand(command string) bool {
	cl := strings.ToLower(strings.TrimSpace(command))
	for _, p := range verbatimPrefixes {
		if cl == strings.TrimRight(p, " ") ||
			strings.HasPrefix(cl, p) ||
			strings.HasPrefix(cl, strings.TrimRight(p, " ")+"\t") {
			return true
		}
	}
	// pipe tail: if last segment is verbatim (e.g. `go test | jq .`)
	if idx := strings.LastIndex(cl, "|"); idx >= 0 {
		tail := strings.TrimSpace(cl[idx+1:])
		if isVerbatimCommand(tail) {
			return true
		}
	}
	return false
}

// minimalPrefixes — structured-but-bulky output that earns the light Minimal
// tier (#616): keep every diff/error/count line, drop only blank runs, dup
// lines, and git index-hash plumbing. These were verbatim before; verbatim
// never compresses, so a 200-commit log or a large diff cost full tokens.
var minimalPrefixes = []string{
	"git diff", "git log", "git show", "git blame",
	"npm audit", "cargo audit", "pnpm audit", "yarn audit",
}

// isMinimalCommand matches on the leading command only (so `git diff | grep x`
// is still minimal). A pipe whose TAIL is a verbatim/passthrough tool is caught
// earlier by classifyCommand's higher-priority checks — the established
// last-segment-wins rule for pipes — so this needs no pipe-tail handling.
func isMinimalCommand(command string) bool {
	cl := strings.ToLower(strings.TrimSpace(command))
	for _, p := range minimalPrefixes {
		if cl == p || strings.HasPrefix(cl, p+" ") || strings.HasPrefix(cl, p+"\t") {
			return true
		}
	}
	return false
}

// ── auth-flow detection ───────────────────────────────────────────────────────

var authFlowStrong = []string{
	"devicelogin", "deviceauth", "device_code", "device code",
	"device-code", "verification_uri", "user_code", "one-time code",
}

var authFlowWeak = []string{
	"enter the code", "enter this code", "enter code:", "use the code",
	"use a web browser to open", "open the page",
	"authenticate by visiting", "sign in with the code",
	"sign in using a code", "verification code",
	"authorize this device", "waiting for authentication",
	"waiting for login", "open your browser", "open in your browser",
}

// containsAuthFlow returns true when output looks like an OAuth device-code
// flow. Output must never be compressed or truncated in this case.
func containsAuthFlow(output string) bool {
	lower := strings.ToLower(output)
	for _, s := range authFlowStrong {
		if strings.Contains(lower, s) {
			return true
		}
	}
	for _, s := range authFlowWeak {
		if strings.Contains(lower, s) {
			if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
				return true
			}
		}
	}
	return false
}

// ── validation ────────────────────────────────────────────────────────────────

// shellWritesEnv opts out of the file-write guard below. Unset (the default)
// keeps redirects/tee/heredoc-to-file blocked so an agent reaches for the Write
// tool; setting it to "1" or "true" lets power-user workflows (e.g. piping a
// build log to a file inside a script) through, matching native Bash (#596).
const shellWritesEnv = "DEX_SHELL_ALLOW_WRITES"

// shellWritesAllowed reports whether the operator opted out of the file-write
// guard via shellWritesEnv.
func shellWritesAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(shellWritesEnv))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// shellValidate rejects commands that would write files via shell redirect,
// tee, or heredoc-to-file — unless DEX_SHELL_ALLOW_WRITES opts out (#596).
func shellValidate(command string) error {
	if shellWritesAllowed() {
		return nil
	}
	if hasFileWriteRedirect(command) {
		return fmt.Errorf("shell: file-write redirect detected (> or >>); use the Write tool instead")
	}
	// Block tee as a whole word — catches |tee (no space), xargs tee, etc.
	if reTeeWord.MatchString(strings.ToLower(command)) {
		return fmt.Errorf("shell: tee detected; use the Write tool instead")
	}
	if hasHeredocFileWrite(command) {
		return fmt.Errorf("shell: heredoc writing to a file detected; use the Write tool instead")
	}
	return nil
}

// hasHeredocFileWrite blocks `cat <<EOF > file` patterns.
func hasHeredocFileWrite(command string) bool {
	if !strings.Contains(command, "<<") {
		return false
	}
	cl := strings.ToLower(command)
	heredocPatterns := []string{"<<eof", "<<'eof'", `<<"eof"`, "<<end", "<<'end'"}
	hasKnown := false
	for _, p := range heredocPatterns {
		if strings.Contains(cl, p) {
			hasKnown = true
			break
		}
	}
	if !hasKnown {
		return false
	}
	return hasFileWriteRedirect(command)
}

// hasFileWriteRedirect detects `>` / `>>` that target a real file, skipping
// `2>`, `>/dev/null`, and `>` inside quotes.
func hasFileWriteRedirect(command string) bool {
	var inSingle, inDouble bool
	b := []byte(command)
	for i, c := range b {
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '>':
			if inSingle || inDouble {
				continue
			}
			start := i + 1
			if start < len(b) && b[start] == '>' {
				start++ // ">>" append
			}
			target := strings.TrimSpace(string(b[start:]))
			// fd duplication/closing (2>&1, 1>&2, >&2, 2>&-): the target begins
			// with '&' — never a file write. Checked before tokenizing so a glued
			// trailing operator (2>&1;) can't break the exemption.
			if strings.HasPrefix(target, "&") {
				continue
			}
			// Delimit the target on ANY shell metacharacter, not just a space, so
			// a glued operator (2>/dev/null;, >/dev/null|grep, (cmd 2>/dev/null))
			// isn't captured into the target and defeat the exemption (#538).
			target = firstShellToken(target)
			// Only a real filesystem target is a write (#507). Allow the
			// bit-bucket /dev/null and a bare '>' with no target on this token.
			if target == "" || target == "/dev/null" {
				continue
			}
			return true
		}
	}
	return false
}

// firstShellToken returns the leading token of s up to the first shell
// metacharacter (whitespace or an operator), or "" if s is empty or begins
// with one. Used to isolate a redirect target from any glued operator (#538).
func firstShellToken(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ';', '|', '&', '(', ')':
			return true
		}
		return false
	})
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// ── handler ───────────────────────────────────────────────────────────────────

func (s *Server) ShellRun(ctx context.Context, in ShellInput) (ShellOutput, error) {
	_, out, err := s.shellRun(ctx, nil, in)
	return out, err
}

func (s *Server) shellRun(ctx context.Context, _ *sdk.CallToolRequest, in ShellInput) (res *sdk.CallToolResult, out ShellOutput, err error) {
	// Stage a Gotcha candidate from any non-zero exit, uniformly across every
	// return path below (#601). Runs after the output is assembled so it sees
	// the compressed text the agent sees; nil unless a known failure signature
	// matched, so the field is omitted on success and on unrecognized failures.
	defer func() {
		if err == nil && out.ExitCode != 0 {
			out.GotchaCandidate = gotcha.Detect(in.Command, out.Output, out.ExitCode)
		}
	}()

	if strings.TrimSpace(in.Command) == "" {
		return nil, ShellOutput{}, fmt.Errorf("command is required")
	}
	if err := shellValidate(in.Command); err != nil {
		return nil, ShellOutput{}, err
	}

	cwd := in.Cwd
	if cwd == "" {
		if in.ProjectRoot != "" {
			cwd = in.ProjectRoot
		} else {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				cwd = "."
			}
		}
	}
	// Contain the working directory to the project root when one is provided.
	if in.ProjectRoot != "" && cwd != "" {
		root, err := filepath.Abs(in.ProjectRoot)
		if err != nil {
			return nil, ShellOutput{}, fmt.Errorf("shell: invalid project_root: %v", err)
		}
		abs, err := filepath.Abs(cwd)
		if err != nil {
			return nil, ShellOutput{}, fmt.Errorf("shell: invalid cwd: %v", err)
		}
		if abs != root && !strings.HasPrefix(abs+string(filepath.Separator), root+string(filepath.Separator)) {
			return nil, ShellOutput{}, fmt.Errorf("shell: cwd %q is outside project root %q", abs, root)
		}
		cwd = abs
	}

	// Re-entry: a parent (nested dex, lean-ctx, …) already compressed once.
	// Degrade to raw to avoid double-compression on the same bytes.
	if !in.Raw && os.Getenv(shellWrappedEnv) == "1" {
		in.Raw = true
	}

	ctx, cancel := context.WithTimeout(ctx, resolveShellTimeout(in.TimeoutSecs))
	defer cancel()

	cmd := exec.CommandContext(ctx, shellInterpreter(), "-c", in.Command)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), shellWrappedEnv+"=1")
	// Without process-group setup, context cancellation only kills the shell
	// wrapper; descendants holding stdout/stderr pipes keep cmd.Wait blocked
	// until they exit naturally, so the timeout fires but the work doesn't
	// actually stop.
	setupShellProcessGroup(cmd)

	buf := &limitedBuf{limit: outputSizeMax}
	cmd.Stdout = buf
	cmd.Stderr = buf

	runErr := cmd.Run()

	exitCode := 0
	if runErr != nil {
		// Timeout-by-cancel: CommandContext sends SIGKILL, which yields an
		// ExitError with ExitCode()=-1. Check ctx first so the conventional
		// 124 surfaces, not the SIGKILL artifact.
		switch {
		case ctx.Err() != nil:
			exitCode = 124
		default:
			if ee, ok := runErr.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
			} else {
				exitCode = 1
			}
		}
	}

	rawBytes := buf.String()

	if in.Raw {
		return nil, ShellOutput{Output: redact.Mask(stripANSI(rawBytes)), ExitCode: exitCode}, nil
	}

	policy := classifyCommand(in.Command)

	// Passthrough: return as-is, no stripping, no compression.
	if policy == policyPassthrough {
		return nil, ShellOutput{Output: rawBytes, ExitCode: exitCode}, nil
	}

	clean := redact.Mask(stripANSI(rawBytes))

	// Lossless JSON compaction (#619): JSON-shaped output (jq, gh api, go list
	// -json, config dumps) is 20–50% insignificant whitespace. Strip it with a
	// text-level scan — no re-parse, no semantic change — and return the result
	// verbatim. This runs ahead of policy routing so JSON from any command is
	// handled, and because pure whitespace removal is already lossless the
	// line-dropping passes below must never touch it.
	if compact, ok := compress.CompactJSONAuto(clean); ok {
		orig := lineCount(clean)
		outLines := lineCount(compact)
		saved := 0
		if orig > 0 {
			saved = (orig - outLines) * 100 / orig
		}
		return nil, ShellOutput{
			Output:        compact,
			ExitCode:      exitCode,
			OriginalLines: orig,
			OutputLines:   outLines,
			SavedPct:      saved,
		}, nil
	}

	// Minimal: light structure-preserving cleanup for git diff/log/show/blame
	// and dependency audits (#616) — keeps every diff/error/count line.
	if policy == policyMinimal {
		compressed, orig, out := CompressMinimal(clean)
		saved := 0
		if orig > 0 {
			saved = (orig - out) * 100 / orig
		}
		s.activityRecord(cwd, shellActivityWeight(in.Command))
		return nil, ShellOutput{
			Output:        compressed,
			ExitCode:      exitCode,
			OriginalLines: orig,
			OutputLines:   out,
			SavedPct:      saved,
		}, nil
	}

	// Verbatim: strip ANSI, preserve content (only hard-cap via maxLines).
	if policy == policyVerbatim {
		compressed, orig, out := CompressText(clean, "", 0) // empty command → generic pass (just caps lines)
		saved := 0
		if orig > 0 {
			saved = (orig - out) * 100 / orig
		}
		return nil, ShellOutput{
			Output:        compressed,
			ExitCode:      exitCode,
			OriginalLines: orig,
			OutputLines:   out,
			SavedPct:      saved,
		}, nil
	}

	// Build tool with errors: preserve full diagnostics verbatim (#81).
	if isBuildToolWithErrors(in.Command, clean) {
		compressed, orig, out := CompressText(clean, "", 0)
		saved := 0
		if orig > 0 {
			saved = (orig - out) * 100 / orig
		}
		s.activityRecord(cwd, shellActivityWeight(in.Command))
		return nil, ShellOutput{
			Output:        compressed,
			ExitCode:      exitCode,
			OriginalLines: orig,
			OutputLines:   out,
			SavedPct:      saved,
		}, nil
	}

	// Compress: but protect auth-flow output from modification.
	if containsAuthFlow(clean) {
		s.activityRecord(cwd, shellActivityWeight(in.Command))
		return nil, ShellOutput{Output: clean, ExitCode: exitCode}, nil
	}

	compressed, origLines, outLines := CompressText(clean, in.Command, 0)
	saved := 0
	if origLines > 0 {
		saved = (origLines - outLines) * 100 / origLines
	}

	s.activityRecord(cwd, shellActivityWeight(in.Command))
	return nil, ShellOutput{
		Output:        compressed,
		ExitCode:      exitCode,
		OriginalLines: origLines,
		OutputLines:   outLines,
		SavedPct:      saved,
	}, nil
}

// shellActivityWeight returns an activity weight for a shell command.
// Build/test/run commands are more significant than casual lookups.
func shellActivityWeight(cmd string) int {
	lower := strings.ToLower(strings.TrimSpace(cmd))
	for _, kw := range []string{"test", "build", "run ", "cargo ", "go test", "go build", "npm ", "make ", "pytest"} {
		if strings.Contains(lower, kw) {
			return 3
		}
	}
	return 2
}
