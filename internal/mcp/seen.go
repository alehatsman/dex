package mcp

import (
	"hash/fnv"
	"strconv"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxSeenSessions bounds the cross-turn dedup ledger (#355 L1). A long-lived
// HTTP server with churning session IDs would otherwise retain one *seenState
// per session for the process lifetime. Reaching the cap drops all per-session
// state — best-effort dedup tolerates the reset. Stdio (one sentinel key) never
// approaches it.
const maxSeenSessions = 4096

// seenState is one session's cross-turn dedup ledger: a monotonic turn counter
// and the turn on which each locator key was first surfaced. The key folds a
// content fingerprint into the range (see note), so a range whose bytes changed
// gets a fresh key and is re-inlined rather than suppressed (#138). Guarded by
// the owning Server's seenMu (held for a whole apply pass), so it needs no lock
// of its own.
type seenState struct {
	turn  int            // incremented once per emitting call
	first map[string]int // locator+fingerprint key → turn first surfaced
}

// fingerprint hashes surfaced bytes so the ledger key tracks content, not just
// position. fnv64a is cheap; a collision only re-suppresses bytes that changed,
// degrading to the pre-#138 behaviour — never hiding more than before.
func fingerprint(content string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(content))
	return strconv.FormatUint(h.Sum64(), 16)
}

// sessionKey derives the dedup-ledger key for a request. The MCP SDK gives stdio
// connections an empty ID, but there is exactly one stdio peer per process, so a
// fixed sentinel keys it correctly; HTTP/streamable transports carry real IDs. A
// nil request (CLI / tests) yields "" — dedup is then disabled (one-shot calls
// have no prior turn to dedup against).
func sessionKey(req *sdk.CallToolRequest) string {
	if req == nil || req.Session == nil {
		return ""
	}
	if id := req.Session.ID(); id != "" {
		return id
	}
	return "stdio"
}

// locatorKey identifies a code range for dedup. Empty when the locator isn't a
// real file range (no path, or a pseudo-hit with start_line 0).
func locatorKey(path string, start, end int) string {
	if path == "" || start < 1 {
		return ""
	}
	return path + ":" + strconv.Itoa(start) + "-" + strconv.Itoa(end)
}

// applySeenContext marks every locator in an ask bundle that was already
// surfaced on an earlier turn of the same session, clearing its inlined content
// so the bytes aren't resent. The three lanes share one turn — they all came
// from this single call. A no-op when key is "" (dedup disabled).
func (s *Server) applySeenContext(key string, out *ContextOutput) {
	if key == "" {
		return
	}
	s.seenMu.Lock()
	defer s.seenMu.Unlock()

	if s.seen == nil {
		s.seen = make(map[string]*seenState)
	}
	st := s.seen[key]
	if st == nil {
		// Bound the ledger (#355 L1): on overflow drop the whole map. Active
		// sessions re-send their already-seen ranges once, then rebuild — the
		// key being added here starts fresh either way.
		if len(s.seen) >= maxSeenSessions {
			s.seen = make(map[string]*seenState)
		}
		st = &seenState{first: make(map[string]int)}
		s.seen[key] = st
	}
	st.turn++
	cur := st.turn

	// note records a locator+content key for this turn and reports the first-seen
	// turn when those exact bytes were already surfaced on an EARLIER turn. Folding
	// the content fingerprint into the key means changed bytes miss (fresh key →
	// re-inlined, #138) and lane renderings that differ for one range (raw vs a
	// summary) track independently instead of clobbering each other. A key first
	// seen on the current turn (e.g. the same bytes in two lanes) is not a repeat.
	note := func(path string, start, end int, content string) (firstTurn int, seen bool) {
		base := locatorKey(path, start, end)
		if base == "" {
			return 0, false
		}
		k := base + ":" + fingerprint(content)
		if ft, ok := st.first[k]; ok {
			if ft < cur {
				return ft, true
			}
			return 0, false
		}
		st.first[k] = cur
		return 0, false
	}

	for i := range out.SemanticHits {
		if ft, seen := note(out.SemanticHits[i].Path, out.SemanticHits[i].StartLine, out.SemanticHits[i].EndLine, out.SemanticHits[i].Content); seen {
			out.SemanticHits[i].SeenTurn = ft
			out.SemanticHits[i].Content = ""
		}
	}
	for i := range out.Symbols {
		if ft, seen := note(out.Symbols[i].Path, out.Symbols[i].StartLine, out.Symbols[i].EndLine, out.Symbols[i].Body); seen {
			out.Symbols[i].SeenTurn = ft
			out.Symbols[i].Body = "" // Body is the heavy field; keep signature/doc for orientation
		}
	}
	for i := range out.SuggestedReads {
		if ft, seen := note(out.SuggestedReads[i].Path, out.SuggestedReads[i].StartLine, out.SuggestedReads[i].EndLine, out.SuggestedReads[i].Content); seen {
			out.SuggestedReads[i].SeenTurn = ft
			out.SuggestedReads[i].Content = ""
		}
	}
}
