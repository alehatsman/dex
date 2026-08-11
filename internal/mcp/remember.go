package mcp

import (
	"context"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// remember is the durable-memory verb of the four-verb surface (#110): persist a
// fact (write) or recall the facts relevant to a task (read), across session
// resets. It is a thin, additive facade over the existing `notes`/knowledge
// engine — same store, same salience and scope-binding — exposing just the two
// everyday moves (write, recall). The admin/relate lanes (gc, export, import,
// consolidate, pin, relate, review) stay on `notes` until the cutover and move
// to the CLI afterward; remember deliberately keeps the hot path to two verbs.
type RememberInput struct {
	// Provide exactly one of Fact (write) or Query (recall).
	Fact  string `json:"fact,omitempty"  jsonschema:"a durable fact to persist (write mode); lead a review finding or gotcha with a bracketed [kind]"`
	Query string `json:"query,omitempty" jsonschema:"recall the facts most relevant to this task/question (read mode); empty query returns top facts by salience"`
	// Scope binds a written fact — or filters a recall — to a file glob/path/package.
	Scope       string `json:"scope,omitempty"       jsonschema:"bind a written fact to a file glob/path/package so it surfaces when a file verb touches a match (#645); in recall mode, filter to facts scoped to this path"`
	Archetype   string `json:"archetype,omitempty"  jsonschema:"write mode: Gotcha | Decision | Convention | Architecture | Observation | ReviewFinding | Pattern | Dependency | Fact (default Observation)"`
	K           int    `json:"k,omitempty"          jsonschema:"recall mode: max facts to return (default 10)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or worktree you are working in"`
}

// RememberResult is the verb-specific payload under the envelope's `result`.
type RememberResult struct {
	Mode            string                `json:"mode"` // "wrote" | "recalled"
	Facts           []KnowledgeFactOutput `json:"facts,omitempty"`
	Similar         []KnowledgeFactOutput `json:"similar,omitempty"` // near-duplicate warnings on write
	ScopeSuggestion string                `json:"scope_suggestion,omitempty"`
}

// RememberOutput is the universal envelope for remember. Provenance is exact — a
// note was read from or written to the store, no inference involved.
type RememberOutput struct {
	Status string         `json:"status"` // ok | no-index | error
	Hint   string         `json:"hint,omitempty"`
	Result RememberResult `json:"result"`
	Trust  EnvTrust       `json:"trust"`
	Next   []NextStep     `json:"next,omitempty"`
}

func rememberHandler(h toolSurface) func(context.Context, *sdk.CallToolRequest, RememberInput) (*sdk.CallToolResult, RememberOutput, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in RememberInput) (*sdk.CallToolResult, RememberOutput, error) {
		return rememberVerb(ctx, h, req, in)
	}
}

// rememberVerb composes over the knowledge handler: Fact → add, otherwise Query
// → list (recall). It only re-shapes the response into the universal envelope.
func rememberVerb(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in RememberInput) (*sdk.CallToolResult, RememberOutput, error) {
	fact := strings.TrimSpace(in.Fact)
	if fact != "" {
		_, kn, err := h.knowledge(ctx, req, KnowledgeInput{
			Action:      "add",
			Body:        fact,
			Scope:       in.Scope,
			Archetype:   in.Archetype,
			ProjectRoot: in.ProjectRoot,
		})
		if err != nil {
			return nil, RememberOutput{Status: "error", Hint: err.Error(), Trust: exactTrust()}, err
		}
		out := RememberOutput{
			Status: kn.Status,
			Hint:   kn.Hint,
			Result: RememberResult{
				Mode:            "wrote",
				Facts:           kn.Facts,
				Similar:         kn.Similar,
				ScopeSuggestion: kn.ScopeSuggestion,
			},
			Trust: exactTrust(),
		}
		// A near-duplicate already exists → route the agent to supersede it.
		if len(kn.Similar) > 0 {
			out.Next = append(out.Next, NextStep{
				Verb: "remember",
				Why:  "a near-duplicate fact already exists — consider superseding it via `notes` (action=add, supersedes_id) rather than stacking duplicates",
			})
		}
		return nil, out, nil
	}

	// Recall mode.
	_, kn, err := h.knowledge(ctx, req, KnowledgeInput{
		Action:      "list",
		Query:       in.Query,
		Scope:       in.Scope,
		K:           in.K,
		ProjectRoot: in.ProjectRoot,
	})
	if err != nil {
		return nil, RememberOutput{Status: "error", Hint: err.Error(), Trust: exactTrust()}, err
	}
	return nil, RememberOutput{
		Status: kn.Status,
		Hint:   kn.Hint,
		Result: RememberResult{Mode: "recalled", Facts: kn.Facts},
		Trust:  exactTrust(),
	}, nil
}
