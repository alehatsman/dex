package mcp

// query_result.go holds the flat, lane-keyed result union for the query verb and
// the projections that build it (#207 / docs/design/95g-query-wire-collapse.md).
//
// The former shape nested a union inside a union — QueryResult{Look, Ask} where
// Look re-wrapped LookResult{Read,Grep,Trace,Locate} and re-declared the envelope
// fields, and Ask was a 35-field god-struct (ContextOutput) serving five intents.
// The clean break: route.lane is the single discriminator, exactly one lane
// pointer below is populated, and the envelope (status/trust/cost/next) lives
// once on QueryOutput. mcp holds projection here, not a second envelope.

// SemanticResult is the evidence payload for the five semantic evidence intents
// (search / editing / assemble / architecture / packages). It is the former
// ContextOutput with the envelope fields (status/hint/trust/cost/next) dropped —
// they are hoisted to QueryOutput and were pure duplication under result.ask —
// and with the orient (map) and review lanes pulled out to their own payloads.
type SemanticResult struct {
	Answer      string `json:"answer,omitempty"`
	AnswerModel string `json:"answer_model,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	Project     string `json:"project,omitempty"`
	Intent      string `json:"intent,omitempty"`

	SemanticHits   []SemHit            `json:"semantic_hits,omitempty"`
	Symbols        []SymbolHit         `json:"symbols,omitempty"`
	Graph          *GraphResult        `json:"graph,omitempty"`
	SuggestedReads []SuggestedRead     `json:"suggested_reads,omitempty"`
	References     []RefHit            `json:"references,omitempty"`
	Annotations    map[string]PathMeta `json:"annotations,omitempty"`

	NextAction string `json:"next_action,omitempty"`
	Avoid      string `json:"avoid,omitempty"`

	SessionTask         string            `json:"session_task,omitempty"`
	ContentBytesInlined int               `json:"content_bytes_inlined,omitempty"`
	Expanded            bool              `json:"expanded,omitempty"`
	RelatedFiles        []string          `json:"related_files,omitempty"`
	Rules               []string          `json:"rules,omitempty"`
	Concerns            *AssembleConcerns `json:"concerns,omitempty"`
}

// OrientResult is the session-start orientation payload (empty question →
// IntentOrient): the deterministic map plus its routing prose. Its own type so
// orient's answer isn't three live fields on a struct built for five other
// intents.
type OrientResult struct {
	Map        string `json:"map,omitempty"`
	NextAction string `json:"next_action,omitempty"`
	Avoid      string `json:"avoid,omitempty"`
}

// semanticResultFrom projects the router's internal ContextOutput onto the wire
// SemanticResult, copying only the evidence fields. The envelope fields
// (status/hint/trust/cost/next) are dropped — QueryOutput already carries them —
// and map/review belong to their own lanes.
func semanticResultFrom(co *ContextOutput) *SemanticResult {
	return &SemanticResult{
		Answer:      co.Answer,
		AnswerModel: co.AnswerModel,
		Endpoint:    co.Endpoint,
		Project:     co.Project,
		Intent:      co.Intent,

		SemanticHits:   co.SemanticHits,
		Symbols:        co.Symbols,
		Graph:          co.Graph,
		SuggestedReads: co.SuggestedReads,
		References:     co.References,
		Annotations:    co.Annotations,

		NextAction: co.NextAction,
		Avoid:      co.Avoid,

		SessionTask:         co.SessionTask,
		ContentBytesInlined: co.ContentBytesInlined,
		Expanded:            co.Expanded,
		RelatedFiles:        co.RelatedFiles,
		Rules:               co.Rules,
		Concerns:            co.Concerns,
	}
}

// semanticLane projects the router's ContextOutput into the flat QueryResult lane
// its resolved intent names, and returns the wire lane name for route.lane.
// orient → OrientResult, review → the ReviewOutput it already built, everything
// else → SemanticResult. Keyed on co.Intent, which orientResponse/reviewResponse
// set authoritatively.
func semanticLane(co *ContextOutput) (QueryResult, string) {
	switch co.Intent {
	case "orient":
		return QueryResult{Orient: &OrientResult{Map: co.Map, NextAction: co.NextAction, Avoid: co.Avoid}}, "orient"
	case "review":
		return QueryResult{Review: co.Review}, "review"
	default:
		return QueryResult{Semantic: semanticResultFrom(co)}, "semantic"
	}
}
