package proxy

import "sync/atomic"

// Stats holds cumulative per-session counters for the proxy.
// All fields are safe for concurrent access via atomic operations.
type Stats struct {
	RequestsTotal      atomic.Int64
	RequestsCompressed atomic.Int64 // at least one pass saved tokens
	TokensBefore       atomic.Int64
	TokensAfter        atomic.Int64

	// Cache-breakpoint counters (#241): how many requests had breakpoints
	// placed, the total markers emitted, and the running stable/volatile token
	// split that yields the cache-efficiency ratio.
	RequestsCacheAligned atomic.Int64
	CacheBreakpoints     atomic.Int64
	StableTokens         atomic.Int64
	VolatileTokens       atomic.Int64

	// Tool-description compression counters (#242): how many requests had at
	// least one description rewritten, and the total descriptions changed.
	RequestsToolDescCompressed atomic.Int64
	ToolDescsCompressed        atomic.Int64

	// Over-pruning counters (#561): the cost side of pruning — file reads stubbed
	// in the old region that the agent re-read inside the keep-window, and the
	// tokens those re-fetches cost. A rising ReReadTokens relative to TokensSaved
	// is the signal that pruning is too aggressive.
	ReReadsAfterStub atomic.Int64
	ReReadTokens     atomic.Int64

	// Dup-in-window counters (#562 target probe): same file read more than once
	// with all copies inside the keep-window. DupReadTokens is the upper bound a
	// cross-read dedup pass could reclaim — if it stays ~0 in real traffic, #562
	// has no target.
	DupReadsInWindow atomic.Int64
	DupReadTokens    atomic.Int64

	// Provider-reported token counts from SSE usage chunks (#57).
	InputTokens      atomic.Int64
	OutputTokens     atomic.Int64
	CacheReadTokens  atomic.Int64
	CacheWriteTokens atomic.Int64
	ReasoningTokens  atomic.Int64

	// Model routing counters: requests where the model field was rewritten.
	RequestsRouted atomic.Int64

	// Edit-fail-after-read counter (#58): how many times a compressed file read
	// in the old region was followed by a failed Edit on the same path.
	EditFails atomic.Int64

	// Result-preservation counters (#45): tool_result blocks in the old region
	// kept verbatim because already compressed (dex saved_pct > 0), carrying
	// <lc_safe>, or containing test/build output.
	ResultsPreserved atomic.Int64
	TokensPreserved  atomic.Int64
}

