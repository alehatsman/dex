// Package mcp wires the dex toolset onto the official MCP Go SDK
// and runs it over stdio.
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolSurface is the set of tool handlers registerTools wires onto an MCP
// server. *Server implements it against a local on-disk index; the remote
// shim's *remoteClient (remote.go) implements it by proxying each call to a
// `dex serve` REST endpoint. Funnelling both through one registerTools means
// the stdio and remote surfaces can never drift in tool names, JSON schemas,
// or descriptions — the schema for each tool is derived by the SDK from the
// shared Input type, so both backends advertise byte-identical tools.
type toolSurface interface {
	contextRouter(context.Context, *sdk.CallToolRequest, ContextInput) (*sdk.CallToolResult, ContextOutput, error)
	locate(context.Context, *sdk.CallToolRequest, LocateInput) (*sdk.CallToolResult, LocateOutput, error)
	review(context.Context, *sdk.CallToolRequest, ReviewInput) (*sdk.CallToolResult, ReviewOutput, error)
	refactor(context.Context, *sdk.CallToolRequest, RefactorInput) (*sdk.CallToolResult, RefactorOutput, error)
	rehearse(context.Context, *sdk.CallToolRequest, RehearseInput) (*sdk.CallToolResult, RehearseOutput, error)
	cohort(context.Context, *sdk.CallToolRequest, CohortInput) (*sdk.CallToolResult, CohortOutput, error)
	search(context.Context, *sdk.CallToolRequest, SearchInput) (*sdk.CallToolResult, SearchOutput, error)
	findSymbol(context.Context, *sdk.CallToolRequest, FindSymbolInput) (*sdk.CallToolResult, FindSymbolOutput, error)
	related(context.Context, *sdk.CallToolRequest, RelatedInput) (*sdk.CallToolResult, RelatedOutput, error)
	graphDeps(context.Context, *sdk.CallToolRequest, GraphDepsInput) (*sdk.CallToolResult, GraphDepsOutput, error)
	graphCallers(context.Context, *sdk.CallToolRequest, CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error)
	graphCallees(context.Context, *sdk.CallToolRequest, CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error)
	graphImpact(context.Context, *sdk.CallToolRequest, ImpactInput) (*sdk.CallToolResult, ImpactOutput, error)
	graphLinks(context.Context, *sdk.CallToolRequest, DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error)
	graphBacklinks(context.Context, *sdk.CallToolRequest, DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error)
	graphTags(context.Context, *sdk.CallToolRequest, TagInput) (*sdk.CallToolResult, TagOutput, error)
	graphCycles(context.Context, *sdk.CallToolRequest, CyclesInput) (*sdk.CallToolResult, CyclesOutput, error)
	graphPath(context.Context, *sdk.CallToolRequest, PathInput) (*sdk.CallToolResult, PathOutput, error)
	graphDiff(context.Context, *sdk.CallToolRequest, DiffInput) (*sdk.CallToolResult, DiffOutput, error)
	graphCommunities(context.Context, *sdk.CallToolRequest, CommunitiesInput) (*sdk.CallToolResult, CommunitiesOutput, error)
	smells(context.Context, *sdk.CallToolRequest, SmellsInput) (*sdk.CallToolResult, SmellsOutput, error)
	clones(context.Context, *sdk.CallToolRequest, ClonesInput) (*sdk.CallToolResult, ClonesOutput, error)
	routes(context.Context, *sdk.CallToolRequest, RoutesInput) (*sdk.CallToolResult, RoutesOutput, error)
	searchTree(context.Context, *sdk.CallToolRequest, SearchTreeInput) (*sdk.CallToolResult, SearchTreeOutput, error)
	searchGrep(context.Context, *sdk.CallToolRequest, SearchGrepInput) (*sdk.CallToolResult, SearchGrepOutput, error)
	check(context.Context, *sdk.CallToolRequest, CheckInput) (*sdk.CallToolResult, CheckOutput, error)
	refs(context.Context, *sdk.CallToolRequest, RefsInput) (*sdk.CallToolResult, RefsOutput, error)
	status(context.Context, *sdk.CallToolRequest, StatusInput) (*sdk.CallToolResult, StatusOutput, error)
	summarize(context.Context, *sdk.CallToolRequest, SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error)
	indexStatus(context.Context, *sdk.CallToolRequest, IndexStatusInput) (*sdk.CallToolResult, IndexStatusOutput, error)
}

