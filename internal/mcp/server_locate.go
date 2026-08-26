package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/output"
	"github.com/alehatsman/dex/internal/retrieve"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── verb: locate ─────────────────────────────────────────────────────────
//
// locate (#636 / GitHub #65 Tier S1) is one-call orientation around a code
// location. It collapses the 4–6 call dance every session repeats — resolve
// the symbol, walk its callers, find sibling tests, find the nearest doc,
// check recent churn — into a single response. It is pure composition over
// shipped infrastructure: ChunkAt / FindSymbol for resolution, traceVerb for
// callers, the Enricher legs for tests / nearest doc / git blame. No new store
// schema, no new backend, no model call.

// LocateInput selects the target three ways. Exactly one of Ref / Symbol /
// Frame is expected; when more than one is set the precedence is Ref, then
// Symbol, then Frame. (Batch verification of many cited locations lives in the
// `check` verb, #708/#787 — not here.)
type LocateInput struct {
	Ref         string `json:"ref,omitempty" jsonschema:"a code location as 'path:line' (or 'path:line:col'); path may be project-relative or absolute"`
	Symbol      string `json:"symbol,omitempty" jsonschema:"a symbol name: bare ('Foo'), receiver-qualified ('(*Server).Run'), or package-tail-qualified ('mcp.NewServer')"`
	Frame       string `json:"frame,omitempty" jsonschema:"a raw stack-trace frame line; the file:line or symbol is parsed out of it"`
	Issues      bool   `json:"issues,omitempty" jsonschema:"opt-in: also list related open GitHub issues via the 'gh' CLI (best-effort, skipped when gh is absent)"`
	K           int    `json:"k,omitempty" jsonschema:"max callers and related notes to return (default 8, max 30)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
}

// LocateOutput is the orientation bundle. Beyond the resolved target every
// field is best-effort: an empty list or string means the lane found nothing
// (or degraded — e.g. callers is empty when the graph isn't indexed), never
// an error. The caller reads what's present and ignores the rest.
type LocateOutput struct {
	Status  string `json:"status"` // "ok" | "no-index" | "not-found" | "error"
	Hint    string `json:"hint,omitempty"`
	Project string `json:"project,omitempty"`

	// Resolved target.
	Path      string `json:"path,omitempty"`
	Symbol    string `json:"symbol,omitempty"`
	Kind      string `json:"kind,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`

	// Composed lanes.
	Callers    []CallSite    `json:"callers"`
	Risk       string        `json:"risk,omitempty"`
	Tests      []string      `json:"tests,omitempty"`
	NearestDoc string        `json:"nearest_doc,omitempty"`
	LastCommit string        `json:"last_commit,omitempty"`
	LastAuthor string        `json:"last_author,omitempty"`
	Issues     []string      `json:"issues,omitempty"`

	// Shared machine-readable contract (#816): confidence in the resolution,
	// the line-level evidence span, index staleness, blast-radius risk flags,
	// and suggested follow-ups — finalized on every return path.
	output.Envelope
}

// finalizeLocateEnvelope fills the shared envelope from the resolved bundle and
// the index's last-indexed time. It runs on every return path (deferred), so an
// error / no-index / not-found response still carries a valid, low-confidence
// contract rather than a zero envelope.
func finalizeLocateEnvelope(out *LocateOutput, lastIndexed time.Time) {
	switch out.Status {
	case "ok":
		out.Confidence = output.Confidence{
			Level: output.LevelHigh,
			Basis: []string{"symbol resolved from index"},
		}
		if out.Path != "" && out.StartLine > 0 {
			end := out.EndLine
			if end < out.StartLine {
				end = out.StartLine
			}
			out.Evidence = []output.EvidenceSpan{{
				Path: out.Path, Start: out.StartLine, End: end,
				Symbol: out.Symbol, Kind: output.SpanExact,
			}}
		}
		if out.Risk != "" {
			out.RiskFlags = []string{"blast-radius: " + out.Risk}
		}
		out.Stale = output.AgeStale(lastIndexed)
		if out.Symbol != "" {
			out.NextCalls = []output.NextCall{{
				Tool:   "trace",
				Args:   out.Symbol + " --dir callees",
				Reason: "follow what the resolved symbol calls",
			}}
		}
	default:
		// no-index / not-found / error: best-effort, no resolved span.
		out.Confidence = output.Confidence{Level: output.LevelLow}
		if out.Hint != "" {
			out.Confidence.Gaps = []string{out.Hint}
		}
		out.Stale = output.AgeStale(lastIndexed)
	}
	out.Envelope.Normalize()
}

