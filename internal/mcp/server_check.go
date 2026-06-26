package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// CheckInput is the input for the check verb — a batch of file:line[:symbol] claims.
type CheckInput struct {
	ProjectRoot string     `json:"project_root"`
	Claims      []ClaimRef `json:"claims"`
}

// ClaimRef is one ref claim to verify.
type ClaimRef struct {
	// Ref is a "file:line" or "file:line:symbol" or bare "file" string.
	Ref string `json:"ref"`
	// Symbol is the expected symbol at that line (optional; an inline :symbol
	// suffix in Ref wins when both are present).
	Symbol string `json:"symbol,omitempty"`
}

// CheckOutput is the result of a batch ref-verification.
type CheckOutput struct {
	Results []ClaimResult `json:"results"`
}

// ClaimResult is the verdict for one ClaimRef.
type ClaimResult struct {
	// Ref is the original ref string.
	Ref string `json:"ref"`
	// Status is one of: ok, moved, gone, no_file, parse_error.
	Status string `json:"status"`
	// SymbolAt is what is actually indexed at the given file:line (if any).
	SymbolAt string `json:"symbol_at,omitempty"`
	// FoundAt is where the expected symbol was found when Status=="moved".
	FoundAt string `json:"found_at,omitempty"`
}

// Check is the public entry point for the check verb (used by the CLI).
func (s *Server) Check(ctx context.Context, in CheckInput) (CheckOutput, error) {
	_, out, err := s.check(ctx, nil, in)
	return out, err
}

func (s *Server) check(ctx context.Context, _ *sdk.CallToolRequest, in CheckInput) (*sdk.CallToolResult, CheckOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, CheckOutput{}, errors.New(hint)
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, CheckOutput{}, fmt.Errorf("no index for %s — run `dex index %s` first", p.Root, p.Root)
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, CheckOutput{}, fmt.Errorf("open index: %w", err)
	}

	out := CheckOutput{Results: make([]ClaimResult, 0, len(in.Claims))}
	for i, c := range in.Claims {
		if i >= maxClaims {
			out.Results = append(out.Results, ClaimResult{Ref: c.Ref, Status: "parse_error"})
			continue
		}
		c2 := ClaimRef{Ref: normalizeClaimRef(p.Root, c.Ref), Symbol: c.Symbol}
		r := verifyOneClaim(ctx, st, c2)
		r.Ref = c.Ref
		out.Results = append(out.Results, r)
	}
	return nil, out, nil
}

// verifyOneClaim resolves a single ClaimRef against the current index.
func verifyOneClaim(ctx context.Context, st *store.Store, c ClaimRef) ClaimResult {
	path, line, sym := parseClaimRef(c)
	res := ClaimResult{Ref: c.Ref}

	if path == "" {
		res.Status = "parse_error"
		return res
	}

	// 1. Check if the file has any indexed content.
	indexed, err := st.PathIndexed(ctx, path)
	if err != nil || !indexed {
		res.Status = "no_file"
		return res
	}

	if line <= 0 {
		// No line given: just confirm the file is indexed.
		res.Status = "ok"
		return res
	}

	// 2. Resolve the enclosing symbol at file:line.
	hit, err := st.ChunkAt(ctx, path, line)
	if err != nil {
		// No chunk covers that line — file exists but line is in an unindexed gap.
		if sym != "" {
			if found := findSymbolInFile(ctx, st, path, sym); found != "" {
				res.Status = "moved"
				res.FoundAt = found
				return res
			}
		}
		res.Status = "gone"
		return res
	}
	res.SymbolAt = hit.Name

	if sym == "" {
		res.Status = "ok"
		return res
	}

	// 3. Check if the indexed symbol matches the expected one.
	if symbolsMatch(hit.Name, sym) {
		res.Status = "ok"
		return res
	}

	// Symbol mismatch — the expected symbol may have moved within this file.
	if found := findSymbolInFile(ctx, st, path, sym); found != "" {
		res.Status = "moved"
		res.FoundAt = found
		return res
	}
	res.Status = "gone"
	return res
}

// parseClaimRef parses "path:line:symbol", "path:line", or "path" from c.Ref.
// An inline :symbol wins over c.Symbol when both are set.
func parseClaimRef(c ClaimRef) (path string, line int, sym string) {
	ref := c.Ref
	parts := strings.Split(ref, ":")
	switch len(parts) {
	case 1:
		path = filepath.ToSlash(parts[0])
		sym = c.Symbol
	case 2:
		path = filepath.ToSlash(parts[0])
		n, err := strconv.Atoi(parts[1])
		if err == nil {
			line = n
		} else {
			sym = parts[1]
		}
		if sym == "" {
			sym = c.Symbol
		}
	default:
		// "path:line:symbol" or longer — walk backwards: first non-int from right is symbol.
		i := len(parts) - 1
		if _, err := strconv.Atoi(parts[i]); err != nil {
			sym = parts[i]
			i--
		} else {
			sym = c.Symbol
		}
		if i >= 1 {
			if n, err := strconv.Atoi(parts[i]); err == nil {
				line = n
				i--
			}
		}
		path = filepath.ToSlash(strings.Join(parts[:i+1], ":"))
	}
	return path, line, sym
}

// symbolsMatch checks whether the indexed name matches the expected symbol.
func symbolsMatch(indexed, expected string) bool {
	if indexed == expected {
		return true
	}
	// Strip receiver/qualifier: "(*Server).Check" → "Check"
	if dot := strings.LastIndex(expected, "."); dot >= 0 {
		if indexed == expected[dot+1:] {
			return true
		}
	}
	if dot := strings.LastIndex(indexed, "."); dot >= 0 {
		if indexed[dot+1:] == expected {
			return true
		}
	}
	return false
}

// findSymbolInFile looks for sym in path via a direct chunk-name scan.
// Returns "file:line" if found, empty string otherwise.
func findSymbolInFile(ctx context.Context, st *store.Store, path, sym string) string {
	hits, err := st.FindSymbolInPath(ctx, path, sym)
	if err != nil || len(hits) == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d", hits[0].Path, hits[0].StartLine)
}
