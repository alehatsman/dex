package mcp

import (
	"time"

	"github.com/alehatsman/dex/internal/retrieve"
)

// fromPackTrust projects retrieve.Trust (#95c) onto the single EnvTrust shape
// shared by every verb (#110 step 2 — ask no longer has its own trustEnvelope).
// Provenance is "semantic" (ask is the embedding-retrieval verb); the two
// freshness booleans (stale, indexing) fold into one `fresh` plus a caveat when
// an index build is in flight, losing no information. Returns nil when every
// field is zero so empty (no-lane) responses stay byte-neutral. In practice
// Confidence is always set on an assembled pack, so trust is present on every
// real ask.
func fromPackTrust(t retrieve.Trust) *EnvTrust {
	if !t.Stale && !t.Indexing && t.IndexedAt.IsZero() && t.Confidence == "" &&
		t.TopScore == 0 && !t.LowConf && !t.GraphResolved && !t.RecallPartial && t.Caveat == "" {
		return nil
	}
	fresh := !t.Stale && !t.Indexing
	env := &EnvTrust{
		Provenance:    "semantic",
		Fresh:         &fresh,
		Confidence:    t.Confidence,
		TopScore:      t.TopScore,
		LowConfidence: t.LowConf,
		GraphResolved: t.GraphResolved,
		RecallPartial: t.RecallPartial,
		Caveat:        t.Caveat,
	}
	if t.Indexing && env.Caveat == "" {
		env.Caveat = "index build in progress — results may be incomplete"
	}
	if !t.IndexedAt.IsZero() {
		env.IndexedAt = t.IndexedAt.UTC().Format(time.RFC3339)
	}
	return env
}

// Neutral → wire projection for the assembled ContextPack (#95a / #103).
// The evidence core is assembled in internal/retrieve over transport-free
// types; these adapters stamp the domain pack fields onto the wire
// ContextOutput slices. The inline overlay (Content/Body/Imports),
// #344 handles and seen-turn dedup are layered on later at the edge, so
// they are copied through where the pack already carries them (Content on
// reads) and left zero otherwise. Field-explicit by design so an added
// wire field never silently reads back an unset domain value.

func fromPackSyms(in []retrieve.SymbolHit) []SymbolHit {
	if len(in) == 0 {
		return nil
	}
	out := make([]SymbolHit, len(in))
	for i := range in {
		out[i] = SymbolHit{
			QualifiedName: in[i].QualifiedName,
			Path:          in[i].Path,
			StartLine:     in[i].StartLine,
			EndLine:       in[i].EndLine,
			Kind:          in[i].Kind,
			Signature:     in[i].Signature,
			Doc:           in[i].Doc,
			Body:          in[i].Body,
			Role:          in[i].Role,
			Truncated:     in[i].Truncated,
			Handle:        in[i].Handle,
			SeenTurn:      in[i].SeenTurn,
		}
	}
	return out
}

func fromPackSems(in []retrieve.SemHit) []SemHit {
	if len(in) == 0 {
		return nil
	}
	out := make([]SemHit, len(in))
	for i := range in {
		out[i] = SemHit{
			Path:      in[i].Path,
			StartLine: in[i].StartLine,
			EndLine:   in[i].EndLine,
			Kind:      in[i].Kind,
			Score:     in[i].Score,
			Reason:    in[i].Reason,
			Lanes:     in[i].Lanes.Names(),
			Content:   in[i].Content,
			Truncated: in[i].Truncated,
		}
	}
	return out
}

func fromPackReads(in []retrieve.SuggestedRead) []SuggestedRead {
	if len(in) == 0 {
		return nil
	}
	out := make([]SuggestedRead, len(in))
	for i := range in {
		out[i] = SuggestedRead{
			Path:      in[i].Path,
			StartLine: in[i].StartLine,
			EndLine:   in[i].EndLine,
			Reason:    in[i].Reason,
			Content:   in[i].Content,
			Truncated: in[i].Truncated,
			Imports:   in[i].Imports,
		}
	}
	return out
}

func fromPackGraph(in *retrieve.GraphResult) *GraphResult {
	if in == nil {
		return nil
	}
	out := &GraphResult{
		Nodes: make([]GraphNode, len(in.Nodes)),
		Edges: make([]GraphEdge, len(in.Edges)),
	}
	for i := range in.Nodes {
		out.Nodes[i] = GraphNode{
			ID:            in.Nodes[i].ID,
			QualifiedName: in.Nodes[i].QualifiedName,
			Kind:          in.Nodes[i].Kind,
			InDegree:      in.Nodes[i].InDegree,
			OutDegree:     in.Nodes[i].OutDegree,
			PageRank:      in.Nodes[i].PageRank,
		}
	}
	for i := range in.Edges {
		out.Edges[i] = GraphEdge{
			From: in.Edges[i].From,
			To:   in.Edges[i].To,
			Kind: in.Edges[i].Kind,
		}
	}
	return out
}
