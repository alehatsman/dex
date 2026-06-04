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

// shellValidate rejects commands that would write files via shell redirect or
// tee — identical rationale to lean-ctx: MCP protocol corruption on large
// payloads, and ctx_shell is for reading output only.
func shellValidate(command string) error {
	if hasFileWriteRedirect(command) {
		return fmt.Errorf("ctx_shell: file-write redirect detected (> or >>); use the Write tool instead")
	}
	lower := strings.ToLower(command)
	if strings.HasPrefix(lower, "tee ") || strings.Contains(lower, "| tee ") {
		return fmt.Errorf("ctx_shell: tee detected; use the Write tool instead")
	}
	return nil
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
				continue // stderr redirect
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
			exitCode = 124 // timeout, same convention as the `timeout` command
		} else {
			exitCode = 1
		}
	}

	raw := stripANSI(buf.String())

	if in.Raw {
		return nil, ShellOutput{
			Output:   raw,
			ExitCode: exitCode,
		}, nil
	}

	compressed, origLines, outLines := CompressText(raw, in.Command, 0)
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
