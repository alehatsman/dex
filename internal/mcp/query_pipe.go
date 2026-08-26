package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// query_pipe.go — the pipe executor (#206 / docs/design/95h-query-pipes.md).
// A `query` input with a top-level `|` composes lanes: seed | transform | … |
// terminal, run left-to-right over the uniform Selection currency #207 surfaced
// on every lane (QueryOutput.Refs). The pipe threads that currency at the WIRE
// layer — each stage is a lane dispatch that already hands back its refs — so no
// wire↔domain conversion happens between stages (design note §"which layer").
//
// A length-1 pipe never reaches here: splitPipe returns a single segment and the
// caller keeps the single-lane path, so composition is purely additive.

// pipeMaxRefs bounds the working set carried between stages (and the fan-out
// breadth of a transform), mirroring the graph node cap so a mid-pipe explosion
// truncates honestly rather than running away.
const pipeMaxRefs = 200

// pipeState is the Selection currency at the wire layer: the located refs
// threaded between stages, the weakest-link provenance seen so far, the ordered
// stage labels echoed to route.stages, and the remaining token budget.
type pipeState struct {
	refs   []Ref
	prov   string   // weakest-link provenance across stages
	stages []string // executed stage labels (seed lane, then each op)
	budget int
}

// splitPipe splits a raw query on top-level `|` separators, trimming each
// segment. A `|` inside a leading `/regex/` seed is NOT a separator: the regex
// is delimited by `/`, so the first segment is taken whole up to its closing
// delimiter before the rest is split. (Only the leading seed may be a regex, so
// this is the only place `/` needs delimiter treatment.)
func splitPipe(raw string) []string {
	t := strings.TrimSpace(raw)
	if !strings.Contains(t, "|") {
		return []string{t}
	}
	var segments []string
	rest := t
	// Peel a leading /regex/ seed off whole, so its internal `|` is preserved.
	if strings.HasPrefix(t, "/") {
		if close := indexOfCloser(t); close > 0 {
			segments = append(segments, t[:close+1])
			rest = t[close+1:]
			// Drop the `|` that follows the regex seed, if any.
			if i := strings.IndexByte(rest, '|'); i >= 0 {
				rest = rest[i+1:]
			} else {
				rest = ""
			}
		}
	}
	for _, seg := range strings.Split(rest, "|") {
		if s := strings.TrimSpace(seg); s != "" {
			segments = append(segments, s)
		}
	}
	if len(segments) == 0 {
		return []string{t}
	}
	return segments
}

// indexOfCloser returns the index of the closing `/` of a leading regex (the
// first unescaped `/` after position 0), or -1 if there is none.
func indexOfCloser(t string) int {
	for i := 1; i < len(t); i++ {
		if t[i] == '/' && t[i-1] != '\\' {
			return i
		}
	}
	return -1
}