// Locate runs the locate verb for callers without an SDK request — the REST
// `/locate` route. It composes over the local *Server exactly like the stdio
// `locate` tool, so both transports agree.
func (s *Server) Locate(ctx context.Context, in LocateInput) (LocateOutput, error) {
	_, out, err := s.locate(ctx, nil, in)
	return out, err
}

func (s *Server) locate(ctx context.Context, _ *sdk.CallToolRequest, in LocateInput) (ctr *sdk.CallToolResult, out LocateOutput, err error) {
	// Finalize the shared envelope on every return path, including the early
	// error/no-index exits below (#816).
	var lastIndexed time.Time
	defer func() { finalizeLocateEnvelope(&out, lastIndexed) }()

	if strings.TrimSpace(in.Ref) == "" && strings.TrimSpace(in.Symbol) == "" && strings.TrimSpace(in.Frame) == "" {
		return nil, LocateOutput{Status: "error", Hint: "locate needs one of: ref, symbol, frame", Callers: []CallSite{}}, nil
	}
	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, LocateOutput{Status: "error", Hint: hint, Callers: []CallSite{}}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, LocateOutput{Status: "no-index", Project: p.Root, Callers: []CallSite{},
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, LocateOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err), Callers: []CallSite{}}, nil
	}
	if stats, serr := st.Stats(ctx); serr == nil {
		lastIndexed = stats.LastIndex
	}

	k := in.K
	if k <= 0 {
		k = 8
	}
	if k > 30 {
		k = 30
	}

	out = LocateOutput{Status: "ok", Project: p.Root, Callers: []CallSite{}}
	res := retrieve.ResolveLocateTarget(ctx, st, p.Root, retrieve.LocateRequest{
		Ref: in.Ref, Symbol: in.Symbol, Frame: in.Frame,
	})
	if res.Status != "ok" {
		out.Status = res.Status
		out.Hint = res.Hint
		return nil, out, nil
	}
	out.Path = res.Path
	out.Symbol = res.Symbol
	out.Kind = res.Kind
	out.StartLine = res.StartLine
	out.EndLine = res.EndLine

	// Callers (+ risk) via the trace verb — degrades to empty on no-graph.
	if res.Symbol != "" {
		_, tr, _ := traceVerb(ctx, s, nil, TraceInput{
			Symbol: res.Symbol, Direction: "callers", K: k, ProjectRoot: p.Root,
		})
		if tr.Status == "ok" {
			out.Callers = tr.Hits
			out.Risk = tr.Risk
		} else if tr.Status == "no-graph" {
			out.Hint = appendHint(out.Hint, "callers unavailable: "+tr.Hint)
		} else if tr.Status == "not-found" {
			out.Hint = appendHint(out.Hint, "symbol not in call graph — possibly unexported, not yet indexed, or graph needs reindex")
		}
	}

	// Static enrichment legs — pure filesystem / git, all best-effort.
	e := &retrieve.Enricher{ProjectRoot: p.Root}
	out.Tests = e.PairSiblingTests(res.Path)
	out.NearestDoc = e.FindNearestDoc(res.Path)
	meta := map[string]*retrieve.PathMeta{}
	e.EnrichBlame(ctx, []string{res.Path}, meta)
	if m := meta[res.Path]; m != nil {
		out.LastCommit = m.LastCommit
		out.LastAuthor = m.LastAuthor
	}

	// Optional: related open issues via `gh` — opt-in, best-effort, hermetic-safe.
	if in.Issues && res.Symbol != "" {
		out.Issues = relatedIssues(ctx, p.Root, retrieve.BareSymbolName(res.Symbol))
	}

	return nil, out, nil
}

// relatedIssues lists up to 5 open issues whose text matches the symbol via
// the `gh` CLI. Best-effort: a missing binary, no repo, or any error yields
// nil. Capped at 3s so a slow network never stalls a locate call.
func relatedIssues(ctx context.Context, root, query string) []string {
	if query == "" {
		return nil
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "gh", "issue", "list",
		"--search", query, "--state", "open", "--limit", "5",
		"--json", "number,title",
		"--template", `{{range .}}#{{.number}} {{.title}}{{"\n"}}{{end}}`)
	cmd.Dir = root
	outBytes, err := cmd.Output()
	if err != nil {
		return nil
	}
	var issues []string
	for _, ln := range strings.Split(strings.TrimSpace(string(outBytes)), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			issues = append(issues, ln)
		}
	}
	return issues
}

// appendHint joins two hint fragments with "; " so a degradation note doesn't
// clobber an existing hint.
func appendHint(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}
