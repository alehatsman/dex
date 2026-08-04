package retrieve

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/store"
)

// Symbol/ref/frame resolution — the domain core of the `locate` verb (#111).
// Turning "what location does the caller mean" into a concrete (path, symbol,
// kind, line-range) is pure retrieval logic over the index, so it lives here
// rather than in the mcp transport. The transport keeps the composition around
// it: callers via the trace verb, sibling tests / nearest doc / blame via the
// Enricher, related notes via recall.

// LocateRequest selects the target three ways. Exactly one of Ref / Symbol /
// Frame is expected; when more than one is set the precedence is Ref, then
// Symbol, then Frame.
type LocateRequest struct {
	Ref    string
	Symbol string
	Frame  string
}

// LocateTarget is the resolved (path, symbol, kind, line-range) plus a status
// the caller maps straight onto its wire output.
type LocateTarget struct {
	Path      string
	Symbol    string
	Kind      string
	StartLine int
	EndLine   int
	Status    string // "ok" | "not-found"
	Hint      string
}

// ResolveLocateTarget turns the request into a concrete target. Ref wins, then
// Symbol, then Frame (which is itself parsed into a ref or a symbol).
func ResolveLocateTarget(ctx context.Context, st *store.Store, root string, in LocateRequest) LocateTarget {
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
	return LocateTarget{Status: "not-found", Hint: "could not parse a file:line or symbol out of the frame"}
}

// resolveByRef resolves a 'path:line' to its enclosing chunk via ChunkAt.
func resolveByRef(ctx context.Context, st *store.Store, root, ref string) LocateTarget {
	path, line, ok := parseRef(ref)
	if !ok {
		return LocateTarget{Status: "not-found", Hint: fmt.Sprintf("ref %q is not 'path:line'", ref)}
	}
	path = normalizeRefPath(root, path)
	hit, err := st.ChunkAt(ctx, path, line)
	if err != nil {
		return LocateTarget{Status: "not-found",
			Hint: fmt.Sprintf("%v — check the path is project-relative and indexed.", err)}
	}
	sym := hit.Name
	// Prefer the graph node's QualifiedName (e.g. "(*Store).Run") over the
	// bare chunk name ("Run") so traceVerb doesn't match every node named Run
	// across all packages (#716).
	if qn, qerr := st.GraphQualifiedNameAt(ctx, path, line); qerr == nil && qn != "" {
		sym = qn
	}
	return LocateTarget{
		Path: hit.Path, Symbol: sym, Kind: hit.Kind,
		StartLine: hit.StartLine, EndLine: hit.EndLine, Status: "ok",
	}
}

// resolveBySymbol resolves a symbol name to its first indexed definition.
func resolveBySymbol(ctx context.Context, st *store.Store, sym string) LocateTarget {
	hits, err := st.FindSymbol(ctx, sym, 1)
	if err != nil {
		return LocateTarget{Status: "not-found", Hint: fmt.Sprintf("lookup %q: %v", sym, err)}
	}
	// Frames and qualified names ('main.Greet', 'mcp.(*Server).Run') don't match
	// the exact-name index; fall back to the bare trailing identifier.
	if len(hits) == 0 {
		if bare := BareSymbolName(sym); bare != sym {
			// For receiver-qualified forms like (*T).Method, fetch more candidates
			// so we can prefer method/function over field — a field named Context
			// loses to a method named Context when the input is (*Server).Context.
			k := 1
			if reReceiverPointer.MatchString(sym) {
				k = 20
			}
			hits, err = st.FindSymbol(ctx, bare, k)
			if err != nil {
				return LocateTarget{Status: "not-found", Hint: fmt.Sprintf("lookup %q: %v", bare, err)}
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
		return LocateTarget{Status: "not-found",
			Hint: fmt.Sprintf("no indexed symbol named %q — check spelling or reindex.", sym)}
	}
	h := hits[0]
	name := h.Name
	if name == "" {
		name = sym
	}
	return LocateTarget{
		Path: h.Path, Symbol: name, Kind: h.Kind,
		StartLine: h.StartLine, EndLine: h.EndLine, Status: "ok",
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

// normalizeRefPath makes a ref path project-relative and slash-cleaned so it
// matches the index's stored paths. Used by resolveByRef.
func normalizeRefPath(root, path string) string {
	if filepath.IsAbs(path) {
		if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
			path = rel
		}
	}
	return filepath.ToSlash(filepath.Clean(path))
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