// runPipe executes a composed query. Segment 0 is the seed (the existing
// single-lane path); interior segments are transforms; the last may be a
// terminal. It assembles one QueryOutput with route.stages, weakest-link trust,
// and the final Selection in refs so the agent can pipe further.
func runPipe(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput, segments []string) (*sdk.CallToolResult, QueryOutput, error) {
	orig := strings.TrimSpace(in.Input)

	// --- seed (segment 0) ---
	seedSeg := strings.TrimSpace(segments[0])
	seedIn := in
	seedIn.Input = seedSeg
	_, seedOut, err := dispatchSingle(ctx, h, req, seedIn)
	if err != nil {
		return nil, seedOut, err
	}
	stages := []string{seedOut.Route.Lane}

	refs := seedOut.Refs
	// A bare-symbol seed's located entity is the symbol ITSELF, not its default
	// callers trace: `sym | callers` must mean "callers OF sym", so the transform
	// — not the seed — supplies the first hop (spec §Seed: "symbol → symbol ref").
	// Left as the raw trace refs, the seed would already be one hop deep.
	if seedOut.Route.Detected == "symbol" {
		refs = []Ref{{Kind: "symbol", ID: seedSeg, Prov: "name-based"}}
	}
	// A path seed that resolved no refs (e.g. a directory, which the read lane
	// does not summarize) still names a location a coercion can expand — carry it
	// as a file ref so `dir | callers` works via ExportedSymbolsByDir.
	if len(refs) == 0 && (seedOut.Route.Lane == "read" || looksLikePath(seedSeg)) {
		refs = []Ref{{Kind: "file", ID: seedSeg, Path: seedSeg, Prov: "name-based"}}
	}
	if len(refs) == 0 {
		// Genuine dead-end at the seed — return it with stages so the agent sees
		// where the pipe stalled rather than an opaque empty.
		seedOut.Route.Detected = "pipe"
		seedOut.Route.Input = orig
		seedOut.Route.Stages = stages
		return nil, seedOut, nil
	}

	st := pipeState{refs: refs, prov: provOr(seedOut.Trust.Provenance, "exact"), stages: stages, budget: in.Budget}
	lastBody := seedOut

	ops := segments[1:]
	for i, seg := range ops {
		name, arg := parseStage(seg)
		isLast := i == len(ops)-1
		switch {
		case isTransformOp(name):
			nextRefs, body, prov, terr := runTransform(ctx, h, req, in, name, st.refs)
			if terr != nil {
				return pipeError(orig, st.stages, terr.Error())
			}
			st.stages = append(st.stages, name)
			st.prov = weakenProvenance(st.prov, prov)
			if len(nextRefs) == 0 {
				// Honest partial: the transform found nothing. Surface its empty
				// typed body with the stages run so far.
				body.Route = QueryRoute{Input: orig, Detected: "pipe", Lane: body.Route.Lane, Stages: st.stages}
				body.Refs = nil
				body.Trust.Provenance = st.prov
				return nil, body, nil
			}
			st.refs = nextRefs
			lastBody = body
		case isTerminalOp(name):
			if !isLast {
				return pipeError(orig, st.stages, "terminal '"+name+"' must be the last stage")
			}
			body, terr := runTerminal(ctx, h, req, in, name, arg, &st)
			if terr != nil {
				return pipeError(orig, st.stages, terr.Error())
			}
			st.stages = append(st.stages, seg)
			lastBody = body
		default:
			return pipeError(orig, st.stages,
				"unknown pipe op '"+name+"' — transforms: callers|callees|impact; terminals: signatures|assemble:N")
		}
	}

	// Assemble the final envelope from the last stage's body, overlaying the pipe
	// route, the accumulated Selection, and weakest-link provenance (a caveat set
	// by a terminal is preserved).
	out := lastBody
	lane := out.Route.Lane
	out.Route = QueryRoute{Input: orig, Detected: "pipe", Lane: lane, Stages: st.stages}
	out.Refs = st.refs
	out.Trust.Provenance = st.prov
	if st.prov != "exact" && out.Trust.Caveat == "" {
		out.Trust.Caveat = "pipe provenance is the weakest link across stages (a coercion or partial-recall walk ran)"
	}
	out.Status = "ok"
	return nil, out, nil
}

// dispatchSingle is the single-lane path shared by queryVerb and the pipe seed:
// classify the input shape and route to the exact/graph or semantic dispatcher.
// A prose lane still feeds the #610 adaptive-compression task signal.
func dispatchSingle(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput) (*sdk.CallToolResult, QueryOutput, error) {
	input := strings.TrimSpace(in.Input)
	lr, cleaned, detected, alt := classifyQuery(input, in.Kind)
	route := QueryRoute{Input: input, Detected: detected, Lane: lr.lane, Forced: detected == "forced", Alt: alt}
	switch lr.lane {
	case "read", "grep", "locate", "symbol":
		return dispatchExact(ctx, h, req, in, lr, cleaned, route)
	default: // semantic
		writeCurrentTask(input)
		return dispatchSemantic(ctx, h, req, in, lr, cleaned, route)
	}
}

// parseStage splits a segment into an op name and its optional `:arg` (e.g.
// "assemble:6000" → "assemble","6000").
func parseStage(seg string) (name, arg string) {
	seg = strings.TrimSpace(seg)
	if i := strings.IndexByte(seg, ':'); i >= 0 {
		return strings.ToLower(strings.TrimSpace(seg[:i])), strings.TrimSpace(seg[i+1:])
	}
	return strings.ToLower(seg), ""
}

func isTransformOp(name string) bool {
	switch name {
	case "callers", "callees", "impact":
		return true
	}
	return false
}

func isTerminalOp(name string) bool {
	switch name {
	case "signatures", "assemble":
		return true
	}
	return false
}