// addTool registers h on srv with a panic recovery guard. A handler panic is
// converted to a structured tool error instead of crashing the MCP session.
// (The go-sdk v1.6.1 dispatch loop has no recover() of its own.)
func addTool[In, Out any](srv *sdk.Server, t *sdk.Tool, h sdk.ToolHandlerFor[In, Out]) {
	sdk.AddTool(srv, t, func(ctx context.Context, req *sdk.CallToolRequest, in In) (res *sdk.CallToolResult, out Out, err error) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("mcp handler panic", "tool", req.Params.Name, "panic", r, "stack", string(debug.Stack()))
				err = fmt.Errorf("internal error: handler panic: %v", r)
				res = &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("internal error: %v", r)}}}
			}
		}()
		// Carry the client session so resolveProject can consult the caller's
		// declared workspace roots (#120) without threading req through every
		// handler.
		res, out, err = h(withSession(ctx, req.Session), req, in)
		// Stamp the uniform cost envelope (#110 step 2): tokens_returned on every
		// four-verb success, plus budget_left when the input carried a budget.
		// No-op for tools whose output doesn't implement costStamper.
		if err == nil {
			stampEnvelopeCost(&out, in)
		}
		return res, out, err
	})
}

// registerTools wires the dex tool surface onto srv, dispatching to h.
// Exposure is capability-derived (#283/#290): the embedder-backed lanes
// (semantic search behind `find`/`ask`) are only registered when
// embedAvailable is true. With no embedder wired (the lean profile,
// DEX_EMBED_ENGINE=none), those are omitted entirely and the surface degrades
// to BM25 + symbol + graph + file lanes (reached via `look` and `ask`, which
// routes to the non-semantic lanes). After the 5c collapse (#145) the always-on
// floor is the index-free verbs `ask` + `look` (fetch) + `act` (run); the raw
// primitives they subsume — `shell`, `grep`, `read` — moved to the expert lane.
//
// Named profiles, not a boolean matrix (#110 step 8, spec: tool-surface.md).
// The deployment shape is one of three profiles — full (embedder+chat),
// bm25-only (no embedder), lean (weak local model) — but the VERB SET is
// constant across all three: a single read verb, query. A profile changes only
// query's internal capability (synthesis → lexical → hits-only, degraded at call
// time from embed/chat), never which tools an agent sees. DEX_EXPERT is an
// additive power-lane overlay, orthogonal to the profile — never a different
// shape of the everyday surface.
func registerTools(srv *sdk.Server, h toolSurface, embedAvailable bool, descMode DescriptionMode) {
	td := func(s string) string { return compressToolDesc(s, descMode) }

	// The single-verb surface (#205, supersedes the two-verb pivot #197): query
	// (read the codebase intelligence). dex is retrieval, not agent memory —
	// durable findings are the harness's job (file-based memory), so the `record`
	// write verb and the whole L3 knowledge subsystem are gone.
	registerQueryTool(srv, h, td) // query — the single read verb (merges ask + look)

	// DEX_EXPERT overlays the granular power lanes additively, in any profile.
	if expertEnabled() {
		registerExpertTools(srv, h, td, embedAvailable)
	}
}

