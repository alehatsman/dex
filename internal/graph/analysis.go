package graph

import "sort"

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
	sigma := make([]float64, n) // number of shortest paths from s to node
	dist := make([]int, n)      // distance from s (-1 = unvisited)
	pred := make([][]int, n)    // predecessors on shortest paths
	delta := make([]float64, n) // pair-dependency accumulator
	stack := make([]int, 0, n)  // nodes in non-increasing order of distance
	queue := make([]int, 0, n)  // BFS queue

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

// ─── Louvain community detection ──────────────────────────────────────────

// CommunityResult maps each node ID to a stable integer community ID (≥0).
// IDs are assigned by sorting the node IDs within each community and issuing
// monotonically increasing labels — so that re-runs on the same graph
// produce the same labelling even though Louvain's internal order can vary.
type CommunityResult struct {
	// Communities maps node ID → community label.
	Communities map[string]int
	// Members maps community label → sorted node IDs.
	Members map[int][]string
}

// LouvainCommunities runs the Louvain algorithm on the undirected projection
// of the supplied edges (both calls and imports are treated as undirected
// edges). It is deterministic for a fixed input by processing nodes in sorted
// order throughout.
//
// The algorithm:
//  1. Phase 1 — iterate nodes in stable order; move each node to the
//     neighbouring community that gives the greatest modularity gain; repeat
//     until no improvement.
//  2. Phase 2 — aggregate communities into super-nodes and repeat from phase 1.
//  3. Terminate when a full pass yields no improvement.
//
// This is O((V+E)·log V) per pass and O(V+E) space.
func LouvainCommunities(nodes []Node, edges []Edge) CommunityResult {
	if len(nodes) == 0 {
		return CommunityResult{
			Communities: map[string]int{},
			Members:     map[int][]string{},
		}
	}

	// Collect unique IDs, sorted for determinism.
	idSet := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		idSet[n.ID] = struct{}{}
	}
	sortedIDs := make([]string, 0, len(idSet))
	for id := range idSet {
		sortedIDs = append(sortedIDs, id)
	}
	sortStrings(sortedIDs)

	// Integer index for each node.
	idx := make(map[string]int, len(sortedIDs))
	for i, id := range sortedIDs {
		idx[id] = i
	}
	n := len(sortedIDs)

	// Build undirected adjacency (weighted by edge count to handle multi-edges).
	// adj[i] = map of neighbour index → weight
	adj := make([]map[int]float64, n)
	for i := range adj {
		adj[i] = map[int]float64{}
	}
	totalW := 0.0
	for _, e := range edges {
		si, okS := idx[e.SrcID]
		di, okD := idx[e.DstID]
		if !okS || !okD || si == di {
			continue
		}
		adj[si][di]++
		adj[di][si]++
		totalW += 2 // symmetric
	}
	if totalW == 0 {
		// No edges — every node is its own community.
		return assignStableIDs(sortedIDs, func(i int) int { return i })
	}

	// community[i] = community of node i; initially each node is its own.
	community := make([]int, n)
	for i := range community {
		community[i] = i
	}

	// commWeight[c] = sum of weights of all edges incident to nodes in c.
	commWeight := make([]float64, n)
	nodeWeight := make([]float64, n)
	for i := range adj {
		for _, w := range adj[i] {
			nodeWeight[i] += w
		}
		commWeight[i] = nodeWeight[i]
	}

	m2 := totalW // 2m

	improved := true
	for improved {
		improved = false
		for _, i := range makeRange(n) { // stable iteration order
			ci := community[i]
			ki := nodeWeight[i]

			// Compute connection to each neighbouring community.
			kc := map[int]float64{} // community → sum of weights toward it
			for j, w := range adj[i] {
				kc[community[j]] += w
			}

			// Modularity gain of removing i from ci:
			// ΔQ_remove = -(kc[ci] - ki*(commWeight[ci]-ki)/m2) / (m2/2)
			// We'll compute gains relative to best alternative.
			kiCi := kc[ci]
			commWeightCi := commWeight[ci] - ki // weight of ci without i

			bestGain := 0.0
			bestComm := ci
			for cj, kij := range kc {
				if cj == ci {
					continue
				}
				// ΔQ = [kij - ki*commWeight[cj]/m2] / (m2/2)
				// Simplified (drop the /m2 scaling since it's constant):
				gain := kij - ki*commWeight[cj]/m2
				gainSelf := kiCi - ki*commWeightCi/m2
				net := gain - gainSelf
				if net > bestGain+1e-10 {
					bestGain = net
					bestComm = cj
				}
			}

			if bestComm != ci {
				// Move i from ci to bestComm.
				commWeight[ci] -= ki
				community[i] = bestComm
				commWeight[bestComm] += ki
				improved = true
			}
		}
	}

	return assignStableIDs(sortedIDs, func(i int) int { return community[i] })
}

// assignStableIDs converts raw community labels (arbitrary ints) to
// monotonically increasing IDs by sorting nodes within each group and
// assigning IDs in lexicographic order of the first node in each group.
func assignStableIDs(sortedIDs []string, communityOf func(int) int) CommunityResult {
	// raw group → []nodeIndex
	raw := map[int][]int{}
	for i, id := range sortedIDs {
		c := communityOf(i)
		raw[c] = append(raw[c], i)
		_ = id
	}

	// Collect representative (smallest index, which = lexicographically first
	// node ID because sortedIDs is already sorted) for each raw group.
	type rep struct {
		rawID int
		minID string
	}
	reps := make([]rep, 0, len(raw))
	for c, members := range raw {
		reps = append(reps, rep{rawID: c, minID: sortedIDs[members[0]]})
	}
	// Sort reps by their representative node ID for stable label assignment.
	for i := 1; i < len(reps); i++ {
		key := reps[i]
		j := i - 1
		for j >= 0 && reps[j].minID > key.minID {
			reps[j+1] = reps[j]
			j--
		}
		reps[j+1] = key
	}

	rawToStable := make(map[int]int, len(reps))
	for stableID, r := range reps {
		rawToStable[r.rawID] = stableID
	}

	communities := make(map[string]int, len(sortedIDs))
	members := make(map[int][]string, len(reps))
	for i, id := range sortedIDs {
		c := rawToStable[communityOf(i)]
		communities[id] = c
		members[c] = append(members[c], id)
	}
	// Sort members within each community.
	for c := range members {
		sortStrings(members[c])
	}
	return CommunityResult{Communities: communities, Members: members}
}

// makeRange returns [0,n) as a slice for range loops.
func makeRange(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}

func sortStrings(s []string) { sort.Strings(s) }
