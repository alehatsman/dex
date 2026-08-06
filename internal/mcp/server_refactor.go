package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/alehatsman/dex/internal/refactor"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── verb: refactor ───────────────────────────────────────────────────────
//
// refactor (#638 / GitHub #65 Tier S3) plans type-precise source edits for the
// host agent to apply. dex is read-only by design (#551): refactor NEVER writes
// files — it returns (path, start_byte, end_byte, replacement) edit triples and
// the agent applies them with its own Edit tool.
//
// v1 supports only op=rename_symbol for Go, planned on-demand via a
// go/packages + go/types load (no index needed — it reads source directly).
// Other ops (change_signature / extract_method / inline / move) and a gopls
// compile-precheck are deferred to v2.

// RefactorInput drives the refactor verb.
type RefactorInput struct {
	Op          string `json:"op,omitempty" jsonschema:"the operation; v1 supports only 'rename_symbol' (the default)"`
	Symbol      string `json:"symbol" jsonschema:"the symbol to rename: bare ('Foo'), receiver-qualified ('(*Server).Run'), or package-tail-qualified ('mcp.NewServer')"`
	To          string `json:"to" jsonschema:"the new identifier"`
	Etag        string `json:"etag,omitempty" jsonschema:"optional plan etag from a prior call; if the touched files changed since, the verb returns status 'stale'"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
}

// RefactorOutput is the edit plan. Edits is the full type-resolved set; the
// agent applies them (e.g. highest-offset-first per file) with its Edit tool.
type RefactorOutput struct {
	Status   string                `json:"status"` // ok | unsupported-language | not-found | ambiguous | stale | error
	Hint     string                `json:"hint,omitempty"`
	Project  string                `json:"project,omitempty"`
	Op       string                `json:"op,omitempty"`
	From     string                `json:"from,omitempty"`
	To       string                `json:"to,omitempty"`
	Object   string                `json:"object,omitempty"`
	Edits    []refactor.EditTriple `json:"edits,omitempty"`
	Files    int                   `json:"files,omitempty"`
	Etag     string                `json:"etag,omitempty"`
	Warnings []string              `json:"warnings,omitempty"`
}

// Refactor runs the refactor verb without an SDK request — the REST `/refactor`
// route. Composes over the local *Server exactly like the stdio tool.
func (s *Server) Refactor(ctx context.Context, in RefactorInput) (RefactorOutput, error) {
	_, out, err := s.refactor(ctx, nil, in)
	return out, err
}

func (s *Server) refactor(ctx context.Context, _ *sdk.CallToolRequest, in RefactorInput) (*sdk.CallToolResult, RefactorOutput, error) {
	op := strings.TrimSpace(in.Op)
	if op == "" {
		op = "rename_symbol"
	}
	if op != "rename_symbol" {
		return nil, RefactorOutput{Status: "error", Op: op,
			Hint: fmt.Sprintf("unsupported op %q — v1 supports only rename_symbol", op)}, nil
	}
	if strings.TrimSpace(in.Symbol) == "" || strings.TrimSpace(in.To) == "" {
		return nil, RefactorOutput{Status: "error", Op: op,
			Hint: "rename_symbol needs both `symbol` and `to`"}, nil
	}

	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, RefactorOutput{Status: "error", Op: op, Hint: hint}, nil
	}

	// No index required — PlanRename loads source directly via go/packages.
	res, err := refactor.PlanRename(ctx, p.Root, in.Symbol, in.To, in.Etag)
	if err != nil {
		return nil, RefactorOutput{Status: "error", Op: op, Project: p.Root, Hint: err.Error()}, nil
	}

	return nil, RefactorOutput{
		Status:   res.Status,
		Hint:     res.Hint,
		Project:  p.Root,
		Op:       op,
		From:     res.From,
		To:       res.To,
		Object:   res.Object,
		Edits:    res.Edits,
		Files:    res.Files,
		Etag:     res.Etag,
		Warnings: res.Warnings,
	}, nil
}
