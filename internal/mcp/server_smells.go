package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type SmellsInput struct {
	MinFuncLines         int    `json:"min_func_lines,omitempty" jsonschema:"minimum function body length to flag as long (default 80)"`
	MinFileSymbols       int    `json:"min_file_symbols,omitempty" jsonschema:"minimum symbols per file to flag as a god file (default 30)"`
	MinGodNodeCallers    int    `json:"min_god_node_callers,omitempty" jsonschema:"min in_degree to flag a function as a god-node (default 20)"`
	MinGodNodePkgCallers int    `json:"min_god_node_pkg_callers,omitempty" jsonschema:"min cross_pkg_callers to flag as a god-node (default 8)"`
	Limit                int    `json:"limit,omitempty" jsonschema:"max results per category (default 20)"`
	ProjectRoot          string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// SmellHit is one flagged symbol in the smells output.
type SmellHit struct {
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	Path          string `json:"path"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`
	Lines         int    `json:"lines,omitempty"`
}

// GodFileHit is one flagged file in the smells output.
type GodFileHit struct {
	Path        string `json:"path"`
	SymbolCount int    `json:"symbol_count"`
}

type SmellsOutput struct {
	Status        string       `json:"status"` // "ok" | "no-index" | "no-graph" | "error"
	Hint          string       `json:"hint,omitempty"`
	Project       string       `json:"project,omitempty"`
	LongFunctions []SmellHit   `json:"long_functions,omitempty"`
	DeadExports   []SmellHit   `json:"dead_exports,omitempty"`
	GodFiles      []GodFileHit `json:"god_files,omitempty"`
	// GodNodes are functions/methods with very high in-degree or cross-pkg
	// caller counts — over-coupled symbols constraining many callers.
	GodNodes []SmellHit `json:"god_nodes,omitempty"`
}

func (s *Server) Smells(ctx context.Context, in SmellsInput) (SmellsOutput, error) {
	_, out, err := s.smells(ctx, nil, in)
	return out, err
}

func (s *Server) smells(ctx context.Context, _ *sdk.CallToolRequest, in SmellsInput) (*sdk.CallToolResult, SmellsOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, SmellsOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, SmellsOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}

	minFuncLines := in.MinFuncLines
	if minFuncLines <= 0 {
		minFuncLines = 80
	}
	minFileSymbols := in.MinFileSymbols
	if minFileSymbols <= 0 {
		minFileSymbols = 30
	}
	minGodNodeCallers := in.MinGodNodeCallers
	if minGodNodeCallers <= 0 {
		minGodNodeCallers = 20
	}
	minGodNodePkgCallers := in.MinGodNodePkgCallers
	if minGodNodePkgCallers <= 0 {
		minGodNodePkgCallers = 8
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, SmellsOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	report, err := st.Smells(ctx, minFuncLines, minFileSymbols, minGodNodeCallers, minGodNodePkgCallers, limit)
	if err != nil {
		return nil, SmellsOutput{Status: "error", Hint: fmt.Sprintf("smells: %v", err)}, nil
	}

	out := SmellsOutput{Status: "ok", Project: p.Root}

	for _, sym := range report.LongFunctions {
		out.LongFunctions = append(out.LongFunctions, SmellHit{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			Path:          sym.FilePath,
			StartLine:     sym.StartLine,
			EndLine:       sym.EndLine,
			Lines:         sym.Lines,
		})
	}
	for _, sym := range report.DeadExports {
		out.DeadExports = append(out.DeadExports, SmellHit{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			Path:          sym.FilePath,
			StartLine:     sym.StartLine,
			EndLine:       sym.EndLine,
			Lines:         sym.Lines,
		})
	}
	for _, f := range report.GodFiles {
		out.GodFiles = append(out.GodFiles, GodFileHit{
			Path:        f.FilePath,
			SymbolCount: f.SymbolCount,
		})
	}
	for _, sym := range report.GodNodes {
		out.GodNodes = append(out.GodNodes, SmellHit{
			QualifiedName: sym.QualifiedName,
			Kind:          sym.Kind,
			Path:          sym.FilePath,
			StartLine:     sym.StartLine,
			EndLine:       sym.EndLine,
		})
	}

	if len(out.LongFunctions) == 0 && len(out.DeadExports) == 0 && len(out.GodFiles) == 0 && len(out.GodNodes) == 0 {
		out.Hint = "no graph nodes indexed — run `dex index . --graph=only` to extract the call graph first."
		out.Status = "no-graph"
	}
	return nil, out, nil
}
