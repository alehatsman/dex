package mcp

import (
	"encoding/json"

	"github.com/alehatsman/dex/internal/tokens"
)

// The universal response envelope for the four-verb surface (#110,
// specs/tool-surface.md). Every verb response carries the same top-level shape —
// {result, trust, cost, next, handles} — so an agent reads provenance, cost, and
// the suggested next move the same way regardless of which verb it called.
//
// Introduced additively: the new verbs (act/remember first, then look/ask) embed
// these fields while the legacy tools keep their existing shapes as aliases until
// the cutover. Only the fields a verb can populate are set; the rest are omitempty
// so a deterministic verb like `act` isn't padded with empty semantic-trust
// fields. (`handles` reuses the existing encoded-handle string form from
// envelope.go and is added per-verb when look/ask land.)

// EnvTrust is the provenance envelope: can the agent trust this result, and how
// was it derived. `provenance` is always set; the freshness/confidence fields
// apply only to index-backed semantic verbs.
type EnvTrust struct {
	// Provenance is how the result was produced: "exact" (deterministic — a
	// command ran, a note was read/written, a file was fetched), "semantic"
	// (embedding retrieval), or "name-based" (tree-sitter symbol/graph match with
	// partial recall).
	Provenance string `json:"provenance"`
	// Fresh reports whether the index backing this result is up to date with the
	// working tree; nil for verbs that don't touch the index (act).
	Fresh *bool `json:"fresh,omitempty"`
	// IndexedAt is when the backing index was last built (RFC3339); empty for
	// index-independent verbs.
	IndexedAt string `json:"indexed_at,omitempty"`
	// Confidence is a coarse high|medium|low band for inference-bearing verbs
	// (ask); empty for exact verbs.
	Confidence string `json:"confidence,omitempty"`
	// Caveat is a one-line honesty note, e.g. "name-based recall may be partial".
	Caveat string `json:"caveat,omitempty"`

	// Evidence signals — set by the semantic verb (ask) only; omitempty so exact
	// verbs still project to {provenance:"exact"}. Folded in from the former
	// parallel trustEnvelope (#110 step 2) so ask shares this one trust shape.
	TopScore      float32 `json:"top_score,omitempty"`      // top retrieval score
	LowConfidence bool    `json:"low_confidence,omitempty"` // self-assessed weak match
	GraphResolved bool    `json:"graph_resolved,omitempty"` // call-graph edges resolved
	RecallPartial bool    `json:"recall_partial,omitempty"` // name-based recall may be partial
}

// EnvCost reports what the response cost the caller's context budget and how much
// dex saved by compressing/curating, so the agent can pace itself.
type EnvCost struct {
	TokensReturned int `json:"tokens_returned,omitempty"`
	SavedPct       int `json:"saved_pct,omitempty"`
	BudgetLeft     int `json:"budget_left,omitempty"`
}

// NextStep is a suggested follow-up call — the verb to run, the arguments to run
// it with, and why. It turns a dead-end response into a routed next move.
type NextStep struct {
	Verb string         `json:"verb"`
	Args map[string]any `json:"args,omitempty"`
	Why  string         `json:"why,omitempty"`
}

// exactTrust is the envelope for a deterministic verb: the result is exactly what
// was asked for, no index or inference involved.
func exactTrust() EnvTrust { return EnvTrust{Provenance: "exact"} }

// costStamper is implemented by the four-verb outputs so the addTool choke point
// records cost.tokens_returned (and budget_left when the caller passed a budget)
// uniformly (#110 step 2), without each handler duplicating the math.
type costStamper interface {
	stampCost(tokensReturned, budgetLeft int)
}

// budgetCarrier is implemented by the four-verb inputs that accept an optional
// token budget; when >0 the envelope reports budget_left = budget − tokens_returned.
type budgetCarrier interface {
	budgetTokens() int
}

// stampEnvelopeCost measures a verb output's serialized size and records it as
// cost.tokens_returned, plus budget_left when the input carried a budget. No-op
// for outputs that don't implement costStamper (every non-verb tool), so the
// generic choke point stays cheap and only the four verbs opt in.
func stampEnvelopeCost(out any, in any) {
	cs, ok := out.(costStamper)
	if !ok {
		return
	}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	toks := tokens.Count(string(b))
	left := 0
	if bc, ok := in.(budgetCarrier); ok {
		if budget := bc.budgetTokens(); budget > 0 {
			if left = budget - toks; left < 0 {
				left = 0
			}
		}
	}
	cs.stampCost(toks, left)
}

// withCost folds a measured token count (and optional budget_left) into an
// EnvCost, preserving any saved_pct a verb already recorded.
func withCost(c *EnvCost, tokensReturned, budgetLeft int) *EnvCost {
	if c == nil {
		c = &EnvCost{}
	}
	c.TokensReturned = tokensReturned
	if budgetLeft > 0 {
		c.BudgetLeft = budgetLeft
	}
	return c
}

func (o *ContextOutput) stampCost(t, left int)  { o.Cost = withCost(o.Cost, t, left) }
func (o *LookOutput) stampCost(t, left int)     { o.Cost = withCost(o.Cost, t, left) }
func (o *RememberOutput) stampCost(t, left int) { o.Cost = withCost(o.Cost, t, left) }

func (in ContextInput) budgetTokens() int  { return in.Budget }
func (in LookInput) budgetTokens() int     { return in.Budget }
func (in RememberInput) budgetTokens() int { return in.Budget }
