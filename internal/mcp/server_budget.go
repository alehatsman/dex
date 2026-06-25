package mcp

import (
	"context"
	"fmt"

	"github.com/alehatsman/dex/internal/heatmap"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: budget ─────────────────────────────────────────────────────────
//
// Per-session context budget report: total tokens emitted across MCP tool
// calls, tool / shell call counts, top files by tokens (heatmap), and any
// active SLO violations. Builds entirely on existing counters
// (slo.Tracker + heatmap.Heatmap) — no new accounting introduced (#33).
//
// MCP-only: a CLI invocation has no agent session and no per-session counters,
// so the parity allow-list (cmd/dex/verb_parity_test.go) lists `budget` next
// to `session`.

type BudgetInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	TopN        int    `json:"top_n,omitempty"        jsonschema:"max top-files entries (default 10, max 50)"`
}

// BudgetFile is one entry in the top-files-by-tokens table.
type BudgetFile struct {
	Path           string `json:"path"`
	AccessCount    int    `json:"access_count"`
	OriginalTokens int    `json:"original_tokens"` // before compression
	SavedTokens    int    `json:"saved_tokens"`    // reclaimed by compression
	NetTokens      int    `json:"net_tokens"`      // original − saved: what hit the wire
	LastAccessed   string `json:"last_accessed,omitempty"`
}

// BudgetViolation is a flattened slo.Violation view (no embed; keeps the JSON
// stable independent of slo.SLOEntry layout changes).
type BudgetViolation struct {
	Name    string  `json:"name"`
	Metric  string  `json:"metric"`
	Action  string  `json:"action"`
	Current float64 `json:"current"`
	Limit   float64 `json:"limit"`
}

type BudgetOutput struct {
	Project       string            `json:"project,omitempty"`
	Status        string            `json:"status"` // "ok" | "error"
	ContextTokens uint64            `json:"context_tokens"`
	ToolCalls     uint64            `json:"tool_calls"`
	ShellCalls    uint64            `json:"shell_calls"`
	TopFiles      []BudgetFile      `json:"top_files,omitempty"`
	Violations    []BudgetViolation `json:"violations,omitempty"`
	Hint          string            `json:"hint,omitempty"`
	Error         string            `json:"error,omitempty"`
}

const (
	budgetDefaultTopN = 10
	budgetMaxTopN     = 50
)

func (s *Server) budget(_ context.Context, _ *sdk.CallToolRequest, in BudgetInput) (*sdk.CallToolResult, BudgetOutput, error) {
	p, herr := s.resolveProject(in.ProjectRoot)
	if herr != "" {
		return nil, BudgetOutput{Status: "error", Error: herr}, nil
	}

	out := BudgetOutput{Project: p.Root, Status: "ok"}
	tr := s.sloFor(p.Root)
	snap := tr.Snapshot()
	out.ContextTokens = snap.ContextTokens
	out.ToolCalls = snap.ToolCalls
	out.ShellCalls = snap.ShellCalls

	topN := in.TopN
	switch {
	case topN <= 0:
		topN = budgetDefaultTopN
	case topN > budgetMaxTopN:
		topN = budgetMaxTopN
	}

	hm := heatmap.Load(p.CacheDir)
	for _, e := range topFilesByNetTokens(hm, topN) {
		out.TopFiles = append(out.TopFiles, BudgetFile{
			Path:           e.Path,
			AccessCount:    e.AccessCount,
			OriginalTokens: e.OriginalTotal,
			SavedTokens:    e.TotalSaved,
			NetTokens:      e.OriginalTotal - e.TotalSaved,
			LastAccessed:   e.LastAccessed,
		})
	}

	if vs := tr.Check(); len(vs) > 0 {
		out.Violations = make([]BudgetViolation, 0, len(vs))
		for _, v := range vs {
			out.Violations = append(out.Violations, BudgetViolation{
				Name:    v.SLO.Name,
				Metric:  v.SLO.Metric,
				Action:  string(v.SLO.Action),
				Current: v.Current,
				Limit:   v.Limit,
			})
		}
	}

	out.Hint = budgetAdvice(out)
	return nil, out, nil
}

// topFilesByNetTokens returns the n entries with the largest net token
// footprint (original − saved). Ranking by net rather than access count
// surfaces the files that are actually consuming the budget, not just the
// most-touched ones.
func topFilesByNetTokens(hm *heatmap.Heatmap, n int) []*heatmap.Entry {
	all := hm.TopFiles(0) // 0 = no cap, all entries
	// Sort ranking by net tokens descending; ties broken by path for stability.
	for i := 1; i < len(all); i++ {
		j := i
		for j > 0 && netTokens(all[j]) > netTokens(all[j-1]) {
			all[j], all[j-1] = all[j-1], all[j]
			j--
		}
	}
	if n > 0 && len(all) > n {
		all = all[:n]
	}
	return all
}

func netTokens(e *heatmap.Entry) int { return e.OriginalTotal - e.TotalSaved }

// budgetAdvice produces a short, context-sensitive hint. Silent when there's
// nothing actionable — the agent should not be nudged on every poll.
func budgetAdvice(out BudgetOutput) string {
	for _, v := range out.Violations {
		if v.Action == "block" {
			return fmt.Sprintf("SLO %q is blocking; use session action=clear or split work", v.Name)
		}
	}
	if len(out.TopFiles) > 0 && out.TopFiles[0].NetTokens > 20_000 {
		return fmt.Sprintf("%s dominates the budget (%d net tokens) — consider mode=signatures or lines:N-M",
			out.TopFiles[0].Path, out.TopFiles[0].NetTokens)
	}
	return ""
}
