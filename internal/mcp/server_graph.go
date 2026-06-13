package mcp

// server_graph.go holds the graph-only MCP tools: graph_deps,
// graph_callers, graph_callees. Each handler reads the static graph
// (graph_nodes / graph_edges) via loadGraphView and never touches the
// embedding or chat endpoints — making these the cheapest tools in
// the surface and useful as a precise fallback when semantic search
// drifts.