// registerExpertTools wires the power lanes behind DEX_EXPERT (#125):
// deps/graph/refactor/quality tools kept off the everyday surface.
func registerExpertTools(srv *sdk.Server, h toolSurface, td func(string) string, embedAvailable bool) {
	if embedAvailable {
		// Raw ranked hits with the full scoring breakdown (bm25/rrf/lane
		// scores). Everyday concept-search is covered by ask(behavior_search);
		// this power lane exposes the underlying ranking for debugging and
		// precise, filtered queries (#142, demoted from the everyday surface).
		addTool(srv, &sdk.Tool{
			Name:        "search",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("Hybrid semantic + BM25 search — raw ranked hits with the full scoring breakdown. " +
				"For everyday concept-search prefer ask (it routes behavior_search and fuses the same lanes); reach " +
				"for search when you need the raw ranking or precise filters. Identifier tokens (CamelCase, " +
				"snake_case, qualified names) are automatically looked up by exact symbol name and fused via " +
				"Reciprocal Rank Fusion — no separate lookup call needed. Supports exclude list, 'languages', and " +
				"'path_glob' filters. On error: 'no-index' (run dex index first), 'embedding-service-unreachable' " +
				"(fall back to grep), or 'ok'."),
		}, h.search)
	}

	// trace + locate: the call-graph and orientation lanes. Demoted from the
	// everyday surface (#143) — everyday agents reach them through `look`
	// (a bare symbol → trace callers, a path:line → locate) and through
	// ask(intent=callers|callees|symbol_lookup|orient). Kept here as direct
	// power lanes for path/impact and precise queries.
	addTool(srv, &sdk.Tool{
		Name:        "trace",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Walk the static call graph from a symbol. `direction`: 'callers' (default — " +
			"who calls it), 'callees' (what it calls), 'path' (shortest call route to the `to` symbol), or " +
			"'impact' (transitive caller blast-radius up to max_depth (default 3): every reachable function with " +
			"its hop depth + PageRank, a risk tier, and `tests_to_run` — the sibling tests of the blast-radius " +
			"files, so change→verify is one call (#654)). " +
			"Go edges are type-resolved; Python/JS/TS/Rust/Java are name-based (tree-sitter) with incomplete " +
			"recall, so an empty result there is not proof of none — verify with grep. Non-empty non-Go " +
			"results are tagged `recall:partial` (callers/callees also fold a grep sweep into `grep_hits`; " +
			"impact just flags the radius as possibly larger). TypeScript additionally resolves constructor-DI " +
			"dispatch — `this.dep.method()` binds to the injected type's method (#85). For a Go method that " +
			"implements a project interface, callers (and impact) also include the INTERFACE-dispatch call sites " +
			"(calls through the interface value), each tagged with `via` naming the interface method — so dynamic " +
			"dispatch isn't missed (#604). Accepts a bare name ('Foo'), " +
			"receiver-qualified ('(*Server).Run'), or package-tail-qualified ('mcp.NewServer'). " +
			"Returns 'no-graph' when calls edges haven't been indexed (`dex index . --graph=only`)."),
	}, traceHandler(h))

	addTool(srv, &sdk.Tool{
		Name:        "locate",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("One-call orientation around a code location. Give any one of `ref` " +
			"('path:line'), `symbol` (a name), or `frame` (a raw stack-trace line) and locate resolves " +
			"the enclosing symbol, then returns its callers (+ risk tier), sibling test files, the nearest " +
			"doc (CLAUDE.md/doc.go/README.md walking up), the file's last commit + author, and related " +
			"project notes — in one response. Pass `issues: true` to also list matching open GitHub issues " +
			"via `gh` (best-effort). " +
			"To batch-verify many cited 'file:line[:symbol]' locations in one call (e.g. confirm citations " +
			"from notes/memory still resolve), use the `check` verb instead. " +
			"Pure composition over the index; needs no chat model. Degrades cleanly: " +
			"callers are empty when the graph isn't indexed; returns 'no-index' / 'not-found' otherwise. " +
			"Reach for locate when you already have a concrete path:line, symbol, or panic frame to orient on — " +
			"ask remains the primary entry point for open-ended questions."),
	}, h.locate)

	// The raw primitives the verbs subsume (#145): grep + read (look routes
	// /regex/→grep, path→read), and review_diff (ask("review my changes") covers
	// the everyday worktree case #144 — this is the targeted ref/branch/pr escape
	// hatch). All index-free, so unconditional.
	addTool(srv, &sdk.Tool{
		Name:        "grep",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Exact RE2 regex search over indexed project files — no embedding required. " +
			"Use for literal pattern matches that semantic search misses: cross-cutting symbol references, " +
			"import paths, string literals, regex-sensitive identifiers. Also the primary search lane when " +
			"the embedding service is unavailable. " +
			"Searches the indexed file list when available (respects .gitignore); " +
			"falls back to walking the project directory and skipping .git/vendor/node_modules. " +
			"Returns up to max_results matches (default 50) with path, line number, and trimmed content. " +
			"Pass `context` (1-10) for surrounding lines (like grep -C). " +
			"Pass `fixed:true` to match literally (like grep -F) — no escaping needed for foo.bar, arr[i], f(x). " +
			"Returns 'no-matches' when nothing matches."),
	}, h.searchGrep)

	addTool(srv, &sdk.Tool{
		Name:        "read",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Fetch exact source context for a file you already know. " +
			"Prefer `look(path)` — the fetch verb routes here; use `read` directly for its full mode/slice surface. " +
			"`mode` (default 'full') selects the view: 'full' = raw file content (no LLM, exact bytes); " +
			"'signatures' = indexed symbols + source lines; 'skeleton' = exported type declarations in full plus " +
			"function/method signatures with @B<n> body handles (expand one later via expand='@B<n>'); " +
			"'map' = imports + exported symbols; 'lines:N-M' = raw line slice; " +
			"'analyze' = a token-cost comparison of every mode plus a recommended mode and NO file content, so you " +
			"can pick the cheapest sufficient view before paying to read it; its `handle` (#620) lets you analyze " +
			"many files then lazily expand only the ones you need via read(handle=…, mode=…); " +
			"'summary' = LLM-generated digest (the only mode needing a chat model — pass `focus` to steer, " +
			"e.g. 'public API surface'; returns status='needs-chat' when no chat model is wired). " +
			"Path must resolve inside project_root. Files larger than 64 KB are truncated. " +
			"Pass paths[] (up to 10) to read multiple files in one call — all use the same mode. " +
			"Re-read savings: every response includes `etag` (content hash). On re-reads pass that etag back; " +
			"if the file is unchanged the server returns status=unchanged — reuse the content already in context. " +
			"If the file changed since the last read the server may return status=delta with a compact unified diff " +
			"in Content (saves 40-60% tokens vs re-sending the full file); update your mental model from the diff. " +
			"Pass `task` (your current task from `session`) for automatic compression routing of the raw default. " +
			"Pass `ref` (a git revision: HEAD~5, v1.0, a sha) to time-travel — read the file AS OF that commit, " +
			"with mode=full (raw) or mode=signatures (the historical API); the file must still exist now (#644). " +
			"Any note whose `scope` binds the file is returned in `scoped_notes` (gotcha-on-touch, #645) — read it before you edit. " +
			"Pass `slice` to extract a surgical subset of the content without sending the whole file: " +
			"head:N (first N lines), tail:N (last N lines), range:L1-L2 (1-indexed inclusive), " +
			"search:PATTERN (RE2 grep with ±3 context lines, groups separated by ---), " +
			"json_path:EXPR (dot-path JSON extraction, e.g. $.dependencies). " +
			"Slice composes with handle: the handle resolves to a range first, then slice extracts within it. " +
			"On error, returns 'chat-service-unreachable' or 'error'."),
	}, h.summarize)

	addTool(srv, &sdk.Tool{
		Name:        "review_diff",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Per-hunk intelligence for a diff or PR — the targeted-selector review lane " +
			"(ask(\"review my changes\") covers the everyday worktree case, #144). " +
			"Give one of `ref` ('HEAD~3..HEAD' or a single ref vs HEAD), `branch` (what it adds since " +
			"diverging from the default branch), or `pr` (a GitHub PR number, resolved via `gh`). " +
			"For each changed hunk review returns the touched symbols, their callers (+ a risk tier from " +
			"caller blast radius and export status), and related notes; per file it adds sibling tests, the " +
			"nearest doc, 30-day churn, last commit/author, recent author history, and any notes whose " +
			"`scope` binds the file (gotcha-on-touch, #645). " +
			"Pass `compact: true` to drop low-risk hunks. Pure composition over the index; needs no chat " +
			"model. Degrades cleanly: callers/risk are empty when the graph isn't indexed (diff + churn " +
			"still returned); returns 'no-index' / 'no-changes' / 'not-found' otherwise."),
	}, h.review)

	// notes' admin/relate surface (delete, pin/unpin, gc, consolidate,
	// export/import, relate/relations, review) is CLI-only (`dex notes`, #147,
	// #195 S4): record covers the everyday write/recall/supersede hot path, and
	// the admin tail isn't an agent-facing effect. The knowledge engine (h.knowledge)
	// still backs record and the CLI; only the MCP tool is gone.

	// lookup is not a standalone tool — `find` already fuses exact
	// symbol-name hits via RRF, and `ask` detects identifiers and runs
	// the same lookup automatically. The findSymbol handler stays (it
	// backs find's fusion, locate's resolver, and the REST /lookup
	// route); only the redundant tool exposure is removed (#685).
	addTool(srv, &sdk.Tool{
		Name:        "deps",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Return the `imports` edges for a file or package — the package the file belongs to, " +
			"and the list of packages it depends on. Sourced from the static graph (no embedding, no chat). " +
			"Pass `path` (relative file inside the project) OR `package` (full package path). " +
			"Returns 'no-index' / 'no-graph' / 'not-found' when the project, graph, or symbol is missing."),
	}, h.graphDeps)
	// callers/callees are not standalone tools — `trace --dir
	// callers|callees` is the single call-graph entry point (#575).

	// plan_rename (#638): type-precise rename planner. Go-only, on-demand
	// (no index). Moved to expert: niche workflow, byte-offset input shape
	// that most agents don't construct naturally.
	addTool(srv, &sdk.Tool{
		Name:        "plan_rename",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Plan a type-precise rename and get back byte-exact edit triples to apply yourself " +
			"(dex never writes files). Set `op` to 'rename_symbol' (default), `symbol` to the target " +
			"(bare 'Foo', receiver-qualified '(*Server).Run', or pkg-qualified 'mcp.NewServer') and `to` to " +
			"the new name. Returns every (path, start_byte, end_byte, replacement) edit across the module, " +
			"resolved by the Go type checker — a method rename touches only that type's method, never " +
			"same-named methods elsewhere. Apply edits highest-offset-first per file. The `etag` echoes the " +
			"touched files' hash; pass it back to detect a stale plan. Go-only in v1 (returns " +
			"'unsupported-language' otherwise); also 'not-found' / 'ambiguous' / 'stale'. Loads packages " +
			"on-demand, so it is slower than the read verbs — reach for it when you're about to rename."),
	}, h.refactor)

	// rehearse_patch (#730): type-check a hypothetical edit in-memory.
	// Moved to expert: complex byte-range splice input, Go-only, rarely
	// called — agents typically edit then verify_change instead.
	addTool(srv, &sdk.Tool{
		Name:        "rehearse_patch",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Type-check a hypothetical edit in-memory and return new type errors, broken " +
			"files, and tests to run — without writing anything. Closes the chain: " +
			"`plan_rename` (plan) → `rehearse_patch` (prove it compiles) → `Edit` (apply) → `verify_change` (test). " +
			"Pass `edits` as byte-range splices — the same shape `plan_rename` emits (path, start_byte, " +
			"end_byte, replacement), applied highest-offset-first per file. Or pass `files` as whole-file " +
			"replacements (path + contents); `files` takes precedence over `edits` for the same path. " +
			"Returns `compiles` (bool), `diagnostics` (new type errors only — pre-existing errors are " +
			"diffed out), `broken_files` (paths with new errors), and `tests_to_run` (sibling test files). " +
			"Go-only in v1 (returns 'unsupported-language' for non-Go roots); also 'no-edits' when no " +
			"edits/files are supplied. Loads packages on-demand — slower than the read verbs."),
	}, h.rehearse)

	// check (#708): batch citation verification. Moved to expert: meta-tool
	// for QA of prior notes/citations, not primary coding workflow.
	addTool(srv, &sdk.Tool{
		Name:        "check",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Verify a batch of file:line[:symbol] references against the current index — " +
			"use this after making or reviewing changes to confirm that cited locations are still valid. " +
			"Pass `claims` as an array of {ref, symbol?} objects where `ref` is 'file:line', " +
			"'file:line:symbol', or 'file:symbol'. Each result has `status`: " +
			"ok (reference is valid), moved (symbol found at a different line in the same file, " +
			"with `found_at`), gone (symbol/line no longer indexed), no_file (path has no indexed " +
			"chunks), or parse_error (malformed ref). `symbol_at` reports what IS indexed at the " +
			"given line when the expected symbol does not match."),
	}, h.check)

	addTool(srv, &sdk.Tool{
		Name:        "routes",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Detect HTTP handlers, MCP tool registrations, and gRPC service implementations " +
			"from the call graph. Matches ServeHTTP implementations, handle*/serve*-named functions, " +
			"and callers of registration functions (Handle, HandleFunc, AddTool, RegisterService, etc.). " +
			"Returns each handler with its file location and the registration function that wires it in. " +
			"Requires a graph index (`dex index . --graph=only`)."),
	}, h.routes)

	addTool(srv, &sdk.Tool{
		Name:        "smells",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("AST-based code quality signals derived from the graph index — no LLM required. " +
			"Returns four categories: `long_functions` (bodies >= min_func_lines, default 80), " +
			"`dead_exports` (exported functions/methods with no indexed callers), " +
			"`god_files` (files with >= min_file_symbols symbols, default 30), and " +
			"`god_nodes` (functions/methods with in_degree >= min_god_node_callers (20) OR " +
			"cross_pkg_callers >= min_god_node_pkg_callers (8) — over-coupled symbols constraining many callers). " +
			"Requires a graph index (`dex index . --graph=only`). Use before a PR or refactor to spot obvious structural issues."),
	}, h.smells)

	// clones / similar (#84): semantic duplication detection over the
	// vectors already indexed for search. Vector-backed, so only wired
	// when an embedder is present.
	if embedAvailable {
		addTool(srv, &sdk.Tool{
			Name:        "clones",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("Find clusters of semantically near-duplicate code blocks (duplication hotspots) — " +
				"the highest-leverage output for review/refactor work, and something grep can't do (it matches " +
				"literals, not meaning). Scans indexed function/method blocks, KNNs each against the rest, and " +
				"union-finds the near-duplicate edges into clusters. Returns clusters of `{path, start_line, " +
				"end_line, kind, name}` with a `similarity` floor and `size`. Args: `path` (restrict to a file/dir " +
				"prefix), `threshold` (min cosine similarity, default 0.90), `min_lines` (default 6), `k`, " +
				"`max_clusters`. Reuses search vectors — no embedder round-trip; an index built without embeddings " +
				"returns none."),
		}, h.clones)

		addTool(srv, &sdk.Tool{
			Name:        "similar",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("Return code blocks across the repo semantically near a given block, ranked by " +
				"similarity. Point it at a block via `path` + `start_line` (the block indexed at that line); set " +
				"`threshold` (cosine similarity 0..1) to keep only genuine near-duplicates. Use it to answer " +
				"'where else is this logic implemented?' before editing or de-duplicating. Vector KNN over the " +
				"search index — no embedder round-trip."),
		}, h.related)
	}

	// cohort (#643): blast radius of an intent. Given an interface, list
	// the types you must edit in lockstep when its method set changes —
	// complete implementors plus near-misses (the backend you forgot).
	addTool(srv, &sdk.Tool{
		Name:        "cohort",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Find the types that must change together with an interface. Given an `interface` " +
			"name (bare 'toolSurface' or pkg-qualified 'mcp.toolSurface'), returns every type that " +
			"implements it ('complete') plus near-misses that implement most of it but are missing methods " +
			"('partial' — the backend you forgot to update), each with its declaration file:line and the " +
			"missing method names. Pure go/types — no index needed; Go-only (returns 'unsupported-language' " +
			"otherwise). Reach for it before adding/removing an interface method to plan the lockstep edit."),
	}, h.cohort)

	// refs (#604 Tier 1): type-precise Go symbol queries via go/types.
	// references — all def+use sites; implementations — concrete types
	// satisfying an interface; supertypes — embedded interfaces / interfaces
	// a type satisfies; subtypes — implementing types / embedding structs.
	addTool(srv, &sdk.Tool{
		Name:        "refs",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Type-precise Go symbol queries via go/types — no index needed; Go-only. " +
			"Give a `symbol` (bare 'Foo', receiver-qualified '(*Server).Run', or pkg-qualified 'mcp.NewServer') " +
			"and an `action`: " +
			"'references' (all def + use sites across the module), " +
			"'implementations' (concrete types satisfying an interface), " +
			"'supertypes' (interfaces embedded by an interface, or interfaces a concrete type satisfies within the module), " +
			"'subtypes' (types implementing an interface, or structs embedding a struct). " +
			"Returns a list of {path, line, kind} sites. Returns 'unsupported-language' for non-Go. " +
			"For interface implementors, `cohort` gives richer coverage-gap analysis; refs gives the raw query."),
	}, h.refs)

	// `path` is not a standalone tool — `trace --dir path --to <dst>`
	// finds the shortest route between two symbols (#575).
	// `diff` removed: review_diff + trace direction=impact cover blast-radius
	// from changed files. `budget` removed: session action=budget covers it.

	addTool(srv, &sdk.Tool{
		Name:        "clusters",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("List Louvain communities in the call/import graph — " +
			"clusters of tightly-interconnected symbols. " +
			"Communities are sorted by descending size. " +
			"Top members per community are sorted by PageRank. " +
			"Community IDs are stable across re-runs for unchanged subgraphs. " +
			"Requires a graph index (`dex index . --graph=only`). " +
			"Useful for understanding module boundaries, finding hidden coupling, and planning refactors."),
	}, h.graphCommunities)

	addTool(srv, &sdk.Tool{
		Name:        "repo_map",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Deterministic, multi-zoom topology map of the project's top packages/dirs " +
			"and how they connect — no embedding or chat required. " +
			"Use for structural exploration when you need raw topology rather than a task context pack. " +
			"For coding tasks, call `ask(task, intent=assemble)` instead — it returns ranked files and orientation together. " +
			"Returns 'no-index' when the project hasn't been indexed yet."),
	}, mapHandler(h))

	addTool(srv, &sdk.Tool{
		Name:        "status",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Report dex endpoint health and the list of indexed projects with their chunk counts and last-indexed times. " +
			"For everyday use, single-project index freshness is embedded in `ask` responses — call this for cross-project health checks or debugging."),
	}, h.status)

	// The session/checkpoint admin surface (set_task/add_note/snapshot/export/
	// import/budget/heatmap) was removed with the two-verb collapse (#195 S4):
	// running a working session is the harness's job, not an advisory effect.
	// The dedup that made it useful stays *internal* — sessionAutoFile tracks
	// touched files and the seen/delta mechanism suppresses re-sends, and the
	// current task still surfaces in query responses as session_task.
}

