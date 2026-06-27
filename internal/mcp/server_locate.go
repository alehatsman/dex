package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/store"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── verb: locate ─────────────────────────────────────────────────────────
//
// locate (#636 / GitHub #65 Tier S1) is one-call orientation around a code
// location. It collapses the 4–6 call dance every session repeats — resolve
// the symbol, walk its callers, find sibling tests, find the nearest doc,
// check recent churn, recall related notes — into a single response. It is
// pure composition over shipped infrastructure: ChunkAt / FindSymbol for
// resolution, traceVerb for callers, the Enricher legs for tests / nearest
// doc / git blame, recallFacts for notes. No new store schema, no new
// backend, no model call.

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
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// LocatedFact is the compact projection of a knowledge fact surfaced next to
// a located symbol — just enough to judge relevance without the full record.
type LocatedFact struct {
	ID        int64   `json:"id"`
	Archetype string  `json:"archetype"`
	Body      string  `json:"body"`
	Salience  float64 `json:"salience"`
	// Scope is set when this note surfaced because its scope matched the file
	// being located (proactive gotcha-on-touch, #645), naming the glob/path it's
	// bound to. Empty for a note recalled by semantic/salience match.
	Scope string `json:"scope,omitempty"`
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
	Notes      []LocatedFact `json:"notes,omitempty"`
	Issues     []string      `json:"issues,omitempty"`
}

// Locate runs the locate verb for callers without an SDK request — the REST
// `/locate` route. It composes over the local *Server exactly like the stdio
// `locate` tool, so both transports agree.
func (s *Server) Locate(ctx context.Context, in LocateInput) (LocateOutput, error) {
	_, out, err := s.locate(ctx, nil, in)
	return out, err
}

