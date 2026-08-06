package mcp

import (
	"context"
	"strings"

	"github.com/alehatsman/dex/internal/cohesion"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── verb: cohort ─────────────────────────────────────────────────────────
//
// cohort (#643) reports the blast radius of an *intent* rather than a symbol:
// the set of types you must edit in lockstep when an interface's method set
// changes. v1 = interface cohesion — given an interface, list every type that
// implements it (complete) plus near-misses that implement most of it (the
// backend you forgot to update). Pure go/packages + go/types, no index needed.
//
// It sits in the analysis/power lane (DEX_EXPERT) next to smells/routes/
// clusters: an occasional planning aid, not an everyday navigation verb.

// CohortInput drives the cohort verb.
type CohortInput struct {
	Interface   string `json:"interface" jsonschema:"the interface to analyse: bare ('toolSurface') or package-tail-qualified ('mcp.toolSurface')"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
}

// CohortOutput mirrors cohesion.CohortResult.
type CohortOutput struct {
	Status    string            `json:"status"` // ok | unsupported-language | not-found | error
	Hint      string            `json:"hint,omitempty"`
	Project   string            `json:"project,omitempty"`
	Interface string            `json:"interface,omitempty"`
	Methods   []string          `json:"methods,omitempty"`
	Members   []cohesion.Member `json:"members,omitempty"`
	Complete  int               `json:"complete"`
	Partial   int               `json:"partial"`
}

// Cohort runs the cohort verb without an SDK request — the REST `/cohort` route.
func (s *Server) Cohort(ctx context.Context, in CohortInput) (CohortOutput, error) {
	_, out, err := s.cohort(ctx, nil, in)
	return out, err
}

func (s *Server) cohort(ctx context.Context, _ *sdk.CallToolRequest, in CohortInput) (*sdk.CallToolResult, CohortOutput, error) {
	if strings.TrimSpace(in.Interface) == "" {
		return nil, CohortOutput{Status: "error", Hint: "cohort needs an `interface` name"}, nil
	}
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, CohortOutput{Status: "error", Hint: hint}, nil
	}

	res, err := cohesion.ImplementorsOf(ctx, p.Root, in.Interface)
	if err != nil {
		return nil, CohortOutput{Status: "error", Project: p.Root, Hint: err.Error()}, nil
	}
	return nil, CohortOutput{
		Status:    res.Status,
		Hint:      res.Hint,
		Project:   p.Root,
		Interface: res.Interface,
		Methods:   res.Methods,
		Members:   res.Members,
		Complete:  res.Complete,
		Partial:   res.Partial,
	}, nil
}
