package mcp

import (
	"context"
	"strings"

	"github.com/alehatsman/dex/internal/symbols"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── verb: refs ───────────────────────────────────────────────────────────
//
// refs (#604 Tier 1) provides type-precise Go symbol queries via go/types:
// find all references, concrete implementations of an interface, supertypes
// (embedded interfaces / interfaces satisfied by a type), and subtypes
// (implementing types / embedding structs).
//
// It sits in the analysis/power lane (DEX_EXPERT) next to cohort/smells.
// Non-Go files return status "unsupported-language".

// RefsInput drives the refs verb.
type RefsInput struct {
	// Action is one of: references, implementations, supertypes, subtypes.
	Action string `json:"action" jsonschema:"one of: references (all use+def sites), implementations (concrete types satisfying an interface), supertypes (embedded interfaces / interfaces a type satisfies), subtypes (implementing types or structs embedding this type)"`
	// Symbol is a bare name, (*Recv).Method, or pkg.Name.
	Symbol      string `json:"symbol" jsonschema:"symbol to query: bare name ('Foo'), receiver-qualified ('(*Server).Run'), or package-tail-qualified ('mcp.NewServer')"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
}

// RefsOutput is the query result.
type RefsOutput struct {
	Status  string         `json:"status"` // "ok" | "unsupported-language" | "not-found" | "error"
	Hint    string         `json:"hint,omitempty"`
	Symbol  string         `json:"symbol,omitempty"`
	Action  string         `json:"action,omitempty"`
	Project string         `json:"project,omitempty"`
	Sites   []symbols.Site `json:"sites,omitempty"`
}

// Refs runs the refs verb without an SDK request — the REST `/refs` route.
func (s *Server) Refs(ctx context.Context, in RefsInput) (RefsOutput, error) {
	_, out, err := s.refs(ctx, nil, in)
	return out, err
}

func (s *Server) refs(ctx context.Context, _ *sdk.CallToolRequest, in RefsInput) (*sdk.CallToolResult, RefsOutput, error) {
	if strings.TrimSpace(in.Symbol) == "" {
		return nil, RefsOutput{Status: "error", Hint: "refs needs a `symbol` name"}, nil
	}
	action := symbols.Action(strings.TrimSpace(in.Action))
	switch action {
	case symbols.References, symbols.Implementations, symbols.Supertypes, symbols.Subtypes:
	case "":
		action = symbols.References
	default:
		return nil, RefsOutput{
			Status: "error",
			Hint:   "refs `action` must be one of: references, implementations, supertypes, subtypes",
		}, nil
	}

	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, RefsOutput{Status: "error", Hint: hint}, nil
	}

	res, err := symbols.Query(ctx, p.Root, action, in.Symbol)
	if err != nil {
		return nil, RefsOutput{Status: "error", Hint: err.Error()}, nil
	}
	return nil, RefsOutput{
		Status:  res.Status,
		Hint:    res.Hint,
		Symbol:  res.Symbol,
		Action:  res.Action,
		Project: res.Project,
		Sites:   res.Sites,
	}, nil
}
