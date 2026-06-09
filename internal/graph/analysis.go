package graph

// analysis.go implements two graph-theoretic analyses on the static call graph:
//
//   TarjanSCC    — strongly connected components, O(V+E). Run on `calls` edges
//                  to surface recursive call cycles (mutual recursion, etc.).
//                  SCCs of size ≥ 2 are "cycles"; size 1 = trivially acyclic.
//
//   Betweenness  — normalized betweenness centrality, O(VE) Brandes algorithm.
//                  A node's betweenness is the fraction of shortest call-paths
//                  that pass through it. High betweenness = bridge node whose
//                  removal most disconnects the call graph.

// SCCResult is one strongly connected component. Size == 1 means the node is
// trivially acyclic (no self-call and not in a mutual-recursion cluster).
type SCCResult struct {
	// IDs is the set of node IDs in this component, in discovery order.
	IDs []string
}

// TarjanSCC computes strongly connected components of the subgraph induced by
// edgeKinds (defaults to EdgeCalls when nil). Returns all components sorted by
// descending size, smallest-last. Pure over the supplied slices — no I/O.
//
// Implementation is the iterative variant of Tarjan's algorithm to avoid
// goroutine stack overflow on large graphs.
func TarjanSCC(nodes []Node, edges []Edge, edgeKinds []EdgeKind) []SCCResult {
	if len(edgeKinds) == 0 {
		edgeKinds = []EdgeKind{EdgeCalls}
	}
	kindSet := make(map[EdgeKind]struct{}, len(edgeKinds))
	for _, k := range edgeKinds {
		kindSet[k] = struct{}{}
	}

	// Build adjacency list (outgoing only).
	adj := make(map[string][]string, len(nodes))
	for _, n := range nodes {
		adj[n.ID] = nil // ensure every node appears even with no edges
	}
	for _, e := range edges {
		if _, ok := kindSet[e.Kind]; !ok {
			continue
		}
		if _, ok := adj[e.SrcID]; !ok {
			continue // src not in our node set
		}
		if _, ok := adj[e.DstID]; !ok {
			continue // dst not in our node set
		}
		adj[e.SrcID] = append(adj[e.SrcID], e.DstID)
	}

	// Iterative Tarjan. We simulate the recursive call stack explicitly.
	const unvisited = -1
	index := make(map[string]int, len(nodes))
	lowlink := make(map[string]int, len(nodes))
	onStack := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		index[n.ID] = unvisited
	}

	counter := 0
	var stack []string
	var sccs []SCCResult

	// Frame represents one level of the simulated DFS recursion.
	type frame struct {
		node     string
		adjIndex int // next neighbour to visit
	}

	for _, n := range nodes {
		if index[n.ID] != unvisited {
			continue
		}
		// DFS starting from n.ID.
		callStack := []frame{{node: n.ID, adjIndex: 0}}
		index[n.ID] = counter
		lowlink[n.ID] = counter
		counter++
		stack = append(stack, n.ID)
		onStack[n.ID] = true

		for len(callStack) > 0 {
			top := &callStack[len(callStack)-1]
			neighbours := adj[top.node]

			if top.adjIndex < len(neighbours) {
				w := neighbours[top.adjIndex]
				top.adjIndex++
				if index[w] == unvisited {
					// Tree edge — recurse.
					index[w] = counter
					lowlink[w] = counter
					counter++
					stack = append(stack, w)
					onStack[w] = true
					callStack = append(callStack, frame{node: w, adjIndex: 0})
				} else if onStack[w] {
					// Back/cross edge to something still on the path.
					if lowlink[w] < lowlink[top.node] {
						lowlink[top.node] = lowlink[w]
					}
				}
			} else {
				// All neighbours processed — pop.
				callStack = callStack[:len(callStack)-1]
				if len(callStack) > 0 {
					parent := &callStack[len(callStack)-1]
					if lowlink[top.node] < lowlink[parent.node] {
						lowlink[parent.node] = lowlink[top.node]
					}
				}
				// If this is a root of an SCC, pop the component off the stack.
				if lowlink[top.node] == index[top.node] {
					var comp []string
					for {
						w := stack[len(stack)-1]
						stack = stack[:len(stack)-1]
						onStack[w] = false
						comp = append(comp, w)
						if w == top.node {
							break
						}
					}
					sccs = append(sccs, SCCResult{IDs: comp})
				}
			}
		}
	}

	// Sort by descending size so callers see the largest cycles first.
	sortSCCsBySize(sccs)
	return sccs
}

