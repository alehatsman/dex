package mcp

// search_context — single-call search+signatures+body tool (N15).
// inlineTaskSymbol / tokenizeWords / symbolQueryScore — shared helpers
// used by compose and by the N16 task-relevance inline in file_view.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type ComposeInput struct {
	Query       string `json:"query"                  jsonschema:"query describing what you're looking for"`
	K           int    `json:"k,omitempty"            jsonschema:"top files to surface (default 3, max 5)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute project root; defaults to server working directory"`
}

type ComposeFile struct {
	Path       string  `json:"path"`
	Score      float64 `json:"score"`
	Signatures string  `json:"signatures,omitempty"`
}

type ComposeBestSymbol struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Body      string `json:"body"`
}

type ComposeOutput struct {
	Status     string             `json:"status"` // "ok" | "no-index" | "embedding-service-unreachable" | "error"
	Hint       string             `json:"hint,omitempty"`
	Project    string             `json:"project,omitempty"`
	Query      string             `json:"query,omitempty"`
	Files      []ComposeFile      `json:"files,omitempty"`
	BestSymbol *ComposeBestSymbol `json:"best_symbol,omitempty"`
}

func (s *Server) Compose(ctx context.Context, in ComposeInput) (ComposeOutput, error) {
	_, out, err := s.compose(ctx, nil, in)
	return out, err
}

func (s *Server) compose(ctx context.Context, _ *sdk.CallToolRequest, in ComposeInput) (*sdk.CallToolResult, ComposeOutput, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, ComposeOutput{Status: "error", Hint: "query is empty"}, nil
	}
	k := in.K
	if k <= 0 {
		k = 3
	}
	if k > 5 {
		k = 5
	}

	root := in.ProjectRoot
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, ComposeOutput{Status: "error", Hint: "could not determine project root; pass project_root explicitly"}, nil
		}
		root = wd
	}
	p, err := proj.Resolve(root, s.IndexDir)
	if err != nil {
		return nil, ComposeOutput{Status: "error", Hint: fmt.Sprintf("resolve project: %v", err)}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, ComposeOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}

	vecs, err := s.EmbedClient.Embed(ctx, []string{in.Query})
	if err != nil {
		if errors.Is(err, embed.ErrUnreachable) {
			return nil, ComposeOutput{Status: "embedding-service-unreachable", Project: p.Root,
				Hint: "embedding service offline — fall back to search_symbol or ask"}, nil
		}
		return nil, ComposeOutput{Status: "error", Hint: fmt.Sprintf("embed: %v", err)}, nil
	}
	vec := vecs[0]

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, ComposeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	hits, err := st.Search(ctx, vec, in.Query, k*15)
	if err != nil {
		return nil, ComposeOutput{Status: "error", Hint: fmt.Sprintf("search: %v", err)}, nil
	}

	// Aggregate max score per file.
	fileScore := make(map[string]float64, len(hits))
	for _, h := range hits {
		if h.Path == "" {
			continue
		}
		if sc := float64(h.Score); sc > fileScore[h.Path] {
			fileScore[h.Path] = sc
		}
	}
	type ranked struct {
		path  string
		score float64
	}
	files := make([]ranked, 0, len(fileScore))
	for path, score := range fileScore {
		files = append(files, ranked{path, score})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].score != files[j].score {
			return files[i].score > files[j].score
		}
		return files[i].path < files[j].path
	})
	if len(files) > k {
		files = files[:k]
	}

	queryTokens := tokenizeWords(in.Query)
	out := ComposeOutput{
		Status:  "ok",
		Project: p.Root,
		Query:   in.Query,
		Files:   make([]ComposeFile, 0, len(files)),
	}

	type candidate struct {
		sym   store.GraphSymbol
		path  string
		score int
	}
	var best candidate

	for _, f := range files {
		syms, _ := st.SymbolsByFile(ctx, f.path)
		absPath := filepath.Join(p.Root, f.path)
		data, _ := os.ReadFile(absPath)

		var sigs string
		if len(syms) > 0 {
			sigs = formatSignatures(data, syms, f.path)
		} else if content, ok := nonCodeMap(f.path, data); ok {
			sigs = content
		}
		out.Files = append(out.Files, ComposeFile{
			Path:       f.path,
			Score:      math.Round(f.score*1000) / 1000,
			Signatures: sigs,
		})

		for _, sym := range syms {
			if sc := symbolQueryScore(queryTokens, sym); sc > best.score {
				best = candidate{sym, f.path, sc}
			}
		}
	}

	if best.score > 0 {
		absPath := filepath.Join(p.Root, best.path)
		if data, err := os.ReadFile(absPath); err == nil {
			endLine := best.sym.EndLine
			if endLine-best.sym.StartLine > 80 {
				endLine = best.sym.StartLine + 79
			}
			body, sLine, eLine := sliceLines(data, best.sym.StartLine, endLine)
			if len(body) > 0 {
				out.BestSymbol = &ComposeBestSymbol{
					Path:      best.path,
					Name:      best.sym.QualifiedName,
					Kind:      best.sym.Kind,
					StartLine: sLine,
					EndLine:   eLine,
					Body:      string(body),
				}
			}
		}
	}

	return nil, out, nil
}

// inlineTaskSymbol appends the body of the best task-matching symbol to
// content when the current session has a declared task. No-op if the session
// is absent, the task is empty, or no symbol overlaps with the task tokens.
func inlineTaskSymbol(ctx context.Context, st *store.Store, data []byte, syms []store.GraphSymbol, content string) string {
	sess, ok, err := st.SessionGet(ctx)
	if err != nil || !ok || sess.Task == "" {
		return content
	}
	queryTokens := tokenizeWords(sess.Task)
	if len(queryTokens) == 0 {
		return content
	}
	var bestSym store.GraphSymbol
	bestScore := 0
	for _, sym := range syms {
		if sc := symbolQueryScore(queryTokens, sym); sc > bestScore {
			bestScore = sc
			bestSym = sym
		}
	}
	if bestScore == 0 || data == nil {
		return content
	}
	endLine := bestSym.EndLine
	if endLine-bestSym.StartLine > 60 {
		endLine = bestSym.StartLine + 59
	}
	body, sLine, eLine := sliceLines(data, bestSym.StartLine, endLine)
	if len(body) == 0 {
		return content
	}
	return content + fmt.Sprintf("\n# Task-relevant: %s %s (lines %d-%d)\n```\n%s```\n",
		bestSym.Kind, bestSym.QualifiedName, sLine, eLine, string(body))
}

// symbolQueryScore returns the word-overlap count between query tokens and a
// symbol's qualified name tokens. 0 means no overlap.
func symbolQueryScore(queryTokens []string, sym store.GraphSymbol) int {
	symTokens := tokenizeWords(sym.QualifiedName)
	score := 0
	for _, qt := range queryTokens {
		for _, st := range symTokens {
			if qt == st {
				score++
			}
		}
	}
	return score
}

// tokenizeWords splits text into lowercase tokens (length > 2) breaking on
// non-alphanumeric characters and camelCase boundaries.
func tokenizeWords(s string) []string {
	var tokens []string
	var cur strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			cur.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			if cur.Len() > 2 {
				tokens = append(tokens, cur.String())
			}
			cur.Reset()
			cur.WriteRune(r + 32) // toLower
		case r >= '0' && r <= '9':
			cur.WriteRune(r)
		default:
			if cur.Len() > 2 {
				tokens = append(tokens, cur.String())
			}
			cur.Reset()
		}
	}
	if cur.Len() > 2 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}