// Snapshot is a JSON-serializable point-in-time view of Stats.
type Snapshot struct {
	RequestsTotal      int64   `json:"requests_total"`
	RequestsCompressed int64   `json:"requests_compressed"`
	TokensBefore       int64   `json:"tokens_before"`
	TokensAfter        int64   `json:"tokens_after"`
	TokensSaved        int64   `json:"tokens_saved"`
	CompressionRatio   float64 `json:"compression_ratio"` // tokens_saved/tokens_before; 0 if no traffic

	RequestsCacheAligned int64   `json:"requests_cache_aligned"`
	CacheBreakpoints     int64   `json:"cache_breakpoints"`
	CacheEfficiency      float64 `json:"cache_efficiency"` // stable/(stable+volatile); 0 if no traffic

	RequestsToolDescCompressed int64 `json:"requests_tool_desc_compressed"`
	ToolDescsCompressed        int64 `json:"tool_descs_compressed"`

	// Over-pruning signal (#561): re-reads of files the pruner stubbed, and the
	// tokens those re-fetches cost. Read alongside TokensSaved to judge whether
	// pruning is paying off net of the re-reads it induces.
	ReReadsAfterStub int64 `json:"rereads_after_stub"`
	ReReadTokens     int64 `json:"reread_tokens"`

	// Dup-in-window probe (#562): redundant in-window reads and the tokens a
	// dedup pass could reclaim. ~0 here means #562 has no target.
	DupReadsInWindow int64 `json:"dup_reads_in_window"`
	DupReadTokens    int64 `json:"dup_read_tokens"`

	// Provider-reported token counts extracted from SSE usage chunks (#57).
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`

	// Model routing: requests where the model field was rewritten by the proxy.
	RequestsRouted int64 `json:"requests_routed"`

	// Edit-fail counter (#58): compressed reads followed by Edit failures.
	EditFails int64 `json:"edit_fails"`

	// Result-preservation counters (#45): old-region tool_result blocks kept
	// verbatim rather than stub-replaced.
	ResultsPreserved int64 `json:"results_preserved"`
	TokensPreserved  int64 `json:"tokens_preserved"`
}

// record adds one request's before/after token counts to the cumulative totals.
func (s *Stats) record(before, after int64) {
	s.RequestsTotal.Add(1)
	if after < before {
		s.RequestsCompressed.Add(1)
	}
	s.TokensBefore.Add(before)
	s.TokensAfter.Add(after)
}

// recordCache folds one request's cache-alignment outcome into the totals. The
// stable/volatile split accumulates even when no breakpoint was applied, so the
// cumulative efficiency reflects the whole stream's cacheable share.
func (s *Stats) recordCache(c CacheStats) {
	if c.Applied {
		s.RequestsCacheAligned.Add(1)
		s.CacheBreakpoints.Add(int64(c.Breakpoints))
	}
	s.StableTokens.Add(int64(c.StableTokens))
	s.VolatileTokens.Add(int64(c.VolatileTokens))
}

// recordToolDesc folds one request's tool-description compression outcome into
// the totals: how many requests had at least one description rewritten and the
// running count of descriptions changed.
func (s *Stats) recordToolDesc(t ToolDescStats) {
	if t.Applied {
		s.RequestsToolDescCompressed.Add(1)
		s.ToolDescsCompressed.Add(int64(t.ToolsCompressed))
	}
}

// recordRoute folds one request's model-routing outcome into the totals.
func (s *Stats) recordRoute(r ModelRouteStats) {
	if r.Applied {
		s.RequestsRouted.Add(1)
	}
}

// recordReReads folds one request's over-pruning signal into the totals: how
// many stubbed files the agent re-read in the keep-window and the tokens those
// re-fetches cost.
func (s *Stats) recordReReads(r ReReadStats) {
	if r.ReReads > 0 {
		s.ReReadsAfterStub.Add(int64(r.ReReads))
		s.ReReadTokens.Add(int64(r.ReReadTokens))
	}
	if r.DupReadsInWindow > 0 {
		s.DupReadsInWindow.Add(int64(r.DupReadsInWindow))
		s.DupReadTokens.Add(int64(r.DupReadTokens))
	}
}

// recordEditFails folds one request's edit-fail signal into the totals and
// invokes hook (when non-nil) for each path that triggered the signal.
func (s *Stats) recordEditFails(ef EditFailStats, hook func(string)) {
	if ef.EditFails > 0 {
		s.EditFails.Add(int64(ef.EditFails))
	}
	if hook != nil {
		for _, p := range ef.Paths {
			hook(p)
		}
	}
}

// recordPrune folds one request's PruneHistoryStats into the running totals.
func (s *Stats) recordPrune(p PruneHistoryStats) {
	if p.ResultsPreserved > 0 {
		s.ResultsPreserved.Add(int64(p.ResultsPreserved))
		s.TokensPreserved.Add(int64(p.TokensPreserved))
	}
}

// recordUsage folds provider-reported SSE token counts into the running totals.
func (s *Stats) recordUsage(u ProviderUsage) {
	if u.InputTokens != 0 {
		s.InputTokens.Add(u.InputTokens)
	}
	if u.OutputTokens != 0 {
		s.OutputTokens.Add(u.OutputTokens)
	}
	if u.CacheReadTokens != 0 {
		s.CacheReadTokens.Add(u.CacheReadTokens)
	}
	if u.CacheWriteTokens != 0 {
		s.CacheWriteTokens.Add(u.CacheWriteTokens)
	}
	if u.ReasoningTokens != 0 {
		s.ReasoningTokens.Add(u.ReasoningTokens)
	}
}

// Snapshot returns a consistent point-in-time view of the stats.
func (s *Stats) Snapshot() Snapshot {
	before := s.TokensBefore.Load()
	after := s.TokensAfter.Load()
	saved := before - after
	if saved < 0 {
		saved = 0
	}
	var ratio float64
	if before > 0 {
		ratio = float64(saved) / float64(before)
	}
	stable := s.StableTokens.Load()
	volatile := s.VolatileTokens.Load()
	var cacheEff float64
	if stable+volatile > 0 {
		cacheEff = float64(stable) / float64(stable+volatile)
	}
	return Snapshot{
		RequestsTotal:        s.RequestsTotal.Load(),
		RequestsCompressed:   s.RequestsCompressed.Load(),
		TokensBefore:         before,
		TokensAfter:          after,
		TokensSaved:          saved,
		CompressionRatio:     ratio,
		RequestsCacheAligned: s.RequestsCacheAligned.Load(),
		CacheBreakpoints:     s.CacheBreakpoints.Load(),
		CacheEfficiency:      cacheEff,

		RequestsToolDescCompressed: s.RequestsToolDescCompressed.Load(),
		ToolDescsCompressed:        s.ToolDescsCompressed.Load(),

		ReReadsAfterStub: s.ReReadsAfterStub.Load(),
		ReReadTokens:     s.ReReadTokens.Load(),

		DupReadsInWindow: s.DupReadsInWindow.Load(),
		DupReadTokens:    s.DupReadTokens.Load(),

		InputTokens:      s.InputTokens.Load(),
		OutputTokens:     s.OutputTokens.Load(),
		CacheReadTokens:  s.CacheReadTokens.Load(),
		CacheWriteTokens: s.CacheWriteTokens.Load(),
		ReasoningTokens:  s.ReasoningTokens.Load(),

		RequestsRouted: s.RequestsRouted.Load(),

		EditFails: s.EditFails.Load(),

		ResultsPreserved: s.ResultsPreserved.Load(),
		TokensPreserved:  s.TokensPreserved.Load(),
	}
}