func sortSCCsBySize(sccs []SCCResult) {
	// Simple insertion sort — SCC count is usually small relative to nodes.
	for i := 1; i < len(sccs); i++ {
		for j := i; j > 0 && len(sccs[j].IDs) > len(sccs[j-1].IDs); j-- {
			sccs[j], sccs[j-1] = sccs[j-1], sccs[j]
		}
	}
}

// BrandesBetweenness computes normalized betweenness centrality for every node
// in the calls subgraph using the Brandes algorithm (O(VE)). Returns a map
// from node ID to betweenness in [0, 1].
//
// Only EdgeCalls edges are used. Nodes that the calls graph doesn't touch
// (types, fields, packages) return 0.
//
// Normalization: divide raw betweenness by (n-1)*(n-2) for directed graphs
// (the number of ordered pairs of distinct source/destination nodes other than
// the node itself). When n ≤ 2, all values are 0.
func BrandesBetweenness(nodes []Node, edges []Edge) map[string]float64 {
	if len(nodes) == 0 {
		return nil
	}

	// Build adjacency and node index for the calls subgraph.
	nodeIdx := make(map[string]int, len(nodes))
	for i, n := range nodes {
		nodeIdx[n.ID] = i
	}
	adj := make([][]int, len(nodes))
	for _, e := range edges {
		if e.Kind != EdgeCalls {
			continue
		}
		si, sok := nodeIdx[e.SrcID]
		di, dok := nodeIdx[e.DstID]
		if !sok || !dok || si == di {
			continue
		}
		adj[si] = append(adj[si], di)
	}

	n := len(nodes)
	cb := make([]float64, n) // raw betweenness accumulator

	// Brandes: BFS from each source.
	sigma := make([]float64, n)  // number of shortest paths from s to node
	dist := make([]int, n)       // distance from s (-1 = unvisited)
	pred := make([][]int, n)     // predecessors on shortest paths
	delta := make([]float64, n)  // pair-dependency accumulator
	stack := make([]int, 0, n)   // nodes in non-increasing order of distance
	queue := make([]int, 0, n)   // BFS queue

	for s := 0; s < n; s++ {
		// Reset per-source data.
		for i := range sigma {
			sigma[i] = 0
			dist[i] = -1
			pred[i] = pred[i][:0]
			delta[i] = 0
		}
		stack = stack[:0]
		queue = queue[:0]

		sigma[s] = 1
		dist[s] = 0
		queue = append(queue, s)

		for len(queue) > 0 {
			v := queue[0]
			queue = queue[1:]
			stack = append(stack, v)
			for _, w := range adj[v] {
				// First visit to w?
				if dist[w] < 0 {
					dist[w] = dist[v] + 1
					queue = append(queue, w)
				}
				// Is this a shortest path?
				if dist[w] == dist[v]+1 {
					sigma[w] += sigma[v]
					pred[w] = append(pred[w], v)
				}
			}
		}

		// Accumulate pair-dependencies in reverse BFS order.
		for len(stack) > 0 {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, v := range pred[w] {
				if sigma[w] > 0 {
					delta[v] += (sigma[v] / sigma[w]) * (1 + delta[w])
				}
			}
			if w != s {
				cb[w] += delta[w]
			}
		}
	}

	// Normalize. For a directed graph the normalizer is (n-1)*(n-2).
	norm := float64((n - 1) * (n - 2))
	out := make(map[string]float64, n)
	for i, nd := range nodes {
		if norm > 0 && cb[i] > 0 {
			out[nd.ID] = cb[i] / norm
		}
	}
	return out
}