func (s *Server) locate(ctx context.Context, _ *sdk.CallToolRequest, in LocateInput) (*sdk.CallToolResult, LocateOutput, error) {
	if strings.TrimSpace(in.Ref) == "" && strings.TrimSpace(in.Symbol) == "" && strings.TrimSpace(in.Frame) == "" {
		return nil, LocateOutput{Status: "error", Hint: "locate needs one of: ref, symbol, frame", Callers: []CallSite{}}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
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

	k := in.K
	if k <= 0 {
		k = 8
	}
	if k > 30 {
		k = 30
	}

	out := LocateOutput{Status: "ok", Project: p.Root, Callers: []CallSite{}}
	res := s.resolveLocateTarget(ctx, st, p.Root, in)
	if res.status != "ok" {
		out.Status = res.status
		out.Hint = res.hint
		return nil, out, nil
	}
	out.Path = res.path
	out.Symbol = res.symbol
	out.Kind = res.kind
	out.StartLine = res.startLine
	out.EndLine = res.endLine

	// Callers (+ risk) via the trace verb — degrades to empty on no-graph.
	if res.symbol != "" {
		_, tr, _ := traceVerb(ctx, s, nil, TraceInput{
			Symbol: res.symbol, Direction: "callers", K: k, ProjectRoot: p.Root,
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
	e := &Enricher{projectRoot: p.Root}
	out.Tests = e.pairSiblingTests(res.path)
	out.NearestDoc = e.findNearestDoc(res.path)
	meta := map[string]*PathMeta{}
	e.enrichBlame(ctx, []string{res.path}, meta)
	if m := meta[res.path]; m != nil {
		out.LastCommit = m.LastCommit
		out.LastAuthor = m.LastAuthor
	}

	// Related notes — semantic match on the symbol name when an embedder is
	// wired, falling back to top-salience facts otherwise (recallFacts handles
	// the degradation internally).
	seenNote := map[int64]bool{}
	// Proactive gotcha-on-touch (#645) FIRST: notes SCOPED to this file's path —
	// surfaced (and tagged with `scope`) because you touched the file, even if
	// they wouldn't match the symbol semantically. The "you're editing X, here's
	// the gotcha about X" signal leads.
	if scoped, serr := st.KnowledgeByScope(ctx, res.path, k); serr == nil {
		for _, f := range scoped {
			seenNote[f.ID] = true
			out.Notes = append(out.Notes, LocatedFact{
				ID: f.ID, Archetype: f.Archetype, Body: f.Body, Salience: f.Salience, Scope: f.Scope,
			})
		}
	}
	// Then semantic recall on the symbol, deduped against the scoped set.
	// skipFallback=true: locate shows no notes rather than irrelevant top-salience
	// ones when the symbol doesn't match any note semantically.
	if facts, ferr := s.recallFacts(ctx, st, res.symbol, k, false, true); ferr == nil {
		for _, f := range facts {
			if seenNote[f.ID] {
				continue
			}
			seenNote[f.ID] = true
			out.Notes = append(out.Notes, LocatedFact{
				ID: f.ID, Archetype: f.Archetype, Body: f.Body, Salience: f.Salience,
			})
		}
	}

	// Optional: related open issues via `gh` — opt-in, best-effort, hermetic-safe.
	if in.Issues && res.symbol != "" {
		out.Issues = relatedIssues(ctx, p.Root, bareSymbolName(res.symbol))
	}

	return nil, out, nil
}

// locateTarget is the resolved (path, symbol, kind, line-range) plus a status
// the caller maps straight onto LocateOutput.
type locateTarget struct {
	path      string
	symbol    string
	kind      string
	startLine int
	endLine   int
	status    string // "ok" | "not-found"
	hint      string
}

// resolveLocateTarget turns the input into a concrete target. Ref wins, then
// Symbol, then Frame (which is itself parsed into a ref or a symbol).
func (s *Server) resolveLocateTarget(ctx context.Context, st *store.Store, root string, in LocateInput) locateTarget {
	if ref := strings.TrimSpace(in.Ref); ref != "" {
		return resolveByRef(ctx, st, root, ref)
	}
	if sym := strings.TrimSpace(in.Symbol); sym != "" {
		return resolveBySymbol(ctx, st, sym)
	}
	// Frame: pull a file:line or a symbol out of the raw line.
	ref, sym := parseFrame(in.Frame)
	if ref != "" {
		return resolveByRef(ctx, st, root, ref)
	}
	if sym != "" {
		return resolveBySymbol(ctx, st, sym)
	}
	return locateTarget{status: "not-found", hint: "could not parse a file:line or symbol out of the frame"}
}

// resolveByRef resolves a 'path:line' to its enclosing chunk via ChunkAt.
func resolveByRef(ctx context.Context, st *store.Store, root, ref string) locateTarget {
	path, line, ok := parseRef(ref)
	if !ok {
		return locateTarget{status: "not-found", hint: fmt.Sprintf("ref %q is not 'path:line'", ref)}
	}
	path = normalizeRefPath(root, path)
	hit, err := st.ChunkAt(ctx, path, line)
	if err != nil {
		return locateTarget{status: "not-found",
			hint: fmt.Sprintf("%v — check the path is project-relative and indexed.", err)}
	}
	sym := hit.Name
	// Prefer the graph node's QualifiedName (e.g. "(*Store).Run") over the
	// bare chunk name ("Run") so traceVerb doesn't match every node named Run
	// across all packages (#716).
	if qn, qerr := st.GraphQualifiedNameAt(ctx, path, line); qerr == nil && qn != "" {
		sym = qn
	}
	return locateTarget{
		path: hit.Path, symbol: sym, kind: hit.Kind,
		startLine: hit.StartLine, endLine: hit.EndLine, status: "ok",
	}
}

// resolveBySymbol resolves a symbol name to its first indexed definition.
func resolveBySymbol(ctx context.Context, st *store.Store, sym string) locateTarget {
	hits, err := st.FindSymbol(ctx, sym, 1)
	if err != nil {
		return locateTarget{status: "not-found", hint: fmt.Sprintf("lookup %q: %v", sym, err)}
	}
	// Frames and qualified names ('main.Greet', 'mcp.(*Server).Run') don't match
	// the exact-name index; fall back to the bare trailing identifier.
	if len(hits) == 0 {
		if bare := bareSymbolName(sym); bare != sym {
			// For receiver-qualified forms like (*T).Method, fetch more candidates
			// so we can prefer method/function over field — a field named Context
			// loses to a method named Context when the input is (*Server).Context.
			k := 1
			if reReceiverPointer.MatchString(sym) {
				k = 20
			}
			hits, err = st.FindSymbol(ctx, bare, k)
			if err != nil {
				return locateTarget{status: "not-found", hint: fmt.Sprintf("lookup %q: %v", bare, err)}
			}
			if reReceiverPointer.MatchString(sym) && len(hits) > 1 {
				// Extract the receiver type (e.g. "Client" from "(*Client).Method")
				// and prefer a hit whose signature mentions that type, so that
				// (*Client).Method doesn't resolve to (*Server).Method.
				rxType := ""
				if m := reReceiverType.FindStringSubmatch(sym); m != nil {
					rxType = m[1]
				}
				chosen := -1
				for i, h := range hits {
					if h.Kind != "method" && h.Kind != "function" {
						continue
					}
					if chosen < 0 {
						chosen = i // first method: fallback if no type match
					}
					if rxType != "" && strings.Contains(h.Signature, rxType) {
						chosen = i
						break
					}
				}
				if chosen >= 0 {
					hits = []store.Hit{hits[chosen]}
				}
			}
		}
	}
	if len(hits) == 0 {
		return locateTarget{status: "not-found",
			hint: fmt.Sprintf("no indexed symbol named %q — check spelling or reindex.", sym)}
	}
	h := hits[0]
	name := h.Name
	if name == "" {
		name = sym
	}
	return locateTarget{
		path: h.Path, symbol: name, kind: h.Kind,
		startLine: h.StartLine, endLine: h.EndLine, status: "ok",
	}
}

// parseRef splits 'path:line' or 'path:line:col' into its path and line.
// A path with no trailing :line is rejected (locate needs a line to find the
// enclosing symbol).
func parseRef(ref string) (path string, line int, ok bool) {
	ref = strings.TrimSpace(ref)
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return "", 0, false
	}
	// Allow a trailing :col — peel it off and retry on the remainder.
	if n, err := strconv.Atoi(ref[i+1:]); err == nil {
		head := ref[:i]
		if j := strings.LastIndex(head, ":"); j >= 0 {
			if ln, err2 := strconv.Atoi(head[j+1:]); err2 == nil {
				return head[:j], ln, true // path:line:col
			}
		}
		return head, n, true // path:line
	}
	return "", 0, false
}

// frameLoc matches a 'file.ext:line' anywhere inside a stack frame.
var frameLoc = regexp.MustCompile(`([^\s:()]+\.[A-Za-z0-9]+):(\d+)`)

// reReceiverPointer matches the (*T). prefix in a receiver-qualified symbol like (*Server).Context.
// Used to bias bare-name fallback toward method/function kinds over fields.
var reReceiverPointer = regexp.MustCompile(`^\(\*[^)]+\)\.`)

// reReceiverType extracts the type name from a pointer-receiver prefix:
// "(*Client).Method" → "Client".
var reReceiverType = regexp.MustCompile(`^\([*]?([^)]+)\)\.*`)

// parseFrame extracts either a 'path:line' ref or a symbol from one raw stack
// frame. A file location wins (it pins the exact site); otherwise the trailing
// call expression is reduced to a trace-friendly symbol form.
func parseFrame(frame string) (ref, symbol string) {
	frame = strings.TrimSpace(frame)
	if m := frameLoc.FindStringSubmatch(frame); m != nil {
		return m[1] + ":" + m[2], ""
	}
	s := frame
	// Keep the rightmost path segment: 'github.com/x/mcp.(*Server).Run' → 'mcp.(*Server).Run'.
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	// Drop a trailing argument list: '(*Server).Run(0xc..)' → '(*Server).Run'.
	if strings.HasSuffix(s, ")") {
		if i := strings.LastIndex(s, "("); i >= 0 {
			s = s[:i]
		}
	}
	// Drop a leading package qualifier on a receiver method: 'mcp.(*Server).Run'
	// → '(*Server).Run' (the receiver-qualified form trace prefers).
	if i := strings.Index(s, ".("); i >= 0 {
		s = s[i+1:]
	}
	return "", strings.TrimSpace(s)
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
