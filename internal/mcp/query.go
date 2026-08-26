package mcp

import (
	"context"
	"regexp"
	"strings"

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
	Input string `json:"input" jsonschema:"what to read. Its SHAPE picks the lane: a file path ('internal/mcp/server.go') → its compressed signatures (for raw bytes use the native Read tool), a location ('server.go:829') or range ('server.go:120-140') → that slice, a regex ('/func .*Verb/') → grep, a bare symbol ('NewServer', '(*Server).Run', 'mcp.NewServer') → its call graph, and a prose question ('how are edits debounced?') → a ranked semantic evidence pack. Output precision tracks input precision."`
	// Kind forces the lane, bypassing shape detection.
	Kind string `json:"kind,omitempty" jsonschema:"force the lane instead of inferring it from input shape: read|grep|locate (exact) · symbol|callers|callees|impact|path (graph) · search|editing|assemble|architecture|packages|orient|review (semantic/intent)"`
	// Want picks the facet within the chosen lane.
	Want string `json:"want,omitempty" jsonschema:"pick the facet within the lane: for a file 'signatures' (default) | 'map' (imports+exports) | 'skeleton' | 'lines:N-M' (slice) — raw full content is the native Read tool's job, not query's; for a symbol 'callers|callees|impact|path'; for a prose pack 'answer' (synthesized prose) or 'assemble' (budget-bounded working set)"`
	To   string `json:"to,omitempty" jsonschema:"destination symbol for the graph 'path' facet (shortest call route from input to this symbol)"`
	// grep pass-through.
	Context int  `json:"context,omitempty" jsonschema:"grep lane only: lines of surrounding context per match (0-10)"`
	Fixed   bool `json:"fixed,omitempty" jsonschema:"grep lane only: treat the pattern as a literal string, not a regex"`
	// shared.
	K           int    `json:"k,omitempty" jsonschema:"max results per lane"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
	Budget      int    `json:"budget,omitempty" jsonschema:"optional context-token budget; when set, the response reports cost.budget_left = budget − tokens_returned"`
}

func (in QueryInput) budgetTokens() int { return in.Budget }

// QueryRoute is the legible routing decision (spec §"the universal envelope"):
// what shape dex detected in the input, which lane it chose, and the alternative
// interpretation it did NOT take. Routing is never silent guessing — an
// ambiguous input names the road not taken so the agent can force it via `kind`.
type QueryRoute struct {
	Input    string     `json:"input"`
	Detected string     `json:"detected"` // the input shape: path|location|regex|symbol|prose
	Lane     string     `json:"lane"`     // the lane chosen: read|grep|locate|symbol|semantic
	Forced   bool       `json:"forced,omitempty"`
	Alt      []QueryAlt `json:"alt,omitempty"`
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
}

// QueryOutput is the universal envelope for the read verb. Status/Trust/Cost/Next
// are hoisted from whichever lane ran so an agent reads them the same way for
// every input shape; Result carries the full lane payload for detail.
type QueryOutput struct {
	Status string      `json:"status"`
	Route  QueryRoute  `json:"route"`
	Result QueryResult `json:"result"`
	Trust  EnvTrust    `json:"trust"`
	Cost   *EnvCost    `json:"cost,omitempty"`
	Next   []NextStep  `json:"next,omitempty"`
}

func (o *QueryOutput) stampCost(t, left int) { o.Cost = withCost(o.Cost, t, left) }

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
		return queryVerb(ctx, h, req, in)
	}
}

// queryVerb classifies the input, dispatches to the existing lane handler, and
// hoists its envelope into QueryOutput with a legible route. The exact and graph
// lanes go through lookVerb (which owns read/grep/locate/trace); the semantic
// lane goes through contextRouter (which owns ResolveIntent + composition).
func queryVerb(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput) (*sdk.CallToolResult, QueryOutput, error) {
	input := strings.TrimSpace(in.Input)
	if input == "" {
		// An empty input is the session-start orientation signal, exactly as an
		// empty ask question is (contextRouter → orientResponse).
		return dispatchSemantic(ctx, h, req, in, laneRoute{lane: "semantic"}, "", QueryRoute{Input: "", Detected: "empty", Lane: "semantic"})
	}

	lr, cleaned, detected, alt := classifyQuery(input, in.Kind)
	route := QueryRoute{Input: input, Detected: detected, Lane: lr.lane, Forced: detected == "forced", Alt: alt}

	switch lr.lane {
	case "read", "grep", "locate", "symbol":
		return dispatchExact(ctx, h, req, in, lr, cleaned, route)
	default: // semantic
		// A prose query is the agent declaring what it is working on — the task
		// source for the #610 adaptive-compression feedback loop now that
		// session(set_task) is gone (#195 S4). Exact-lane lookups (path/regex/
		// symbol) are navigation, not a task, so they don't feed it.
		writeCurrentTask(input)
		return dispatchSemantic(ctx, h, req, in, lr, cleaned, route)
	}
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
		Route:  route,
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
	// On an empty symbol result the road-not-taken is a genuine fallback: offer
	// the search lane in next, not just as a passive alt.
	if lr.lane == "symbol" && isEmptyStatus(lo.Status) {
		out.Next = append(out.Next, NextStep{
			Verb: "query",
			Args: map[string]any{"input": cleaned, "kind": "search"},
			Why:  "no symbol by that name — search for the behavior instead",
		})
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
		Route:  route,
		Result: qr,
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