// registerQueryTool wires the single read verb of the two-verb surface (#196,
// epic #195, spec specs/two-verb-surface.md). query merges the four-verb `ask`
// (infer intent from a question) and `look` (exact fetch of a named target) into
// one classifier over the same engine — output precision tracks input precision.
// It BM25-falls-back on its own when no embedder is wired.
func registerQueryTool(srv *sdk.Server, h toolSurface, td func(string) string) {
	addTool(srv, &sdk.Tool{
		Name:        "query",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("The read verb — one call to read the codebase intelligence. Its input SHAPE picks " +
			"the lane and the answer's precision: a file path ('internal/mcp/server.go') → its compressed " +
			"signatures (raw bytes are the native Read tool's job), a `path:line` " +
			"('server.go:829') or range ('server.go:120-140') → that slice, a `/regex/` ('/func .*Verb/') → grep, a bare symbol ('NewServer', " +
			"'(*Server).Run', 'mcp.NewServer') → JUST its call graph, and a prose question ('how are edits " +
			"debounced?') → a ranked semantic evidence pack (semantic_hits + symbols + suggested_reads, contents " +
			"inlined). A named symbol earns a precise (graph) answer, never a fuzzy one — that is the narrow " +
			"default. Force the lane with `kind` (read|grep|locate|symbol|callers|callees|impact|path|search|" +
			"editing|assemble|architecture|packages|orient|review) and the facet with `want`. The envelope's " +
			"`route` echoes the shape it detected and the alternative it did not take; `trust` carries per-result " +
			"provenance (exact | semantic | name-based); `next` offers the road not taken. Empty input returns the " +
			"session-start orientation map."),
	}, queryHandler(h))
}

// Version is the build version. A release build overrides it via
// -ldflags "-X .../internal/mcp.Version=<v>" (mooncake task install). When it
// is still "dev" — e.g. a plain `go install` — resolveVersion (version.go)
// recovers the VCS revision Go embeds in the build info.
var Version = "dev"
