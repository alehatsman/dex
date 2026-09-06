package mcp

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/alehatsman/dex/internal/proj"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// query is the single read verb of the two-verb surface (#196, epic #195, spec
// specs/two-verb-surface.md). It merges the four-verb surface's `ask` (infer
// intent from a question) and `look` (exact fetch of a named target) into one
// verb whose OUTPUT PRECISION TRACKS INPUT PRECISION.
//
// The whole design is a pure string classifier (classifyQuery) over the SAME
// engine both old verbs already drove: the exact-fetch lanes behind lookVerb
// (read / grep / locate / trace) and the intent router behind contextRouter
// (ResolveIntent → semantic / architecture / orient / review). query adds no
// retrieval; it decides which existing lane an input belongs to and folds the
// result into one envelope with a legible `route`.
//
// The precision ladder (see the spec):
//
//	/regex/ · path:line · path          → exact lanes  (provenance: exact)
//	single identifier token (Foo, pkg.F) → graph lane   (NARROW DEFAULT — just its call graph)
//	prose ("how are edits debounced")    → semantic/intent lane (contextRouter)
//
// The narrow-default guarantee — a bare symbol returns JUST its call graph,
// never a fused semantic pack — is why merging ask+look is safe: naming a symbol
// is precise input and earns a precise (graph) answer, not a probabilistic one.

