package mcp

import "github.com/alehatsman/dex/internal/graphquery"

// The graphquery engine returns transport-neutral result types whose fields
// mirror these wire types exactly (modulo json tags), so each maps with a
// direct struct conversion. These adapters keep the JSON contract (and its
// tags) in the transport layer while the graph algorithms live in graphquery.

func impactNodesFrom(rs []graphquery.Reachable) []ImpactNode {
	out := make([]ImpactNode, len(rs))
	for i, r := range rs {
		out[i] = ImpactNode(r)
	}
	return out
}

func pathHopsFrom(hs []graphquery.Hop) []PathHop {
	out := make([]PathHop, len(hs))
	for i, h := range hs {
		out[i] = PathHop(h)
	}
	return out
}

func docLinksFrom(es []graphquery.DocEdge) []DocLink {
	out := make([]DocLink, len(es))
	for i, e := range es {
		out[i] = DocLink(e)
	}
	return out
}

func packageNodesFrom(ss []graphquery.PackageStat) []PackageNode {
	out := make([]PackageNode, len(ss))
	for i, s := range ss {
		out[i] = PackageNode(s)
	}
	return out
}

func packageEdgesFrom(is []graphquery.PackageImport) []PackageEdge {
	out := make([]PackageEdge, len(is))
	for i, im := range is {
		out[i] = PackageEdge(im)
	}
	return out
}
