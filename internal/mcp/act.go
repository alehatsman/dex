package mcp

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// act is the "run and verify" verb of the four-verb surface (#110): execute a
// shell command and get compressed output back, with a trust/cost envelope and a
// routed next step. It is a thin, additive facade over the existing `shell`
// tool — same execution, same compression pipeline — so `shell` stays valid as
// an alias until the cutover. Writes/verification (build, test, git) are act's
// job; deriving context is not.

// ActInput mirrors the subset of ShellInput that the exec verb exposes.
type ActInput struct {
	Command     string `json:"command"                jsonschema:"the shell command to run"`
	Cwd         string `json:"cwd,omitempty"          jsonschema:"working directory (default: server's cwd); must be under project_root when set"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; when set, cwd must resolve inside it"`
	Raw         bool   `json:"raw,omitempty"          jsonschema:"skip compression and return full output"`
	Expect      string `json:"expect,omitempty"       jsonschema:"output-intent hint biasing compression: counts|table|json|logs|raw. Empty = auto-detect."`
	TimeoutSecs int    `json:"timeout_secs,omitempty" jsonschema:"per-call timeout in seconds (default 60, max 600); 0 uses the default"`
}

// ActResult is the verb-specific payload under the envelope's `result`.
type ActResult struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

// ActOutput is the universal envelope for act: an exact-provenance result, the
// compression cost, and — when the command failed with a recognized signature —
// a next step to remember the gotcha.
type ActOutput struct {
	Status string     `json:"status"` // ok | error
	Hint   string     `json:"hint,omitempty"`
	Result ActResult  `json:"result"`
	Trust  EnvTrust   `json:"trust"`
	Cost   *EnvCost   `json:"cost,omitempty"`
	Next   []NextStep `json:"next,omitempty"`
}

func actHandler(h toolSurface) func(context.Context, *sdk.CallToolRequest, ActInput) (*sdk.CallToolResult, ActOutput, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in ActInput) (*sdk.CallToolResult, ActOutput, error) {
		return actVerb(ctx, h, req, in)
	}
}

// actVerb composes over the existing shell handler so the exec path, sandboxing,
// compression, and gotcha detection are all shared — act only re-shapes the
// response into the universal envelope.
func actVerb(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in ActInput) (*sdk.CallToolResult, ActOutput, error) {
	if strings.TrimSpace(in.Command) == "" {
		return nil, ActOutput{Status: "error", Hint: "command is required", Trust: exactTrust()}, nil
	}
	_, sh, err := h.shellRun(ctx, req, ShellInput{
		Command:     in.Command,
		Cwd:         in.Cwd,
		ProjectRoot: in.ProjectRoot,
		Raw:         in.Raw,
		Expect:      in.Expect,
		TimeoutSecs: in.TimeoutSecs,
	})
	if err != nil {
		return nil, ActOutput{Status: "error", Hint: err.Error(), Trust: exactTrust()}, err
	}

	out := ActOutput{
		Status: "ok",
		Result: ActResult{ExitCode: sh.ExitCode, Output: sh.Output},
		Trust:  exactTrust(),
	}
	if sh.SavedPct > 0 {
		out.Cost = &EnvCost{SavedPct: sh.SavedPct}
	}
	// A non-zero exit whose output matched a known failure signature: route the
	// agent to persist the pitfall so it isn't re-derived next time (spec §act).
	if g := sh.GotchaCandidate; g != nil {
		fact := strings.TrimSpace(g.Trigger)
		if g.OutputFragment != "" {
			fact = fmt.Sprintf("%s — %s", fact, strings.TrimSpace(g.OutputFragment))
		}
		archetype := g.Archetype
		if archetype == "" {
			archetype = "Gotcha"
		}
		out.Next = append(out.Next, NextStep{
			Verb: "remember",
			Args: map[string]any{"fact": fact, "archetype": archetype},
			Why:  "command failed with a recognized signature — persist it so it isn't re-derived",
		})
	}
	return nil, out, nil
}
