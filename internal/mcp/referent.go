package mcp

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/store"
)

// A recalled fact that names a code referent — a path, a path:line, or a
// symbol — that no longer resolves against the current index is worse than no
// fact: the agent acts on it with recall-time confidence (#167). referent
// extraction + liveness checking downgrade such a fact to needs_verification at
// recall — never deleting it, never touching its confidence.

// referentKind classifies what an extracted referent points at.
type referentKind string

const (
	kindPath     referentKind = "path"     // a bare file path
	kindPathLine referentKind = "pathline" // a file path with a line number
	kindSymbol   referentKind = "symbol"   // a code symbol (func/type/method)
)

// referent is one code anchor extracted from a fact body.
type referent struct {
	Kind referentKind
	Raw  string // exactly as written, for the verification note
	Path string // kindPath / kindPathLine
	Line int    // kindPathLine (0 = none)
	Name string // kindSymbol
}

// pathLineRe matches `dir/file.go:42` — a path token with a code-ish extension
// followed by a line number. Anchored on a `/`-bearing or extension-bearing
// token so a bare `note:12` prose fragment does not match.
var pathLineRe = regexp.MustCompile(`\b([\w./-]+\.[A-Za-z][A-Za-z0-9]{0,4}):(\d+)\b`)

// pathRe matches a bare file path: a `/`-separated token ending in a code-ish
// extension (e.g. `internal/mcp/server.go`). Single-segment names without a
// slash are excluded — too easily a prose word with a suffix.
var pathRe = regexp.MustCompile(`\b([\w.-]+/[\w./-]*[\w-]\.[A-Za-z][A-Za-z0-9]{0,4})\b`)

// methodRe matches a Go method reference `(*Recv).Method` or `(Recv).Method`.
var methodRe = regexp.MustCompile(`\(\*?([A-Z][A-Za-z0-9_]*)\)\.([A-Z][A-Za-z0-9_]*)`)

// dottedRe matches a qualified symbol `pkg.Exported` — lowercase package, then
// an exported identifier. Distinctive enough to extract unquoted.
var dottedRe = regexp.MustCompile(`\b([a-z][a-z0-9_]*)\.([A-Z][A-Za-z0-9_]*)\b`)

// backtickIdentRe matches a backtick-quoted CamelCase identifier — the only
// bare-identifier form trusted as a symbol, since the author marked it as code.
var backtickIdentRe = regexp.MustCompile("`([A-Z][A-Za-z0-9_]*)`")

// extractReferents pulls the code anchors out of a fact body. It favours
// precision over recall: a missed referent just means no liveness signal, while
// a false one would wrongly flag a good fact. path:line is matched first and its
// span suppressed so the bare-path matcher does not double-count the same path.
func extractReferents(body string) []referent {
	var out []referent
	seen := map[string]bool{}
	add := func(r referent) {
		key := string(r.Kind) + "\x00" + r.Raw
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, r)
	}

	// path:line first; blank out matched spans so pathRe skips them.
	masked := []byte(body)
	for _, m := range pathLineRe.FindAllStringSubmatchIndex(body, -1) {
		full := body[m[0]:m[1]]
		p := body[m[2]:m[3]]
		line, _ := strconv.Atoi(body[m[4]:m[5]])
		add(referent{Kind: kindPathLine, Raw: full, Path: p, Line: line})
		for i := m[0]; i < m[1]; i++ {
			masked[i] = ' '
		}
	}
	for _, m := range pathRe.FindAllStringSubmatch(string(masked), -1) {
		add(referent{Kind: kindPath, Raw: m[1], Path: m[1]})
	}
	for _, m := range methodRe.FindAllStringSubmatch(body, -1) {
		add(referent{Kind: kindSymbol, Raw: m[0], Name: m[2]})
	}
	for _, m := range dottedRe.FindAllStringSubmatch(body, -1) {
		add(referent{Kind: kindSymbol, Raw: m[0], Name: m[2]})
	}
	for _, m := range backtickIdentRe.FindAllStringSubmatch(body, -1) {
		add(referent{Kind: kindSymbol, Raw: m[0], Name: m[1]})
	}
	return out
}

