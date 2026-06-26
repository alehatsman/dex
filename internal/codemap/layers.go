package codemap

import (
	"fmt"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// DefaultLayersBudget caps the orientation "layers" section (#581).
const DefaultLayersBudget = 220

// maxPkgsPerLayer caps how many package names a layer line shows before it
// summarises the rest as "(+N more)".
const maxPkgsPerLayer = 8

// ImportEdge is one internal package→package dependency: From imports To. Both
// are project package paths.
type ImportEdge struct {
	From string
	To   string
}

// layerize assigns each package a dependency layer: a package with no internal
// dependencies is layer 0 (foundational); otherwise its layer is 1 + the max
// layer of the internal packages it imports. Returns layers[i] = the package
// paths at layer i (sorted), plus any packages caught in an import cycle. Go
// forbids import cycles, so `cyclic` is only ever non-empty for tree-sitter
// languages; the on-stack guard makes the longest-path walk terminate anyway.
func layerize(edges []ImportEdge) (layers [][]string, cyclic []string) {
	deps := map[string]map[string]bool{}
	nodes := map[string]bool{}
	for _, e := range edges {
		nodes[e.From] = true
		nodes[e.To] = true
		if deps[e.From] == nil {
			deps[e.From] = map[string]bool{}
		}
		deps[e.From][e.To] = true
	}

	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := map[string]int{}
	layerOf := map[string]int{}
	cyclicSet := map[string]bool{}

	var visit func(string) int
	visit = func(p string) int {
		switch state[p] {
		case done:
			return layerOf[p]
		case onStack: // back-edge → cycle
			cyclicSet[p] = true
			return 0
		}
		state[p] = onStack
		max := -1
		// Deterministic dep order so cycle attribution is stable.
		ds := make([]string, 0, len(deps[p]))
		for d := range deps[p] {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		for _, d := range ds {
			if dl := visit(d); dl > max {
				max = dl
			}
		}
		layerOf[p] = max + 1
		state[p] = done
		return layerOf[p]
	}

	sorted := make([]string, 0, len(nodes))
	for n := range nodes {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		visit(n)
	}

	maxLayer := 0
	for n, l := range layerOf {
		if cyclicSet[n] {
			continue
		}
		if l > maxLayer {
			maxLayer = l
		}
	}
	layers = make([][]string, maxLayer+1)
	for _, n := range sorted {
		if cyclicSet[n] {
			cyclic = append(cyclic, n)
			continue
		}
		layers[layerOf[n]] = append(layers[layerOf[n]], n)
	}
	return layers, cyclic
}

const layersHeader = "## layers (foundational → top)\n"

// RenderLayers renders the dependency-layer section (#581): one line per layer,
// foundational first, package short-names greedily fit to budget. Returns ""
// when there are no internal import edges (the section is then omitted).
func RenderLayers(edges []ImportEdge, budget int) string {
	if len(edges) == 0 {
		return ""
	}
	if budget <= 0 {
		budget = DefaultLayersBudget
	}
	layers, cyclic := layerize(edges)
	// Collision-aware names computed across ALL packages, so two distinct paths
	// that share a tail (internal/compress vs internal/bench/compress) get
	// disambiguated rather than printed as the same name in different layers.
	all := append([]string{}, cyclic...)
	for _, l := range layers {
		all = append(all, l...)
	}
	names := disambiguateNames(all)

	var b strings.Builder
	b.WriteString(layersHeader)
	for i, pkgs := range layers {
		if len(pkgs) == 0 {
			continue
		}
		line := layerLine(i, pkgs, names)
		if b.Len() > len(layersHeader) && tokens.Count(b.String()+line) > budget {
			break
		}
		b.WriteString(line)
	}
	if len(cyclic) > 0 {
		b.WriteString("- cycle: " + strings.Join(displayNames(cyclic, names, maxPkgsPerLayer), ", ") + "\n")
	}
	if b.Len() == len(layersHeader) {
		return "" // nothing fit / no layers
	}
	return b.String()
}

// layerLine renders one layer row with capped, disambiguated package names.
func layerLine(i int, pkgs []string, names map[string]string) string {
	shown := displayNames(pkgs, names, maxPkgsPerLayer)
	extra := len(pkgs) - len(shown)
	line := fmt.Sprintf("- L%d: %s", i, strings.Join(shown, ", "))
	if extra > 0 {
		line += fmt.Sprintf(" (+%d more)", extra)
	}
	return line + "\n"
}

// displayNames maps package paths through the disambiguated-name table, then
// dedupes + sorts + caps the result.
func displayNames(paths []string, names map[string]string, max int) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		n := names[p]
		if n == "" {
			n = shortPkg(p)
		}
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// disambiguateNames assigns each package path a display name: its last path
// segment, widened to the last two segments when the one-segment name collides
// with another package.
func disambiguateNames(paths []string) map[string]string {
	tail1 := map[string]int{}
	for _, p := range paths {
		tail1[lastSegments(p, 1)]++
	}
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		if tail1[lastSegments(p, 1)] > 1 {
			out[p] = lastSegments(p, 2)
		} else {
			out[p] = lastSegments(p, 1)
		}
	}
	return out
}

// lastSegments returns the last n slash-separated segments of a path.
func lastSegments(p string, n int) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if n >= len(segs) {
		return strings.Join(segs, "/")
	}
	return strings.Join(segs[len(segs)-n:], "/")
}
