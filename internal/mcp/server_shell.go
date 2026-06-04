package mcp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type ShellInput struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"  jsonschema:"working directory (default: server's cwd)"`
	Raw     bool   `json:"raw,omitempty"  jsonschema:"skip compression and return full output"`
}

type ShellOutput struct {
	Output        string `json:"output"`
	ExitCode      int    `json:"exit_code"`
	OriginalLines int    `json:"original_lines,omitempty"`
	OutputLines   int    `json:"output_lines,omitempty"`
	SavedPct      int    `json:"saved_pct,omitempty"`
}

const shellTimeout = 60 * time.Second

var reAnsi = regexp.MustCompile(`\x1b\[[0-9;]*[mGKHF]`)

func stripANSI(s string) string { return reAnsi.ReplaceAllString(s, "") }

// ── output policy ─────────────────────────────────────────────────────────────

type shellPolicy int

const (
	// policyCompress: apply CompressText (build/test/lint output).
	policyCompress shellPolicy = iota
	// policyVerbatim: strip ANSI, hard-cap lines, no pattern compression
	// (curl, jq, cat, structured data queries).
	policyVerbatim
	// policyPassthrough: return output completely unchanged — no ANSI strip,
	// no compression, no truncation (dev servers, auth flows, interactive REPLs).
	policyPassthrough
)

// classifyCommand returns the output policy for the given command.
// Priority: passthrough > verbatim > compress.
func classifyCommand(command string) shellPolicy {
	if isPassthroughCommand(command) {
		return policyPassthrough
	}
	if isVerbatimCommand(command) {
		return policyVerbatim
	}
	return policyCompress
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

func isPassthroughCommand(command string) bool {
	cl := strings.ToLower(strings.TrimSpace(command))
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
	"csvlook ", "csvcut ", "csvjson ", "in2csv ",
	// File viewers (non-streaming)
	"cat ", "bat ", "batcat ", "pygmentize ",
	"head ", "xxd ", "hexdump ", "od ", "strings ",
	// Infra inspection (read-only)
	"terraform output", "terraform show", "terraform state show",
	"terraform state list", "terraform state pull",
	"tofu output", "tofu show", "tofu state",
	"docker inspect", "podman inspect",
	"kubectl get ", "kubectl describe ", "kubectl explain ",
	// Cloud CLI queries
	"aws ec2 describe", "aws s3 ls", "aws iam list",
	"gcloud compute instances list", "gcloud projects list",
	// Version / help (always short, never worth compressing)
	"--version", "-v ", "--help", "-h ",
	// Environment dumps
	"env", "printenv", "set ",
	// Archive listing
	"tar -t", "tar --list", "unzip -l", "zip -sf",
	// Git data (log/show/diff/blame produce structured output agents read)
	"git log ", "git show ", "git diff ", "git blame ",
	"git stash list", "git tag", "git branch",
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

// shellValidate rejects commands that would write files via shell redirect,
// tee, or heredoc-to-file.
func shellValidate(command string) error {
	if hasFileWriteRedirect(command) {
		return fmt.Errorf("ctx_shell: file-write redirect detected (> or >>); use the Write tool instead")
	}
	lower := strings.ToLower(command)
	if strings.HasPrefix(lower, "tee ") || strings.Contains(lower, "| tee ") {
		return fmt.Errorf("ctx_shell: tee detected; use the Write tool instead")
	}
	if hasHeredocFileWrite(command) {
		return fmt.Errorf("ctx_shell: heredoc writing to a file detected; use the Write tool instead")
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
			if i > 0 && b[i-1] == '2' {
				continue
			}
			start := i + 1
			if start < len(b) && b[start] == '>' {
				start++
			}
			target := strings.TrimSpace(string(b[start:]))
			target = strings.SplitN(target, " ", 2)[0]
			if target == "/dev/null" || target == "" {
				continue
			}
			return true
		}
	}
	return false
}

// ── handler ───────────────────────────────────────────────────────────────────

func (s *Server) ShellRun(ctx context.Context, in ShellInput) (ShellOutput, error) {
	_, out, err := s.shellRun(ctx, nil, in)
	return out, err
}

func (s *Server) shellRun(_ context.Context, _ *sdk.CallToolRequest, in ShellInput) (*sdk.CallToolResult, ShellOutput, error) {
	if strings.TrimSpace(in.Command) == "" {
		return nil, ShellOutput{}, fmt.Errorf("command is required")
	}
	if err := shellValidate(in.Command); err != nil {
		return nil, ShellOutput{}, err
	}

	cwd := in.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			cwd = "."
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), shellTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", in.Command)
	cmd.Dir = cwd

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()

	exitCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else if ctx.Err() != nil {
			exitCode = 124
		} else {
			exitCode = 1
		}
	}

	rawBytes := buf.String()

	if in.Raw {
		return nil, ShellOutput{Output: stripANSI(rawBytes), ExitCode: exitCode}, nil
	}

	policy := classifyCommand(in.Command)

	// Passthrough: return as-is, no stripping, no compression.
	if policy == policyPassthrough {
		return nil, ShellOutput{Output: rawBytes, ExitCode: exitCode}, nil
	}

	clean := stripANSI(rawBytes)

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

	// Compress: but protect auth-flow output from modification.
	if containsAuthFlow(clean) {
		return nil, ShellOutput{Output: clean, ExitCode: exitCode}, nil
	}

	compressed, origLines, outLines := CompressText(clean, in.Command, 0)
	saved := 0
	if origLines > 0 {
		saved = (origLines - outLines) * 100 / origLines
	}

	return nil, ShellOutput{
		Output:        compressed,
		ExitCode:      exitCode,
		OriginalLines: origLines,
		OutputLines:   outLines,
		SavedPct:      saved,
	}, nil
}
