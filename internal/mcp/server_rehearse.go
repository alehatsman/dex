package mcp

import (
	"context"

	"github.com/alehatsman/dex/internal/rehearse"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── verb: rehearse ────────────────────────────────────────────────────────
//
// rehearse (#730) type-checks a hypothetical edit via go/packages Overlay.
// dex is read-only (#551): rehearse NEVER writes files — edits are applied
// in-memory and new type errors + broken files + tests are returned.
// Closes the chain: refactor (plan) → rehearse (prove compiles) → Edit (apply) → verify (test).
// v1 is Go-only (on-demand packages.Load, no index needed).

// RehearseEdit is one byte-range splice — same shape as a refactor EditTriple.
type RehearseEdit struct {
	Path        string `json:"path"`       // project-relative, slash-separated
	StartByte   int    `json:"start_byte"` // 0-based, inclusive
	EndByte     int    `json:"end_byte"`   // 0-based, exclusive
	Replacement string `json:"replacement"`
}

// RehearseFile is a whole-file replacement in the overlay.
type RehearseFile struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

// RehearseInput is the JSON schema for the rehearse MCP tool.
type RehearseInput struct {
	// Edits are byte-range splices (same shape as refactor output). Applied highest-offset-first per file.
	Edits []RehearseEdit `json:"edits,omitempty"`
	// Files replaces whole files in the overlay. Takes precedence over Edits for the same path.
	Files       []RehearseFile `json:"files,omitempty"`
	ProjectRoot string         `json:"project_root,omitempty"`
}

// RehearseOutput is the rehearsal result.
type RehearseOutput struct {
	Status      string                `json:"status"` // ok | error | unsupported-language | no-edits
	Hint        string                `json:"hint,omitempty"`
	Compiles    bool                  `json:"compiles"`
	Diagnostics []rehearse.Diagnostic `json:"diagnostics,omitempty"`
	BrokenFiles []string              `json:"broken_files,omitempty"`
	TestsToRun  []string              `json:"tests_to_run,omitempty"`
	OverlayEtag string                `json:"overlay_etag,omitempty"`
}

// Rehearse runs the rehearse verb without an SDK request — for the REST route.
func (s *Server) Rehearse(ctx context.Context, in RehearseInput) (RehearseOutput, error) {
	_, out, err := s.rehearse(ctx, nil, in)
	return out, err
}

func (s *Server) rehearse(ctx context.Context, _ *sdk.CallToolRequest, in RehearseInput) (*sdk.CallToolResult, RehearseOutput, error) {
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, RehearseOutput{Status: "error", Hint: hint}, nil
	}

	rin := rehearse.Input{}
	for _, e := range in.Edits {
		rin.Edits = append(rin.Edits, rehearse.EditTriple{Path: e.Path, StartByte: e.StartByte, EndByte: e.EndByte, Replacement: e.Replacement})
	}
	for _, f := range in.Files {
		rin.Files = append(rin.Files, rehearse.WholeFile{Path: f.Path, Contents: f.Contents})
	}

	res, err := rehearse.Rehearse(ctx, p.Root, rin)
	if err != nil {
		return nil, RehearseOutput{Status: "error", Hint: err.Error()}, nil
	}

	return nil, RehearseOutput{
		Status:      res.Status,
		Hint:        res.Hint,
		Compiles:    res.Compiles,
		Diagnostics: res.Diagnostics,
		BrokenFiles: res.BrokenFiles,
		TestsToRun:  res.TestsToRun,
		OverlayEtag: res.OverlayEtag,
	}, nil
}
