package feedback

// Shadow A/B analysis (#731, acceptance 4). The MCP server, when
// DEX_FEEDBACK_SHADOW=1, logs one ShadowRecord per ask: the served (static,
// LORO-calibrated #317) top-k alongside the shadow (lane-agreement reweighted)
// top-k it would have served. This file is the OTHER half — the checker that
// joins those records back against the observe log (hooks.jsonl) and asks the
// one question the data gate hinges on: did the shadow ranking surface more of
// the files the agent ACTUALLY opened than the served ranking did?
//
// The record type lives here, not in internal/mcp, so the writer and this
// reader share a single definition — the same one-parser discipline the rest of
// the package keeps (a second drifting copy is the #734 bug class).

import "fmt"

// ShadowRecord is one A/B comparison for a single ask: the served top-k vs the
// shadow (reweighted) top-k, the signal that drove the reweight, and a
// divergence summary. internal/mcp writes these; AnalyzeShadow consumes them.
type ShadowRecord struct {
	TS           int64    `json:"ts"`
	Intent       string   `json:"intent"`
	OpenRate     float64  `json:"open_rate"`
	N            int      `json:"n"`
	ServedTopK   []string `json:"served_topk"`
	ShadowTopK   []string `json:"shadow_topk"`
	TopKJaccard  float64  `json:"topk_jaccard"`
	MaxRankShift int      `json:"max_rank_shift"`
	Reordered    bool     `json:"reordered"`
}

// ShadowReport is the verdict from joining the shadow log against the observe
// log. Until the reweight diverges from served — it is near-identity at low
// sample counts — Reordered stays ~0 and Verdict is "insufficient".
type ShadowReport struct {
	ShadowLogPath string `json:"shadow_log_path"`
	Records       int    `json:"records"`   // shadow records read
	Matched       int    `json:"matched"`   // joined to an ask in the observe log
	Reordered     int    `json:"reordered"` // matched AND the top-k sets differ

	ServedOpened   int     `json:"served_opened"`
	ShadowOpened   int     `json:"shadow_opened"`
	ServedSlots    int     `json:"served_slots"`
	ShadowSlots    int     `json:"shadow_slots"`
	ServedOpenRate float64 `json:"served_open_rate"`
	ShadowOpenRate float64 `json:"shadow_open_rate"`

	ShadowWins   int `json:"shadow_wins"`   // divergent asks where shadow uniquely caught an opened file
	ShadowLosses int `json:"shadow_losses"` // divergent asks where served uniquely caught an opened file

	Verdict string `json:"verdict"` // insufficient | no-target | win-candidate
	Note    string `json:"note"`
}

// shadowMatchToleranceSec bounds how far apart a shadow record's timestamp and
// its ask event's timestamp may be and still join. Both are time.Now().Unix();
// the observe hook fires just after the ask returns, so a few seconds of slack
// covers the lag without crossing into a neighbouring ask.
const shadowMatchToleranceSec int64 = 30

// ReadShadowLog parses feedback_shadow.jsonl into records (shares the one
// JSONL scanner with ReadLog).
func ReadShadowLog(path string) ([]ShadowRecord, error) {
	out, err := scanJSONL[ShadowRecord](path)
	if err != nil {
		return nil, fmt.Errorf("open shadow log: %w (run with DEX_FEEDBACK_SHADOW=1 first)", err)
	}
	return out, nil
}

// askSlot is an ask event located within its session, so the join can look
// ahead from the ask's position for the files the agent opened.
type askSlot struct {
	intent  string
	ts      int64
	session []Event
	pos     int
	used    bool
}

// askSlots collects every ask event across sessions with its in-session
// position, so a shadow record can be joined to the exact ask that produced it.
func askSlots(events []Event) []*askSlot {
	var slots []*askSlot
	for _, s := range splitSessions(events) {
		for i, e := range s {
			if IsAskTool(e.ToolName) {
				slots = append(slots, &askSlot{intent: e.Intent, ts: e.TS, session: s, pos: i})
			}
		}
	}
	return slots
}