// indexedExts returns the set of file extensions dex actually indexed, derived
// from the code-path set. A referent whose extension is absent from this set is
// left unjudged (dex has no authority over it — e.g. go.mod, tasks.yml), which
// keeps liveness precise: only paths in a known-indexed language can be "dead".
func indexedExts(paths map[string]int) map[string]bool {
	exts := map[string]bool{}
	for p := range paths {
		if e := strings.ToLower(path.Ext(p)); e != "" {
			exts[e] = true
		}
	}
	return exts
}

// annotateLiveness flags any recalled fact whose named referents have all gone
// dead against the current index. A fact is flagged iff, for some referent kind
// it carries, EVERY referent of that kind fails to resolve — one live anchor
// means the fact is still grounded. Facts with no extractable referent are never
// touched. It never mutates the store; the flags are computed, presentation-only.
func (s *Server) annotateLiveness(ctx context.Context, st *store.Store, facts []KnowledgeFactOutput) {
	if len(facts) == 0 {
		return
	}
	paths, err := st.CodeFilePaths(ctx)
	if err != nil || len(paths) == 0 {
		// No path set to check against (empty/broken index) — stay silent
		// rather than flag everything (#167 non-goal: no false downgrades).
		return
	}
	exts := indexedExts(paths)

	symbolLive := func(name string) bool {
		hits, err := st.FindSymbol(ctx, name, 1)
		return err != nil || len(hits) > 0 // err → don't flag (can't judge)
	}
	for i := range facts {
		refs := extractReferents(facts[i].Body)
		if len(refs) == 0 {
			continue
		}
		note := deadReferentNote(refs, paths, exts, symbolLive)
		if note != "" {
			facts[i].NeedsVerification = true
			facts[i].VerificationNote = note
		}
	}
}

// deadReferentNote decides whether a fact's referents are collectively stale and
// returns the human note. It groups referents by kind; a kind counts as dead
// only if it had at least one judgeable referent and none of them resolved. The
// fact is flagged when every judgeable kind is dead, and the note names the
// specific dead anchors so the agent knows what to re-verify. symbolLive reports
// whether a symbol name still resolves against the index (injected so the pure
// path/line logic stays store-free and unit-testable).
func deadReferentNote(refs []referent, paths map[string]int, exts map[string]bool, symbolLive func(name string) bool) string {
	type kindState struct {
		judged int
		dead   []string
	}
	states := map[referentKind]*kindState{}
	get := func(k referentKind) *kindState {
		if states[k] == nil {
			states[k] = &kindState{}
		}
		return states[k]
	}

	for _, r := range refs {
		switch r.Kind {
		case kindPath, kindPathLine:
			if !exts[strings.ToLower(path.Ext(r.Path))] {
				continue // extension dex does not index — no authority to judge
			}
			ps := get(r.Kind)
			ps.judged++
			maxLine, ok := paths[r.Path]
			switch {
			case !ok:
				ps.dead = append(ps.dead, fmt.Sprintf("%s (file no longer indexed)", r.Raw))
			case r.Kind == kindPathLine && r.Line > maxLine:
				ps.dead = append(ps.dead, fmt.Sprintf("%s (file now ends at line %d)", r.Raw, maxLine))
			}
		case kindSymbol:
			ks := get(kindSymbol)
			ks.judged++
			if !symbolLive(r.Name) {
				ks.dead = append(ks.dead, fmt.Sprintf("%s (symbol not found)", r.Raw))
			}
		}
	}

	if len(states) == 0 {
		return "" // nothing judgeable
	}
	// Flag only when every judged kind is fully dead (all-fail, not any-fail).
	var deadParts []string
	for _, ks := range states {
		if ks.judged == 0 {
			continue
		}
		if len(ks.dead) < ks.judged {
			return "" // this kind still has a live anchor — fact stays grounded
		}
		deadParts = append(deadParts, ks.dead...)
	}
	if len(deadParts) == 0 {
		return ""
	}
	return "names " + strings.Join(deadParts, ", ") + " — verify against current HEAD before relying on this fact"
}
