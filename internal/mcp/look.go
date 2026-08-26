package mcp

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// look is the "exact fetch" verb of the four-verb surface (#110): give it a
// concrete target — a file path, a `path:line` location, a `/regex/`, or a
// symbol name — and it returns exactly that, no inference. It is a deterministic
// router over the existing exact-fetch tools (read / grep / trace / locate),
// added additively so those tools stay valid as aliases until the cutover. Where
// `ask` infers, `look` fetches what you already know how to name.
//
// The routing is a pure string classifier (classifyLookTarget) so the target →
// lane decision is fully testable and never depends on I/O. An explicit `kind`
// overrides it for the rare ambiguous target.

// LookInput is the single-target fetch request. Only `target` is required; the
// rest are lane-specific pass-throughs applied when classification selects that
// lane.
type LookInput struct {
	Target string `json:"target" jsonschema:"what to fetch: a file path ('internal/mcp/server.go'), a location ('server.go:829'), a regex ('/func .*Verb/'), or a symbol ('NewServer', '(*Server).Run', 'mcp.NewServer'). dex classifies it and routes to read/grep/trace/locate."`
	// Kind forces the lane, bypassing classification. Values: read|grep|trace|locate.
	Kind string `json:"kind,omitempty" jsonschema:"force the fetch lane instead of auto-classifying the target: 'read' (file), 'grep' (regex), 'trace' (symbol call graph), or 'locate' (path:line orientation)"`
	// read pass-through.
	Mode string `json:"mode,omitempty" jsonschema:"read lane only: read mode (full|signatures|skeleton|map|lines:N-M|analyze). Default full."`
	// trace pass-through.
	Direction string `json:"direction,omitempty" jsonschema:"trace lane only: 'callers' (default), 'callees', 'path', or 'impact'"`
	To        string `json:"to,omitempty" jsonschema:"trace lane only: destination symbol when direction=path"`
	// grep pass-through.
	Context int  `json:"context,omitempty" jsonschema:"grep lane only: lines of surrounding context per match (0-10)"`
	Fixed   bool `json:"fixed,omitempty" jsonschema:"grep lane only: treat the pattern as a literal string, not a regex"`
	// shared.
	K           int    `json:"k,omitempty" jsonschema:"max results for the grep/trace/locate lanes"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
	Budget      int    `json:"budget,omitempty" jsonschema:"optional context-token budget; when set, the response reports cost.budget_left = budget − tokens_returned"`
}

// LookResult carries whichever lane ran; the other pointers stay nil. `kind`
// names the lane so the agent doesn't have to sniff which field is populated.
type LookResult struct {
	Kind   string            `json:"kind"` // read | grep | trace | locate
	Read   *SummarizeOutput  `json:"read,omitempty"`
	Grep   *SearchGrepOutput `json:"grep,omitempty"`
	Trace  *TraceOutput      `json:"trace,omitempty"`
	Locate *LocateOutput     `json:"locate,omitempty"`
}

// LookOutput is the universal envelope for look: an exact-provenance fetch plus a
// routed next step where a cheap, useful follow-up exists.
type LookOutput struct {
	Status string     `json:"status"` // ok | error | <underlying lane status>
	Hint   string     `json:"hint,omitempty"`
	Result LookResult `json:"result"`
	Trust  EnvTrust   `json:"trust"`
	Cost   *EnvCost   `json:"cost,omitempty"`
	Next   []NextStep `json:"next,omitempty"`
}

// locationSuffix matches a trailing `:line` or `:line:col`, the shape of a
// concrete code location for the locate lane.
var locationSuffix = regexp.MustCompile(`:\d+(:\d+)?$`)

// lookPathExts is the curated set of source/doc/config extensions that mark a
// bare target (no slash) as a file to read rather than a symbol to trace. It is
// deliberately an allowlist: it must NOT catch a package-tail symbol like
// `mcp.NewServer` (whose "extension" `NewServer` is not here), which is the one
// disambiguation the classifier has to get right.
var lookPathExts = map[string]bool{
	"go": true, "mod": true, "sum": true,
	"ts": true, "tsx": true, "js": true, "jsx": true, "mjs": true, "cjs": true, "vue": true, "svelte": true,
	"py": true, "rs": true, "java": true, "kt": true, "kts": true, "scala": true, "clj": true,
	"c": true, "h": true, "cc": true, "cpp": true, "hpp": true, "cs": true, "m": true, "mm": true,
	"rb": true, "php": true, "swift": true, "dart": true, "lua": true, "pl": true, "r": true,
	"ex": true, "exs": true, "erl": true, "hs": true,
	"sh": true, "bash": true, "zsh": true, "fish": true,
	"sql": true, "proto": true, "graphql": true, "gql": true,
	"md": true, "markdown": true, "rst": true, "txt": true, "adoc": true,
	"json": true, "yaml": true, "yml": true, "toml": true, "ini": true, "cfg": true, "conf": true,
	"xml": true, "html": true, "htm": true, "css": true, "scss": true, "sass": true, "less": true,
	"csv": true, "tsv": true, "lock": true, "gradle": true, "mk": true, "make": true,
	"tf": true, "tfvars": true, "bzl": true, "env": true, "gitignore": true, "dockerfile": true,
}

// classifyLookTarget is the pure routing decision: it maps a raw target string to
// one of the four exact-fetch lanes and returns the cleaned argument for that
// lane. It performs no I/O so the whole target→lane matrix is table-testable.
//
// Order (most specific first): /regex/ → path:line → path → symbol. Regex wins
// outright (explicit delimiters); a trailing :line over a path-like head is a
// location; a slash or a known file extension is a path; everything else is a
// symbol to trace.
func classifyLookTarget(raw string) (kind, cleaned string) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", ""
	}
	// /regex/ — explicit pattern delimiters win over every other reading.
	if len(t) >= 2 && strings.HasPrefix(t, "/") && strings.HasSuffix(t, "/") {
		return "grep", t[1 : len(t)-1]
	}
	// path:line[:col] — a concrete location, but only when the head before the
	// line number is itself path-like; `Foo:12` is not a location.
	if loc := locationSuffix.FindStringIndex(t); loc != nil {
		head := t[:loc[0]]
		if looksLikePath(head) {
			return "locate", t
		}
	}
	// path — a file to read.
	if looksLikePath(t) {
		return "read", t
	}
	// otherwise a symbol — trace it.
	return "trace", t
}

// looksLikePath reports whether a target denotes a file path (for the read lane)
// rather than a symbol. True when it contains a path separator, an explicit
// relative/home prefix, or a base name ending in a known source/doc/config
// extension.
func looksLikePath(t string) bool {
	if t == "" {
		return false
	}
	// A path argument is always a single token. Multi-word input is prose even
	// when it mentions a slash or a path-like fragment ("how does since:/diff:
	// resolve a ref") — without this guard the slash short-circuit below
	// misroutes prose questions to the read lane as a literal, whole-sentence
	// "file path" (#229).
	if strings.ContainsAny(t, " \t\n") {
		return false
	}
	if strings.ContainsAny(t, "/\\") {
		return true
	}
	if strings.HasPrefix(t, "~") {
		return true
	}
	base := t
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	dot := strings.LastIndex(base, ".")
	if dot < 0 || dot == len(base)-1 {
		return false
	}
	ext := strings.ToLower(base[dot+1:])
	return lookPathExts[ext]
}

// lookVerb classifies the target, dispatches to the selected exact-fetch handler,
// and folds its result into the universal envelope. Provenance is "exact" for
// every lane — look never infers.
func lookVerb(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in LookInput) (*sdk.CallToolResult, LookOutput, error) {
	target := strings.TrimSpace(in.Target)
	if target == "" {
		return nil, LookOutput{Status: "error", Hint: "target is required", Trust: exactTrust()}, nil
	}

	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	var cleaned string
	if kind == "" {
		kind, cleaned = classifyLookTarget(target)
	} else {
		// An explicit kind still strips /regex/ delimiters so the pattern is clean.
		cleaned = target
		if kind == "grep" && len(target) >= 2 && strings.HasPrefix(target, "/") && strings.HasSuffix(target, "/") {
			cleaned = target[1 : len(target)-1]
		}
	}

	switch kind {
	case "read":
		_, ro, err := h.summarize(ctx, req, SummarizeInput{Path: cleaned, Mode: in.Mode, ProjectRoot: in.ProjectRoot, BudgetTokens: in.Budget})
		if err != nil {
			return nil, LookOutput{Status: "error", Hint: err.Error(), Trust: exactTrust()}, err
		}
		out := LookOutput{
			Status: ro.Status,
			Hint:   ro.Hint,
			Result: LookResult{Kind: "read", Read: &ro},
			Trust:  exactTrust(),
		}
		// Automatic session dedup (#110 step 3): suppress the bytes if this exact
		// range was already surfaced this session — no etag round-trip required.
		if sl, ok := h.(seenLooker); ok {
			sl.applySeenLook(sessionKey(req), &out)
		}
		return nil, out, nil

	case "grep":
		_, grepOut, err := h.searchGrep(ctx, req, SearchGrepInput{
			Pattern: cleaned, Context: in.Context, Fixed: in.Fixed,
			MaxResults: in.K, ProjectRoot: in.ProjectRoot,
		})
		if err != nil {
			return nil, LookOutput{Status: "error", Hint: err.Error(), Trust: exactTrust()}, err
		}
		out := LookOutput{
			Status: grepOut.Status,
			Hint:   grepOut.Hint,
			Result: LookResult{Kind: "grep", Grep: &grepOut},
			Trust:  exactTrust(),
		}
		// Route the agent to read the first hit in place — the natural next move
		// after a grep is to look at where it matched.
		if len(grepOut.Matches) > 0 {
			m := grepOut.Matches[0]
			out.Next = append(out.Next, NextStep{
				Verb: "query",
				Args: map[string]any{"input": firstMatchRef(m)},
				Why:  "read the first match in its enclosing context",
			})
		}
		return nil, out, nil

	case "trace":
		_, to, err := traceVerb(ctx, h, req, TraceInput{
			Symbol: cleaned, Direction: in.Direction, To: in.To,
			K: in.K, ProjectRoot: in.ProjectRoot,
		})
		if err != nil {
			return nil, LookOutput{Status: "error", Hint: err.Error(), Trust: exactTrust()}, err
		}
		out := LookOutput{
			Status: to.Status,
			Hint:   to.Hint,
			Result: LookResult{Kind: "trace", Trace: &to},
			Trust:  exactTrust(),
		}
		// An index-backed empty during a destructive reindex is not authoritative
		// absence — flag it so the agent retries rather than trusts the gap (#152).
		flagRebuildIfEmpty(ctx, h, in.ProjectRoot, to.Status, &out)
		return nil, out, nil

	case "locate":
		_, lo, err := h.locate(ctx, req, LocateInput{Ref: cleaned, K: in.K, ProjectRoot: in.ProjectRoot})
		if err != nil {
			return nil, LookOutput{Status: "error", Hint: err.Error(), Trust: exactTrust()}, err
		}
		out := LookOutput{
			Status: lo.Status,
			Hint:   lo.Hint,
			Result: LookResult{Kind: "locate", Locate: &lo},
			Trust:  exactTrust(),
		}
		flagRebuildIfEmpty(ctx, h, in.ProjectRoot, lo.Status, &out)
		return nil, out, nil

	default:
		return nil, LookOutput{
			Status: "error",
			Hint:   "unknown kind '" + kind + "'; use read|grep|trace|locate or omit to auto-classify",
			Trust:  exactTrust(),
		}, nil
	}
}

// firstMatchRef renders a grep match as a look target for the follow-up read.
func firstMatchRef(m GrepMatch) string {
	if m.Line > 0 {
		return m.Path + ":" + strconv.Itoa(m.Line)
	}
	return m.Path
}
