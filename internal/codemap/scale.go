package codemap

import (
	"fmt"
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// DefaultScaleBudget caps the orientation "scale" section (#581). It is a single
// line, so the cap only guards against a pathological render.
const DefaultScaleBudget = 60

const scaleHeader = "## scale\n"

// Scale is the renderer's view of a project's size — files, packages, declared
// symbols, and call edges. Mirrors store.GraphScale (codemap stays free of a
// store import; the mcp layer maps one to the other, as it does for ImportEdge).
type Scale struct {
	Files     int
	Packages  int
	Symbols   int
	CallEdges int
}

// Empty reports whether the scale carries no signal, so RenderScale omits the
// section and the bundle stays byte-identical to a pre-scale render.
func (s Scale) Empty() bool {
	return s.Files == 0 && s.Packages == 0 && s.Symbols == 0 && s.CallEdges == 0
}

// RenderScale renders the orientation "scale" section: a one-glance size line so
// a reader knows immediately whether they're in a 300-file service or a 3000-file
// monorepo. Pure counts, zero inference — deterministic and byte-stable. Returns
// "" for an empty/unindexed graph, omitting the section. The budget guards a
// pathological render; the line is normally well under it.
func RenderScale(s Scale, budget int) string {
	if s.Empty() {
		return ""
	}
	if budget <= 0 {
		budget = DefaultScaleBudget
	}
	// Most-informative first: scale reads files → packages → symbols → edges.
	// Parts are added greedily while the line fits budget, so a tight budget
	// drops the trailing (least-orienting) counts rather than the whole section.
	candidates := []struct {
		n    int
		noun string
	}{
		{s.Files, "file"},
		{s.Packages, "package"},
		{s.Symbols, "symbol"},
		{s.CallEdges, "call edge"},
	}
	var parts []string
	for _, c := range candidates {
		if c.n <= 0 {
			continue
		}
		next := append(parts, plural(c.n, c.noun))
		if len(parts) > 0 && tokens.Count(scaleHeader+strings.Join(next, " · ")+"\n") > budget {
			break
		}
		parts = next
	}
	if len(parts) == 0 {
		return ""
	}
	return scaleHeader + strings.Join(parts, " · ") + "\n"
}

// plural formats a count with its noun, adding a trailing "s" for n != 1
// ("1 file", "2 files", "3 call edges").
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
