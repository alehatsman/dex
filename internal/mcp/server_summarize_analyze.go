package mcp

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/summarize"
	"github.com/alehatsman/dex/internal/tokens"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ReadAnalysis is the `read mode=analyze` payload (#623): a per-mode token-cost
// comparison for a file plus a recommended mode, returned WITHOUT the file
// content so an agent can pick the cheapest sufficient view before paying to
// read it. It is the measured counterpart to estimateModeTokens' fixed-fraction
// heuristics — every count is a real render of the file through that mode.
type ReadAnalysis struct {
	Path  string `json:"path"`
	Lines int    `json:"lines"`
	Bytes int    `json:"bytes"`
	// Handle is a #344 expansion handle for the whole file (#620): echo it back
	// as read(handle=…, mode=…) to lazily expand only the files you need after
	// analyzing many. An opaque, anti-hallucination token — never a path to type.
	Handle          string     `json:"handle,omitempty"`
	Indexed         bool       `json:"indexed"` // structural modes available?
	MeanBitsPerChar float64    `json:"mean_bits_per_char"`
	Compressibility string     `json:"compressibility"` // low | medium | high
	Modes           []ModeCost `json:"modes"`
	Recommendation  string     `json:"recommendation"`
	Reason          string     `json:"reason,omitempty"`
}

// ModeCost is one mode's measured token cost within a ReadAnalysis.
type ModeCost struct {
	Mode     string `json:"mode"`
	Tokens   int    `json:"tokens"`
	SavedPct int    `json:"saved_pct"`      // vs full; 0 for full or when larger
	Lossy    bool   `json:"lossy"`          // drops or transforms source content
	Note     string `json:"note,omitempty"` // e.g. why a mode was unavailable
}

// summarizeModeAnalyze renders the file through each mode, counts the actual
// tokens, and returns the comparison + a recommendation — no file content. It
// never errors on a missing index: it degrades to the index-free modes (full,
// aggressive) and flags the structural ones as unavailable.
func (s *Server) summarizeModeAnalyze(w summarizeWork) (*sdk.CallToolResult, SummarizeOutput, error) {
	content := string(w.data)
	fullTok := tokens.Count(content)

	an := ReadAnalysis{
		Path:  w.relTarget,
		Lines: strings.Count(strings.TrimRight(content, "\n"), "\n") + 1,
		Bytes: len(w.data),
	}
	an.MeanBitsPerChar = meanBitsPerChar(content)

	add := func(mode string, text string, lossy bool, note string) {
		t := tokens.Count(text)
		saved := 0
		if fullTok > 0 && t < fullTok {
			saved = (fullTok - t) * 100 / fullTok
		}
		an.Modes = append(an.Modes, ModeCost{Mode: mode, Tokens: t, SavedPct: saved, Lossy: lossy, Note: note})
	}

	// full — the raw file, the baseline every saving is measured against.
	add(string(ReadModeFull), content, false, "")

	// aggressive — lossy text compression; index-free, always available.
	ext := filepath.Ext(w.realTarget)
	strict := profiles.Active(w.p.Root).StrictAnchors()
	add(string(ReadModeAggressive), compress.CompressCode(content, ext, strict), true, "")

	// Structural modes need the symbol index. Absence is a degraded result, not
	// an error — the agent still learns full vs aggressive.
	if st, err := s.openStore(w.p.DBPath); err == nil {
		if syms, err := st.SymbolsByFile(w.ctx, w.relTarget); err == nil && len(syms) > 0 {
			an.Indexed = true
			add(string(ReadModeSignatures), summarize.SignaturesView(w.ctx, st, w.data, syms, w.relTarget), false, "")
			add(string(ReadModeSkeleton), compress.SkeletonPass(w.data, w.relTarget, bodyScopesForSymbols(syms)).Text, false, "")
			imports, _ := st.ImportsForFile(w.ctx, w.relTarget)
			add(string(ReadModeMap), summarize.MapView(w.ctx, st, w.data, syms, imports, w.relTarget), false, "")
		}
	}

	an.Compressibility = compressibilityLabel(an)
	an.Recommendation, an.Reason = recommendReadMode(an, fullTok)
	// Whole-file expansion handle (#620): lets the agent analyze many files and
	// then lazily expand only the ones it needs via read(handle=…, mode=…),
	// echoing an opaque token instead of reconstructing the path.
	endLine := an.Lines
	if endLine < 1 {
		endLine = 1
	}
	an.Handle = EncodeHandle(w.relTarget, 1, endLine)

	out := w.out
	out.Status = "ok"
	out.Etag = w.etag
	out.Path = w.relTarget
	out.Bytes = len(w.data)
	out.Analysis = &an
	return nil, out, nil
}

// bodyScopesForSymbols projects indexed symbols into the BodyScope set the
// skeleton pass consumes. Shared by mode=skeleton and mode=analyze.
func bodyScopesForSymbols(syms []store.GraphSymbol) []compress.BodyScope {
	scopes := make([]compress.BodyScope, 0, len(syms))
	for _, sym := range syms {
		scopes = append(scopes, compress.BodyScope{
			Name:      sym.QualifiedName,
			Kind:      sym.Kind,
			Exported:  isSymExported(sym.Name, sym.FilePath),
			StartLine: sym.StartLine,
			EndLine:   sym.EndLine,
		})
	}
	return scopes
}

// isSymExported reports whether a symbol named name from filePath should be
// treated as exported. Go uses uppercase-first; every other language dex
// indexes uses either lowercase public names (Python, TypeScript, Rust) or
// explicit modifiers (pub, export) that are not yet tracked in the index.
// Treat all non-Go symbols as exported so the skeleton doesn't silently
// discard the entire public API of non-Go files.
func isSymExported(name, filePath string) bool {
	if len(name) == 0 {
		return false
	}
	if strings.HasSuffix(filePath, ".go") {
		return name[0] >= 'A' && name[0] <= 'Z'
	}
	return true
}

// meanBitsPerChar is the Shannon entropy of content over its rune distribution
// — a rough density signal (English prose ~4, dense/minified code ~5+). Empty
// input is 0.
func meanBitsPerChar(s string) float64 {
	if s == "" {
		return 0
	}
	freq := map[rune]int{}
	var n int
	for _, r := range s {
		freq[r]++
		n++
	}
	if n == 0 {
		return 0
	}
	var h float64
	for _, c := range freq {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return math.Round(h*100) / 100
}

// compressibilityLabel grades how much the file can shrink, from the best
// measured saving across all modes (lossy included). It answers "is it worth
// compressing this file at all?" more directly than raw entropy.
func compressibilityLabel(an ReadAnalysis) string {
	best := 0
	for _, m := range an.Modes {
		if m.SavedPct > best {
			best = m.SavedPct
		}
	}
	switch {
	case best >= 70:
		return "high"
	case best >= 40:
		return "medium"
	default:
		return "low"
	}
}

// smallFileTokenFloor is the size under which compression isn't worth it — just
// read the whole file. Aligned with minCompressLines' spirit: tiny inputs gain
// nothing and lose context from a compressed view.
const smallFileTokenFloor = 400

// recommendReadMode picks the cheapest mode that still conveys the file
// usefully: full for small files, signatures for indexed code (best API/token
// tradeoff), aggressive for large unindexed files, full otherwise.
func recommendReadMode(an ReadAnalysis, fullTok int) (mode, reason string) {
	if fullTok <= smallFileTokenFloor {
		return string(ReadModeFull), fmt.Sprintf("small file (%d tokens) — read it whole", fullTok)
	}
	if sig, ok := modeCost(an, string(ReadModeSignatures)); ok {
		return string(ReadModeSignatures), fmt.Sprintf("indexed code — signatures keeps the API surface at %d%% fewer tokens", sig.SavedPct)
	}
	if agg, ok := modeCost(an, string(ReadModeAggressive)); ok && agg.SavedPct >= 30 {
		return string(ReadModeAggressive), fmt.Sprintf("no indexed symbols — aggressive compress saves %d%% (lossy); run `dex index` for lossless signatures", agg.SavedPct)
	}
	return string(ReadModeFull), "no structural saving available — read full"
}

// modeCost returns the ModeCost for a named mode, if present.
func modeCost(an ReadAnalysis, mode string) (ModeCost, bool) {
	for _, m := range an.Modes {
		if m.Mode == mode {
			return m, true
		}
	}
	return ModeCost{}, false
}