// QueryInput is the single-string read request. Only `input` is required; `kind`
// forces the lane and `want` picks the facet, both otherwise inferred from the
// shape of `input`.
type QueryInput struct {
	Input string `json:"input" jsonschema:"what to read. Its SHAPE picks the lane: a file path ('internal/mcp/server.go') → its compressed signatures (for raw bytes use the native Read tool), a location ('server.go:829') or range ('server.go:120-140') → that slice, a regex ('/func .*Verb/') → grep, a bare symbol ('NewServer', '(*Server).Run', 'mcp.NewServer') → its call graph, and a prose question ('how are edits debounced?') → a ranked semantic evidence pack. Output precision tracks input precision. A 'field:pattern' seed selects a symbol set from the index (pkg:/func:/type:/file:/kind:, space-separated = AND, glob */?; e.g. 'func:*Handler'). A 'since:<ref>' or 'diff:<ref>' seed selects the symbols a diff touches (ref='working' or omitted = uncommitted changes; a single ref = ref..HEAD; e.g. 'since:HEAD~3'). Compose lanes in one call with '|': '<seed> | callers|callees|impact | signatures|assemble:N' runs the stages left-to-right in one round-trip (e.g. '(*Server).Run | callers | impact', 'pkg:store | callers')."`
	// Kind forces the lane, bypassing shape detection.
	Kind string `json:"kind,omitempty" jsonschema:"force the lane instead of inferring it from input shape: read|grep|locate (exact) · symbol|callers|callees|impact|path (graph) · search|editing|assemble|architecture|packages|orient|review (semantic/intent)"`
	// Want picks the facet within the chosen lane.
	Want string `json:"want,omitempty" jsonschema:"pick the facet within the lane: for a file 'signatures' (default) | 'map' (imports+exports) | 'skeleton' | 'lines:N-M' (slice) — raw full content is the native Read tool's job, not query's; for a symbol 'callers|callees|impact|path'; for a prose pack 'answer' (synthesized prose) or 'assemble' (budget-bounded working set)"`
	To   string `json:"to,omitempty" jsonschema:"destination symbol for the graph 'path' facet (shortest call route from input to this symbol)"`
	// grep pass-through.
	Context int  `json:"context,omitempty" jsonschema:"grep lane only: lines of surrounding context per match (0-10)"`
	Fixed   bool `json:"fixed,omitempty" jsonschema:"grep lane only: treat the pattern as a literal string, not a regex"`
	// shared.
	K           int    `json:"k,omitempty" jsonschema:"max results per lane (for the symbol lane's impact facet, this caps results PER GRAPH DEPTH, not the total — see result.trace.elided for what a lower k drops)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
	Budget      int    `json:"budget,omitempty" jsonschema:"optional context-token budget; when set, the response reports cost.budget_left = budget − tokens_returned"`
	// ProjectRoots fans this same query out across multiple already-indexed
	// local projects instead of running it against one (#221). When set it
	// overrides ProjectRoot: the query runs once per root, independently, and
	// results come back labeled per project in QueryOutput.Fanout — no merge,
	// no cross-project graph joins. The literal entry "all" expands to every
	// project this server knows about (same discovery `dex reindex --all`
	// uses).
	ProjectRoots []string `json:"project_roots,omitempty" jsonschema:"run this query independently across these already-indexed local project roots and return labeled per-project results in the response's fanout field, instead of a single project_root; pass [\"all\"] to fan out across every project this server already knows about"`
	// Claims is kind=check's batch of file:line[:symbol] refs to verify. The one
	// deliberate exception to "one string input" (#849 spec, resolved open
	// question #2): a batch of claims genuinely isn't a single scalar, and
	// documenting one exception here beats quietly growing a second later.
	Claims []ClaimRef `json:"claims,omitempty" jsonschema:"only used when kind=check: a batch of file:line or file:line:symbol refs to verify against the index"`
}

func (in QueryInput) budgetTokens() int { return in.Budget }

// QueryRoute is the legible routing decision (spec §"the universal envelope"):
// what shape dex detected in the input, which lane it chose, and the alternative
// interpretation it did NOT take. Routing is never silent guessing — an
// ambiguous input names the road not taken so the agent can force it via `kind`.
type QueryRoute struct {
	Input    string     `json:"input"`
	Detected string     `json:"detected"` // the input shape: path|location|regex|symbol|prose|pipe
	Lane     string     `json:"lane"`     // the lane chosen: read|grep|locate|symbol|semantic
	Forced   bool       `json:"forced,omitempty"`
	Alt      []QueryAlt `json:"alt,omitempty"`
	// Stages echoes the ordered pipe segments actually executed (#206) — the seed
	// lane then each transform/terminal op. Empty for a single-lane query; set only
	// when the input composed lanes with `|`. Teaches the pipe grammar in-band.
	Stages []string `json:"stages,omitempty"`
}

// QueryAlt is one interpretation query did not take, offered as a ready-to-force
// `kind` so the agent settles an ambiguous input in one follow-up.
type QueryAlt struct {
	Kind string `json:"kind"`
	Why  string `json:"why"`
}

// QueryResult is the flat, lane-keyed union of the payloads (#207 / #95g).
// route.lane is the single discriminator; exactly one pointer below is populated,
// and the envelope (status/trust/cost/next) lives once on QueryOutput — no
// look/ask wrapper, no re-declared envelope. The exact lanes are the same
// payloads the exact-fetch handlers return; the semantic god-struct is split
// per-intent (SemanticResult / OrientResult / ReviewOutput). Projections live in
// query_result.go.
type QueryResult struct {
	// exact lanes (unwrapped from the former LookResult)
	Read   *SummarizeOutput  `json:"read,omitempty"`
	Grep   *SearchGrepOutput `json:"grep,omitempty"`
	Trace  *TraceOutput      `json:"trace,omitempty"`
	Locate *LocateOutput     `json:"locate,omitempty"`

	// semantic lanes (split from the former ContextOutput god-struct)
	Semantic *SemanticResult `json:"semantic,omitempty"`
	Orient   *OrientResult   `json:"orient,omitempty"`
	Review   *ReviewOutput   `json:"review,omitempty"`

	// selector lane (#210): a symbol set enumerated from the index by a
	// `field:pattern` seed. Refs carries the same symbols for pipe threading.
	Select *SelectResult `json:"select,omitempty"`

	// since lane (#219): the symbol set touched by a diff, resolved through the
	// same range logic review_diff uses. Refs carries the same symbols for pipe
	// threading.
	Since *SinceResult `json:"since,omitempty"`

	// forced-kind-only lanes folded in by the CLI collapse (#849): each wraps an
	// existing single-input server verb verbatim, no new retrieval logic.
	Check  *CheckOutput     `json:"check,omitempty"`
	Xref   *RefsOutput      `json:"refs,omitempty"` // Go field named Xref, not Refs, so it can't be confused with QueryOutput's uniform Refs []Ref selection index above.
	Cohort *CohortOutput    `json:"cohort,omitempty"`
	Deps   *GraphDepsOutput `json:"deps,omitempty"`
	// StatusReport, not Status: QueryOutput.Status is the envelope's ok/error
	// string — a same-named object field one level down would misread as the
	// envelope status itself.
	StatusReport *StatusOutput `json:"status_report,omitempty"`

	// clones/similar, folded in by the REST collapse (#851) — same pattern as
	// the #849 lanes above: thin wraps of existing single-input server verbs.
	Clones  *ClonesOutput  `json:"clones,omitempty"`
	Similar *RelatedOutput `json:"similar,omitempty"`
}

// QueryOutput is the universal envelope for the read verb. Status/Trust/Cost/Next
// are hoisted from whichever lane ran so an agent reads them the same way for
// every input shape; Result carries the full lane payload for detail.
type QueryOutput struct {
	Status string `json:"status"`
	// Hint carries a lane-level explanation that doesn't fit route/trust — most
	// notably why a semantic-lane call returned "loop-blocked" (#231): without
	// this, dispatchSemantic projected ContextOutput's own Hint away entirely,
	// so an agent saw an empty payload with no explanation of why.
	Hint   string      `json:"hint,omitempty"`
	Route  QueryRoute  `json:"route"`
	Result QueryResult `json:"result"`
	// Refs is the uniform Selection currency (#207 / #95f): a flat index of the
	// located entities the lane surfaced, one shape across every lane, so an agent
	// (and a pipe stage, #206) threads results without sniffing the typed payload.
	// The payload in Result stays authoritative for rendering.
	Refs  []Ref      `json:"refs,omitempty"`
	Trust EnvTrust   `json:"trust"`
	Cost  *EnvCost   `json:"cost,omitempty"`
	Next  []NextStep `json:"next,omitempty"`
	// Fanout carries the per-project results of a project_roots query (#221),
	// one entry per requested root in request order (not completion order —
	// deterministic regardless of which project answers first). Set only when
	// the request had ProjectRoots; Result/Route/Refs stay zero in that case —
	// Fanout is the whole answer. No merging: each project's own QueryResult
	// is returned as-is, labeled by root.
	Fanout []QueryFanout `json:"fanout,omitempty"`
}

// QueryFanout is one project's outcome from a project_roots query. Status
// mirrors the top-level envelope's vocabulary ("ok"/"no-index"/"error") so a
// per-project failure — an unindexed root, a bad path — degrades that one
// entry instead of failing the whole fan-out.
type QueryFanout struct {
	Root   string             `json:"root"`
	Status string             `json:"status"`
	Error  string             `json:"error,omitempty"`
	Result *QueryFanoutResult `json:"result,omitempty"`
}

// QueryFanoutResult is QueryOutput's payload minus Fanout itself — a
// per-project query never recurses into another fan-out (dispatchFanout
// always clears ProjectRoots before rerunning the query per root), so this
// is a lossless, cycle-free projection of the single-project envelope.
type QueryFanoutResult struct {
	Status string      `json:"status"`
	Hint   string      `json:"hint,omitempty"`
	Route  QueryRoute  `json:"route"`
	Result QueryResult `json:"result"`
	Refs   []Ref       `json:"refs,omitempty"`
	Trust  EnvTrust    `json:"trust"`
	Cost   *EnvCost    `json:"cost,omitempty"`
	Next   []NextStep  `json:"next,omitempty"`
}

func (o *QueryOutput) stampCost(t int, left *int) { o.Cost = withCost(o.Cost, t, left) }

// symbolToken matches a single bare / receiver-qualified / package-qualified
// identifier — the shape that routes to the graph lane. It deliberately rejects
// anything with whitespace (prose) so `classifyLookTarget`'s `trace` catch-all,
// which would misroute a prose question to the graph lane, is split here.
//
// Matches: Foo · foo_bar · (*Server).Run · Server.Run · mcp.NewServer · a::b::c
var symbolToken = regexp.MustCompile(`^\(?\*?[\p{L}_][\p{L}\p{N}_]*(\)?\.[\p{L}_][\p{L}\p{N}_]*|::[\p{L}_][\p{L}\p{N}_]*)*$`)

// looksLikeSymbol reports whether a target is a single symbol name (→ graph lane)
// rather than a prose question (→ semantic lane). This is the gate the two-verb
// merge adds: it sits exactly where classifyLookTarget bottoms out at `trace`.
func looksLikeSymbol(t string) bool {
	t = strings.TrimSpace(t)
	if t == "" {
		return false
	}
	// Any whitespace or a question mark is prose, never a symbol.
	if strings.ContainsAny(t, " \t\n?") {
		return false
	}
	return symbolToken.MatchString(t)
}

// kindToLane maps an explicit `kind` override to the lane that serves it and the
// facet (direction / intent) it implies. Unifies look's Kind and ask's Intent.
// ok=false for an unknown kind.
type laneRoute struct {
	lane      string // read|grep|locate|symbol|semantic
	direction string // graph lane: callers|callees|impact|path
	intent    string // semantic lane: forced ResolveIntent intent
	mode      string // read lane: forced read mode (e.g. lines:N-M for a range)
}

// pathLineRange matches a `path:N-M` slice request. classifyLookTarget only
// recognises a single-line `path:N` location; a range is a precise slice, served
// by the read lane's lines mode.
var pathLineRange = regexp.MustCompile(`^(.+):(\d+)-(\d+)$`)

func kindToLane(kind string) (laneRoute, bool) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "read":
		return laneRoute{lane: "read"}, true
	case "grep":
		return laneRoute{lane: "grep"}, true
	case "locate":
		return laneRoute{lane: "locate"}, true
	case "symbol":
		return laneRoute{lane: "symbol"}, true
	case "callers":
		return laneRoute{lane: "symbol", direction: "callers"}, true
	case "callees":
		return laneRoute{lane: "symbol", direction: "callees"}, true
	case "impact":
		return laneRoute{lane: "symbol", direction: "impact"}, true
	case "path":
		return laneRoute{lane: "symbol", direction: "path"}, true
	case "search":
		return laneRoute{lane: "semantic", intent: "behavior_search"}, true
	case "editing":
		return laneRoute{lane: "semantic", intent: "editing_context"}, true
	case "assemble":
		return laneRoute{lane: "semantic", intent: "assemble"}, true
	case "architecture":
		return laneRoute{lane: "semantic", intent: "architecture"}, true
	case "packages":
		return laneRoute{lane: "semantic", intent: "package_topology"}, true
	case "orient":
		return laneRoute{lane: "semantic", intent: "orient"}, true
	case "review":
		return laneRoute{lane: "semantic", intent: "review"}, true
	// The four kinds below (#849 CLI collapse) have no shape-detected route —
	// they're forced-kind-only, each a thin dispatch onto an existing, single-
	// input server verb (check/refs/cohort/deps/status) that already fits
	// (input, kind) but was never reachable through query.
	case "check":
		return laneRoute{lane: "check"}, true
	case "refs":
		return laneRoute{lane: "refs"}, true
	case "cohort":
		return laneRoute{lane: "cohort"}, true
	case "deps":
		return laneRoute{lane: "deps"}, true
	case "status":
		return laneRoute{lane: "status"}, true
	// clones/similar (#851 REST collapse): the input-anchored pair of the
	// zero-subject/input-anchored split the spec's resolved open question #1
	// made — routes/smells/clusters (zero-subject, no required input) stay
	// separate; clones/similar (each keyed by a real input — a path prefix, a
	// path:line block) fold in.
	case "clones":
		return laneRoute{lane: "clones"}, true
	case "similar":
		return laneRoute{lane: "similar"}, true
	default:
		return laneRoute{}, false
	}
}

// classifyQuery is the pure routing decision for the read verb: input shape (or
// an explicit kind) → the lane that serves it. It is the union of two shipped
// classifiers — classifyLookTarget (exact-fetch shapes) and the symbol-vs-prose
// gate that ResolveIntent needs — with NO I/O so the whole ladder is
// table-testable.
//
// Returns the laneRoute, the cleaned argument for that lane, the detected shape
// (for the legible route), and the alternative interpretation not taken.
func classifyQuery(raw, kindOverride string) (lr laneRoute, cleaned, detected string, alt []QueryAlt) {
	t := strings.TrimSpace(raw)

	// A since/diff seed (#219): `since:<ref>` or `diff:<ref>` resolves to the
	// symbol set a diff touches, via the same range logic review_diff uses.
	// Checked first since it also uses ':' and only when no kind is forced.
	if strings.TrimSpace(kindOverride) == "" {
		if ref, ok := parseSinceSeed(t); ok {
			return laneRoute{lane: "since"}, ref, "since", nil
		}
	}

	// A selector-grammar seed (#210): every whitespace token is `field:pattern`
	// with a known field. Checked before the path:line / classifyLookTarget
	// shapes (which also use ':') so `pkg:store` / `func:*Handler` route to the
	// selector lane rather than being misread as a location. Only when no kind is
	// forced — an explicit kind wins below.
	if strings.TrimSpace(kindOverride) == "" && isSelectorQuery(t) {
		return laneRoute{lane: "select"}, t, "selector", nil
	}

	// An explicit kind wins outright — but still strip /regex/ delimiters so the
	// grep pattern is clean, matching lookVerb's behaviour.
	if strings.TrimSpace(kindOverride) != "" {
		if lr, ok := kindToLane(kindOverride); ok {
			cleaned = t
			if lr.lane == "grep" && len(t) >= 2 && strings.HasPrefix(t, "/") && strings.HasSuffix(t, "/") {
				cleaned = t[1 : len(t)-1]
			}
			return lr, cleaned, "forced", nil
		}
		// An unrecognized kind must fail loudly (#231) — silently falling back to
		// shape-detection would make a typo'd kind indistinguishable from an
		// intentional omission.
		return laneRoute{lane: "invalid-kind"}, t, "invalid-kind", nil
	}

	// path:N-M range → a precise slice via the read lane's lines mode (locate
	// handles only single lines).
	if m := pathLineRange.FindStringSubmatch(t); m != nil && looksLikePath(m[1]) {
		return laneRoute{lane: "read", mode: "lines:" + m[2] + "-" + m[3]}, m[1], "location", nil
	}

	// Shape detection: reuse the exact-fetch classifier, then split its `trace`
	// catch-all into symbol (graph) vs prose (semantic).
	lookKind, lookCleaned := classifyLookTarget(t)
	switch lookKind {
	case "grep":
		return laneRoute{lane: "grep"}, lookCleaned, "regex", nil
	case "locate":
		return laneRoute{lane: "locate"}, lookCleaned, "location", nil
	case "read":
		return laneRoute{lane: "read"}, lookCleaned, "path", nil
	case "trace":
		// The one new decision the merge introduces.
		if looksLikeSymbol(lookCleaned) {
			return laneRoute{lane: "symbol"}, lookCleaned,
				"symbol", []QueryAlt{{Kind: "search", Why: "treat it as a behavior query instead of a symbol name"}}
		}
		return laneRoute{lane: "semantic"}, lookCleaned, "prose", nil
	default:
		// classifyLookTarget only returns the four above; treat anything else as prose.
		return laneRoute{lane: "semantic"}, lookCleaned, "prose", nil
	}
}

func queryHandler(h toolSurface) func(context.Context, *sdk.CallToolRequest, QueryInput) (*sdk.CallToolResult, QueryOutput, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in QueryInput) (*sdk.CallToolResult, QueryOutput, error) {
		// Dispatch through the surface's query method so query is first-class: the
		// local Server composes queryVerb over its lanes, while the remote shim
		// (remoteClient.query) collapses the whole call into one POST /query round
		// trip instead of composing lane-by-lane over the network (#207 serve).
		return h.query(ctx, req, in)
	}
}

// query is *Server's first-class query handler: it composes queryVerb over the
// local lanes. It is the surface method queryHandler dispatches to, so the local
// stdio path is unchanged while the remote shim can override with a single-round
// -trip implementation. A non-empty ProjectRoots (#221) branches to the fan-out
// path instead — orchestration only, no new lane logic.
func (s *Server) query(ctx context.Context, req *sdk.CallToolRequest, in QueryInput) (*sdk.CallToolResult, QueryOutput, error) {
	if len(in.ProjectRoots) > 0 {
		return dispatchFanout(ctx, s, req, in)
	}
	return queryVerb(ctx, s, req, in)
}

// Query is the exported REST entry point for the `dex serve` /query route — the
// first-class server-side query endpoint (#207). It runs the full lane
// composition server-side so a container agent (moongit) reaches a whole query
// in one round trip rather than several. Mirrors Trace/Locate/Summarize.
func (s *Server) Query(ctx context.Context, in QueryInput) (QueryOutput, error) {
	if len(in.ProjectRoots) > 0 {
		_, out, err := dispatchFanout(ctx, s, nil, in)
		return out, err
	}
	_, out, err := queryVerb(ctx, s, nil, in)
	return out, err
}

// dispatchFanout runs in independently, once per resolved root, and returns
// the labeled per-project results as QueryOutput.Fanout (#221). Each root
// reruns the SAME single-lane or pipe path an equivalent single-project query
// would (via queryVerb) — no new lane logic, no merging, no cross-project
// graph joins. A per-project failure (unresolvable root, lane error) becomes
// that entry's status/error; it does not abort the other projects' queries.
func dispatchFanout(ctx context.Context, s *Server, req *sdk.CallToolRequest, in QueryInput) (*sdk.CallToolResult, QueryOutput, error) {
	roots, err := expandProjectRoots(ctx, s, in.ProjectRoots)
	if err != nil {
		return nil, QueryOutput{Status: "error", Hint: err.Error()}, nil
	}
	fanout := make([]QueryFanout, len(roots))
	var wg sync.WaitGroup
	for i, root := range roots {
		wg.Add(1)
		go func(i int, root string) {
			defer wg.Done()
			sub := in
			sub.ProjectRoots = nil
			sub.ProjectRoot = root
			_, out, qerr := queryVerb(ctx, s, req, sub)
			result := &QueryFanoutResult{
				Status: out.Status, Hint: out.Hint, Route: out.Route,
				Result: out.Result, Refs: out.Refs, Trust: out.Trust,
				Cost: out.Cost, Next: out.Next,
			}
			switch {
			case qerr != nil:
				fanout[i] = QueryFanout{Root: root, Status: "error", Error: qerr.Error(), Result: result}
			case out.Status != "" && out.Status != "ok":
				fanout[i] = QueryFanout{Root: root, Status: out.Status, Result: result}
			default:
				fanout[i] = QueryFanout{Root: root, Status: "ok", Result: result}
			}
		}(i, root)
	}
	wg.Wait()
	return nil, QueryOutput{Status: "ok", Fanout: fanout}, nil
}

// expandProjectRoots resolves the request's project_roots list: the literal
// entry "all" expands to every project this server's index dir knows about
// (proj.KnownRoots — the same discovery `dex reindex --all` uses); every
// other entry passes through unchanged (resolved later, per-root, by the
// same resolveProject an ordinary single-project query already uses — an
// unindexed explicit path surfaces as that entry's own no-index status, not
// a fan-out-wide error). Duplicates (e.g. "all" plus an explicit root also
// discovered by it) are collapsed so a project isn't queried twice.
func expandProjectRoots(ctx context.Context, s *Server, roots []string) ([]string, error) {
	seen := make(map[string]bool, len(roots))
	var out []string
	add := func(root string) {
		if !seen[root] {
			seen[root] = true
			out = append(out, root)
		}
	}
	for _, r := range roots {
		if r != "all" {
			add(r)
			continue
		}
		known, err := proj.KnownRoots(ctx, s.IndexDir, nil)
		if err != nil {
			return nil, fmt.Errorf("discover known projects: %w", err)
		}
		for _, k := range known {
			add(k)
		}
	}
	return out, nil
}

// queryVerb classifies the input, dispatches to the existing lane handler, and
// hoists its envelope into QueryOutput with a legible route. The exact and graph
// lanes go through lookVerb (which owns read/grep/locate/trace); the semantic
// lane goes through contextRouter (which owns ResolveIntent + composition).
func queryVerb(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput) (*sdk.CallToolResult, QueryOutput, error) {
	input := strings.TrimSpace(in.Input)
	if input == "" && strings.TrimSpace(in.Kind) == "" {
		// An empty input with NO forced kind is the session-start orientation
		// signal, exactly as an empty ask question is (contextRouter →
		// orientResponse). A forced kind always wins outright (#849) — an
		// empty-input zero-subject kind (status; check, which reads Claims
		// instead of Input) falls through to the normal classify/dispatch path
		// below rather than being silently overridden to orient.
		return dispatchSemantic(ctx, h, req, in, laneRoute{lane: "semantic"}, "", QueryRoute{Input: "", Detected: "empty", Lane: "semantic"})
	}

	// A top-level `|` composes lanes into a pipe (#206): seed | transform | … |
	// terminal, run left-to-right over the uniform Selection currency. A length-1
	// pipe never reaches here (splitPipe returns one segment → this predicate is
	// false), so the single-lane path below is byte-for-byte unchanged.
	if segments := splitPipe(input); len(segments) > 1 {
		return runPipe(ctx, h, req, in, segments)
	}

	// Single-lane path (the common case): classify the shape and dispatch. Shared
	// with the pipe seed via dispatchSingle. A prose query still feeds the #610
	// adaptive-compression task signal (writeCurrentTask, inside dispatchSingle);
	// exact-lane lookups are navigation, not a task, so they don't.
	return dispatchSingle(ctx, h, req, in)
}

// dispatchExact routes the read/grep/locate/symbol lanes through lookVerb, which
// already owns them, and wraps its LookOutput. `want` maps to the read mode or
// the graph direction; kindToLane's direction wins when kind forced a graph facet.
func dispatchExact(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput, lr laneRoute, cleaned string, route QueryRoute) (*sdk.CallToolResult, QueryOutput, error) {
	li := LookInput{
		Target: cleaned, K: in.K, To: in.To,
		Context: in.Context, Fixed: in.Fixed,
		ProjectRoot: in.ProjectRoot, Budget: in.Budget,
	}
	switch lr.lane {
	case "read":
		li.Kind = "read"
		mode := in.Want
		if lr.mode != "" { // a range slice carries its mode from the classifier
			mode = lr.mode
		}
		switch {
		case mode == "":
			// The read lane is code INTELLIGENCE, not a file server: a bare path
			// defaults to the compressed signatures view (~10× smaller), which
			// native Read cannot produce (#196 decision). Raw bytes are native
			// Read's job — see the want=full redirect below.
			li.Mode = "signatures"
		case mode == "full":
			return redirectToNativeRead(cleaned, route)
		default:
			li.Mode = mode
		}
	case "grep":
		li.Kind = "grep"
	case "locate":
		li.Kind = "locate"
	case "symbol":
		li.Kind = "trace"
		// Direction from a forced kind (callers/callees/impact/path) wins; else
		// from want; else lookVerb defaults to callers.
		if lr.direction != "" {
			li.Direction = lr.direction
		} else if in.Want != "" {
			li.Direction = in.Want
		}
	}

	_, lo, err := lookVerb(ctx, h, req, li)
	// The symbol input shape routes to the trace lane; report route.lane as the
	// wire lane that names the populated field (result.trace), keeping detected
	// as the "symbol" input shape (#95g §4 — one operation name on the wire).
	if lr.lane == "symbol" {
		route.Lane = "trace"
	}
	// Unwrap the LookOutput envelope into the flat lane (#95g): exactly one of
	// these is populated, matching route.lane. The envelope fields hoist below.
	out := QueryOutput{
		Status: lo.Status,
		// lo.Hint carries the exact-lane explanation (e.g. an unrecognized
		// read mode's guidance) — dropping it here left an exact-lane error
		// rendering as a bare "status: error" with no reason (#854).
		Hint:  lo.Hint,
		Route: route,
		Result: QueryResult{
			Read:   lo.Result.Read,
			Grep:   lo.Result.Grep,
			Trace:  lo.Result.Trace,
			Locate: lo.Result.Locate,
		},
		Trust: lo.Trust,
		Cost:  lo.Cost,
		Next:  lo.Next,
	}
	out.Refs = refsFromExact(out.Result) // uniform Selection currency (#95f)
	// On an empty exact-lane result the road-not-taken is a genuine fallback:
	// offer the search lane in next, not just as a passive alt (#231 — this
	// used to be symbol-only, leaving grep/locate no-matches dead-ended).
	if isEmptyStatus(lo.Status) {
		switch lr.lane {
		case "symbol":
			out.Next = append(out.Next, searchFallbackNext(cleaned, "no symbol by that name — search for the behavior instead"))
		case "grep":
			out.Next = append(out.Next, searchFallbackNext(cleaned, "no literal/regex match — search for the behavior instead"))
		case "locate":
			out.Next = append(out.Next, searchFallbackNext(cleaned, "no location at that path/line — search for the behavior instead"))
		}
	}
	return nil, out, err
}

// dispatchSemantic routes the semantic/intent lane through contextRouter, which
// owns ResolveIntent and the composition lanes. A forced intent (from kind) or a
// `want` facet maps onto ContextInput.
func dispatchSemantic(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput, lr laneRoute, cleaned string, route QueryRoute) (*sdk.CallToolResult, QueryOutput, error) {
	ci := ContextInput{
		Question: cleaned, K: in.K,
		ProjectRoot: in.ProjectRoot, Budget: in.Budget,
	}
	if lr.intent != "" {
		ci.Intent = lr.intent
	}
	// want on the prose lane: 'answer' turns on synthesis; 'assemble' forces the
	// working-set intent (unless a kind already forced one).
	switch strings.ToLower(strings.TrimSpace(in.Want)) {
	case "answer":
		ci.AnswerStyle = "brief"
	case "assemble":
		if ci.Intent == "" {
			ci.Intent = "assemble"
		}
	}

	_, co, err := h.contextRouter(ctx, req, ci)
	// Project the router's internal ContextOutput into the flat lane its resolved
	// intent names (#95g), and refine route.lane from the coarse "semantic" to the
	// actual sub-lane (orient/review/semantic).
	qr, lane := semanticLane(&co)
	route.Lane = lane
	// contextRouter owns its own trust shape inside ContextOutput; project it into
	// the envelope's EnvTrust so query's top-level trust is uniform. Semantic
	// provenance unless the router resolved a deterministic (orient/topology) lane.
	out := QueryOutput{
		Status: co.Status,
		Hint:   co.Hint,
		Route:  route,
		Result: qr,
		Refs:   refsFromSemantic(&co), // uniform Selection currency (#95f)
		Trust:  semanticTrustFrom(&co),
		Next:   co.Next,
	}
	return nil, out, err
}

// semanticTrustFrom projects contextRouter's own trust envelope onto the query
// envelope's EnvTrust. contextRouter already computes the authoritative
// confidence/freshness/recall shape (context.go), so query reuses it verbatim;
// the fallback is a bare semantic provenance for the rare no-lane early return.
func semanticTrustFrom(co *ContextOutput) EnvTrust {
	if co != nil && co.Trust != nil {
		return *co.Trust
	}
	return EnvTrust{Provenance: "semantic"}
}

// redirectToNativeRead answers a want=full request without duplicating a dumb
// cat: query is intelligence-only, so raw file content is deferred to the
// harness's native Read tool (#196 decision). The envelope stays legible — it
// says what to do instead rather than silently returning a compressed view.
func redirectToNativeRead(path string, route QueryRoute) (*sdk.CallToolResult, QueryOutput, error) {
	return nil, QueryOutput{
		Status: "use-native-read",
		Route:  route,
		Trust:  EnvTrust{Provenance: "exact", Caveat: "raw file content is the native Read tool's job; query serves compressed structural views (signatures/map/skeleton) and slices"},
		Next: []NextStep{
			{Verb: "query", Args: map[string]any{"input": path}, Why: "compressed signatures view of this file (query's default)"},
			{Verb: "query", Args: map[string]any{"input": path, "want": "map"}, Why: "imports + exports only"},
		},
	}, nil
}

// searchFallbackNext builds the "try the search lane instead" NextStep every
// exact/selector lane offers on a dead end — one shared shape (#231 review
// fix) instead of three hand-copied literals that could drift out of sync.
func searchFallbackNext(cleaned, why string) NextStep {
	return NextStep{
		Verb: "query",
		Args: map[string]any{"input": cleaned, "kind": "search"},
		Why:  why,
	}
}

// isEmptyStatus reports whether a lane status means "found nothing" (vs an
// error or a hit), so query can offer a fallback lane in next.
func isEmptyStatus(s string) bool {
	switch s {
	case "not-found", "no-matches", "no-graph", "no-match":
		return true
	default:
		return false
	}
}