// AnalyzeShadow joins shadow records to the asks that produced them and tallies,
// for each, how many of the served vs shadow top-k paths the agent went on to
// open within the same session. window bounds the per-path lookahead (0 = whole
// session), matching Compute.
func AnalyzeShadow(events []Event, shadow []ShadowRecord, window int) ShadowReport {
	rep := ShadowReport{Records: len(shadow)}
	slots := askSlots(events)

	for _, rec := range shadow {
		slot := matchSlot(slots, rec)
		if slot == nil {
			continue
		}
		slot.used = true
		rep.Matched++

		from := slot.pos + 1
		rep.ServedOpened += countOpened(slot.session, from, rec.ServedTopK, window)
		rep.ShadowOpened += countOpened(slot.session, from, rec.ShadowTopK, window)
		rep.ServedSlots += len(rec.ServedTopK)
		rep.ShadowSlots += len(rec.ShadowTopK)

		// Open-rate only moves when the top-k SETS differ; an order-only change
		// holds the same files. The win/loss tally is the discriminating cut.
		if sameSet(rec.ServedTopK, rec.ShadowTopK) {
			continue
		}
		rep.Reordered++
		shadowWin := openedUnique(slot, from, rec.ShadowTopK, rec.ServedTopK, window)
		servedWin := openedUnique(slot, from, rec.ServedTopK, rec.ShadowTopK, window)
		switch {
		case shadowWin > servedWin:
			rep.ShadowWins++
		case servedWin > shadowWin:
			rep.ShadowLosses++
		}
	}

	rep.ServedOpenRate = ratio(rep.ServedOpened, rep.ServedSlots)
	rep.ShadowOpenRate = ratio(rep.ShadowOpened, rep.ShadowSlots)
	rep.Verdict, rep.Note = shadowVerdict(rep)
	return rep
}

// matchSlot returns the nearest unused ask slot with the same intent within the
// timestamp tolerance, or nil. Conservative: a mismatch leaves the record
// unmatched rather than crediting it to the wrong ask.
func matchSlot(slots []*askSlot, rec ShadowRecord) *askSlot {
	var best *askSlot
	bestDelta := shadowMatchToleranceSec + 1
	for _, sl := range slots {
		if sl.used || sl.intent != rec.Intent {
			continue
		}
		d := rec.TS - sl.ts
		if d < 0 {
			d = -d
		}
		if d <= shadowMatchToleranceSec && d < bestDelta {
			best, bestDelta = sl, d
		}
	}
	return best
}

// countOpened counts how many of paths were opened by a consume tool after the
// ask, within window.
func countOpened(s []Event, from int, paths []string, window int) int {
	n := 0
	for _, p := range paths {
		if sessionOpens(s, from, p, window) {
			n++
		}
	}
	return n
}

// openedUnique counts paths present in a but NOT in b that the agent opened —
// the files one ranking surfaced in its top-k that the other buried.
func openedUnique(slot *askSlot, from int, a, b []string, window int) int {
	bset := toSet(b)
	n := 0
	for _, p := range a {
		if _, in := bset[p]; in {
			continue
		}
		if sessionOpens(slot.session, from, p, window) {
			n++
		}
	}
	return n
}

func toSet(paths []string) map[string]struct{} {
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	return set
}

// sameSet reports whether two path lists hold the same files (order aside).
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := toSet(a)
	for _, p := range b {
		if _, ok := set[p]; !ok {
			return false
		}
	}
	return true
}

// shadowVerdict turns the tally into the data-gate verdict. The flip is only a
// candidate when the divergent asks show the shadow top-k catching more opened
// files than served, both by win/loss and by aggregate open-rate.
func shadowVerdict(r ShadowReport) (string, string) {
	if r.Matched == 0 {
		return "insufficient", "no shadow records joined to an ask in the observe log — accrue traffic with DEX_FEEDBACK_SHADOW=1, then re-run."
	}
	if r.Reordered == 0 {
		return "insufficient", fmt.Sprintf("%d asks matched but the reweight never changed the top-k set (near-identity at low sample counts) — keep accruing until shadow_topk diverges.", r.Matched)
	}
	net := r.ShadowWins - r.ShadowLosses
	delta := r.ShadowOpenRate - r.ServedOpenRate
	switch {
	case net > 0 && delta > 0:
		return "win-candidate", fmt.Sprintf("over %d divergent asks the shadow top-k caught more opened files (wins %d / losses %d, open-rate %+.1f pts) — verify nDCG non-regression in eval before flipping the default.", r.Reordered, r.ShadowWins, r.ShadowLosses, delta*100)
	case net < 0 || delta < 0:
		return "no-target", fmt.Sprintf("over %d divergent asks the reweight did NOT raise open-rate (wins %d / losses %d, open-rate %+.1f pts) — if this holds at volume, close the flip no-target (#562 precedent).", r.Reordered, r.ShadowWins, r.ShadowLosses, delta*100)
	default:
		return "no-target", fmt.Sprintf("over %d divergent asks the reweight was a wash (wins %d / losses %d) — keep accruing or close no-target.", r.Reordered, r.ShadowWins, r.ShadowLosses)
	}
}