// runTransform fans a graph transform out over the input refs: coerce each to a
// symbol, run the trace lane per symbol, and union+dedupe the results by ID. The
// aggregate typed TraceOutput becomes the default terminal body. Provenance is
// name-based when any coercion happened or any walk had partial recall.
func runTransform(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput, op string, refs []Ref) (out []Ref, body QueryOutput, prov string, err error) {
	syms, coerced, cerr := coerceToSymbols(ctx, h, req, in, refs)
	if cerr != nil {
		return nil, QueryOutput{}, "", cerr
	}
	if len(syms) > pipeMaxRefs {
		syms = syms[:pipeMaxRefs]
	}
	prov = "exact"
	if coerced {
		prov = "name-based"
	}

	agg := &TraceOutput{Direction: op, Status: "not-found", Hits: []CallSite{}}
	seen := make(map[string]bool)
	for _, s := range syms {
		_, to, terr := traceVerb(ctx, h, req, TraceInput{
			Symbol: s.ID, Direction: op, K: in.K, ProjectRoot: in.ProjectRoot,
		})
		if terr != nil {
			continue
		}
		if to.Status == "ok" {
			agg.Status = "ok"
		}
		agg.Hits = append(agg.Hits, to.Hits...)
		agg.Nodes = append(agg.Nodes, to.Nodes...)
		if to.Recall == "partial" {
			agg.Recall = "partial"
		}
		if agg.Risk == "" {
			agg.Risk = to.Risk
		}
		for _, r := range refsFromTrace(&to) {
			if r.ID == "" || seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, r)
		}
		if len(out) >= pipeMaxRefs {
			agg.Truncated = true
			break
		}
	}
	if agg.Recall == "partial" {
		prov = weakenProvenance(prov, "name-based")
	}
	body = QueryOutput{
		Status: agg.Status,
		Route:  QueryRoute{Lane: "trace"},
		Result: QueryResult{Trace: agg},
		Trust:  EnvTrust{Provenance: prov},
	}
	return out, body, prov, nil
}

// runTerminal projects the final Selection into a body. Both terminals render
// the refs' files as compressed signatures; assemble:N additionally caps the
// output at N tokens, dropping lowest-priority files (an honest partial).
func runTerminal(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput, name, arg string, st *pipeState) (QueryOutput, error) {
	switch name {
	case "signatures":
		return renderSignatures(ctx, h, req, in, st.refs, 0), nil
	case "assemble":
		budget := st.budget
		if arg != "" {
			if n, e := strconv.Atoi(arg); e == nil && n > 0 {
				budget = n
			}
		}
		return renderSignatures(ctx, h, req, in, st.refs, budget), nil
	}
	return QueryOutput{}, fmt.Errorf("unknown terminal %q", name)
}

// renderSignatures reads each distinct file in the Selection as compressed
// signatures and concatenates them. When budget > 0 it stops once the running
// token count would exceed it (keeping ref order), recording the drop as a
// caveat — the #164 clamp discipline applied to a pipe terminal.
func renderSignatures(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput, refs []Ref, budget int) QueryOutput {
	paths := uniquePaths(refs)
	var b strings.Builder
	used := make([]string, 0, len(paths))
	dropped := 0
	for idx, p := range paths {
		_, ro, err := h.summarize(ctx, req, SummarizeInput{Path: p, Mode: "signatures", ProjectRoot: in.ProjectRoot})
		if err != nil || strings.TrimSpace(ro.Content) == "" {
			continue
		}
		if budget > 0 && b.Len() > 0 && tokens.Count(b.String()+"\n\n"+ro.Content) > budget {
			dropped = len(paths) - idx
			break
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(ro.Content)
		used = append(used, p)
	}
	out := QueryOutput{
		Status: "ok",
		Route:  QueryRoute{Lane: "read"},
		Result: QueryResult{Read: &SummarizeOutput{Status: "ok", Paths: used, Content: b.String(), Truncated: dropped > 0}},
		Trust:  EnvTrust{Provenance: "exact"},
	}
	if dropped > 0 {
		out.Trust.Caveat = fmt.Sprintf("assemble budget reached — %d lower-priority file(s) dropped", dropped)
	}
	return out
}

// uniquePaths returns the distinct file paths across a ref set, preserving first
// -seen order (highest-priority refs come first from the transforms).
func uniquePaths(refs []Ref) []string {
	seen := make(map[string]bool, len(refs))
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		p := refPath(r)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// pipeError builds a legible pipe failure: the stages that ran, the reason, and
// a status the agent can branch on. Refs are omitted — the pipe did not complete.
func pipeError(orig string, stages []string, reason string) (*sdk.CallToolResult, QueryOutput, error) {
	return nil, QueryOutput{
		Status: "error",
		Route:  QueryRoute{Input: orig, Detected: "pipe", Stages: stages},
		Trust:  EnvTrust{Provenance: "exact", Caveat: reason},
	}, nil
}

// provOr returns p when non-empty, else the fallback.
func provOr(p, fallback string) string {
	if p == "" {
		return fallback
	}
	return p
}

// provRank orders provenance from strongest to weakest so weakenProvenance can
// take the minimum: exact > name-based > semantic.
func provRank(p string) int {
	switch p {
	case "exact":
		return 3
	case "name-based":
		return 2
	case "semantic":
		return 1
	default:
		return 0
	}
}

// weakenProvenance returns the weaker (lower-ranked) of two provenances — the
// weakest-link rule that makes a semantic seed taint the whole pipe (spec §Why).
func weakenProvenance(a, b string) string {
	if provRank(b) < provRank(a) {
		return b
	}
	return a
}
