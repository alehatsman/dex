package mcp

// Structural facet of the grep lane: match by AST shape via a raw
// tree-sitter query instead of RE2 line text. See specs/238-structural-
// query-grep.md for the design. No new dependency — reuses the tree-sitter
// grammars and github.com/smacker/go-tree-sitter's Query API already
// vendored for the graph extractors (internal/graph/sitter*.go); this
// package did not previously import tree-sitter at all.
//
// Cost model deliberately mirrors internal/graph's extractor framework:
// every call parses each candidate file fresh (no cross-call cache), one
// *sitter.Parser per file, tree closed immediately after use.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/throttle"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// structuralLang pairs a grammar with the file extensions it parses.
// Mirrors the extractor registrations in internal/graph/sitter_jsts_tags.go
// (TS/TSX are distinct grammars — the plain TS grammar mislexes JSX, #232)
// and the single-extension languages in sitter_python.go/_java.go/_rust.go.
type structuralLang struct {
	language *sitter.Language
	exts     []string
}

var structuralLangs = map[string]structuralLang{
	"python":     {python.GetLanguage(), []string{".py"}},
	"javascript": {javascript.GetLanguage(), []string{".js", ".jsx"}},
	"typescript": {typescript.GetLanguage(), []string{".ts"}},
	"tsx":        {tsx.GetLanguage(), []string{".tsx"}},
	"rust":       {rust.GetLanguage(), []string{".rs"}},
	"java":       {java.GetLanguage(), []string{".java"}},
}

func structuralLangNames() string {
	names := make([]string, 0, len(structuralLangs))
	for name := range structuralLangs {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// structuralPredicates are the only predicate operators
// smacker/go-tree-sitter's QueryCursor.FilterPredicates actually evaluates.
// set!/is?/is-not? compile (NewQuery only validates arity/type) but are
// never enforced — FilterPredicates has no case for them, so a match with
// one of those predicates is silently accepted as if it always passed.
// Rejecting them at compile time trades "some valid tree-sitter queries
// are refused" for "never a silent false match" (see spec, open question 1).
var structuralPredicates = map[string]bool{
	"eq?":        true,
	"not-eq?":    true,
	"match?":     true,
	"not-match?": true,
}

// structuralMaxFileSize mirrors internal/graph's maxParseFileSize — the
// same bound the graph extractors use before handing a file to the parser.
const structuralMaxFileSize = 1 << 20

func (s *Server) searchGrepStructural(ctx context.Context, p *proj.Project, in SearchGrepInput) (*sdk.CallToolResult, SearchGrepOutput, error) {
	if in.Lang == "" {
		return nil, SearchGrepOutput{Status: "error",
			Hint: fmt.Sprintf("lang is required with query — one of: %s", structuralLangNames())}, nil
	}
	lang, ok := structuralLangs[in.Lang]
	if !ok {
		return nil, SearchGrepOutput{Status: "error",
			Hint: fmt.Sprintf("unsupported lang %q — one of: %s", in.Lang, structuralLangNames())}, nil
	}

	query, err := sitter.NewQuery([]byte(in.Query), lang.language)
	if err != nil {
		return nil, SearchGrepOutput{Status: "error", Hint: fmt.Sprintf("invalid query: %v", err)}, nil
	}
	defer query.Close()
	if op, ok := firstUnsupportedPredicate(query); ok {
		return nil, SearchGrepOutput{Status: "error",
			Hint: fmt.Sprintf("predicate #%s is parsed but not enforced by dex's tree-sitter engine — supported: eq?, not-eq?, match?, not-match?", op)}, nil
	}

	maxResults := in.MaxResults
	if maxResults <= 0 || maxResults > 200 {
		maxResults = 50
	}
	ctxN := in.Context
	if ctxN < 0 {
		ctxN = 0
	}
	if ctxN > 10 {
		ctxN = 10
	}

	prefix := strings.Trim(in.Path, "/")
	if prefix == "." {
		prefix = ""
	}
	filePaths, early := s.grepFileList(p, prefix, "")
	if early != nil {
		return nil, *early, nil
	}
	filePaths = filterByExt(filePaths, lang.exts)
	if extFilter := strings.TrimPrefix(in.Ext, "."); extFilter != "" {
		filePaths = filterByExt(filePaths, []string{"." + extFilter})
	}

	var matches []GrepMatch
	truncated := false
outer:
	for _, absPath := range filePaths {
		info, statErr := os.Stat(absPath)
		if statErr != nil || info.Size() > structuralMaxFileSize {
			continue
		}
		data, readErr := os.ReadFile(absPath)
		if readErr != nil {
			continue
		}
		relPath, _ := filepath.Rel(p.Root, absPath)

		parser := sitter.NewParser()
		parser.SetLanguage(lang.language)
		tree, parseErr := parser.ParseCtx(ctx, nil, data)
		if parseErr != nil {
			continue
		}

		lines := strings.Split(string(data), "\n")
		qc := sitter.NewQueryCursor()
		qc.Exec(query, tree.RootNode())
		for {
			m, ok := qc.NextMatch()
			if !ok {
				break
			}
			m = qc.FilterPredicates(m, data)
			if len(m.Captures) == 0 {
				continue
			}
			row := int(m.Captures[0].Node.StartPoint().Row)
			if row < 0 || row >= len(lines) {
				continue
			}
			matches = append(matches, newGrepMatch(lines, row, relPath, ctxN))
			if len(matches) >= maxResults {
				truncated = true
				break
			}
		}
		qc.Close()
		tree.Close()
		if truncated {
			break outer
		}
	}

	out := s.finishGrepOutput(p, matches, truncated, maxResults, throttle.ArgsKey(in.Query), true)
	return nil, out, nil
}

// firstUnsupportedPredicate scans every pattern in query for a predicate
// operator FilterPredicates doesn't enforce (see structuralPredicates).
func firstUnsupportedPredicate(query *sitter.Query) (string, bool) {
	for i := uint32(0); i < query.PatternCount(); i++ {
		for _, steps := range query.PredicatesForPattern(i) {
			if len(steps) == 0 {
				continue
			}
			op := query.StringValueForId(steps[0].ValueId)
			if !structuralPredicates[op] {
				return op, true
			}
		}
	}
	return "", false
}

// filterByExt keeps paths whose extension is in exts.
func filterByExt(paths []string, exts []string) []string {
	filtered := paths[:0]
	for _, path := range paths {
		for _, ext := range exts {
			if strings.HasSuffix(path, ext) {
				filtered = append(filtered, path)
				break
			}
		}
	}
	return filtered
}
