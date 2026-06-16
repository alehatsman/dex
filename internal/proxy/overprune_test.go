package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/tokens"
)

// readToolUseMsg builds an assistant message with one Read tool_use carrying a
// file_path input — what AnalyzeReReads resolves paths from.
func readToolUseMsg(id, path string) any {
	return map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  "Read",
				"input": map[string]any{"file_path": path},
			},
		},
	}
}

// big returns a string of n lines so reads clear minPruneChars.
func big(n int) string {
	return strings.Repeat("the quick brown fox jumped over the lazy dog\n", n)
}

// filler builds a throwaway user turn to pad the message list so the reads
// under test land in the OLD (pre-keepRecent) region.
func filler() any {
	return map[string]any{"role": "user", "content": "ok"}
}

func TestAnalyzeReReads_DetectsReadAfterStub(t *testing.T) {
	// Old region: read of foo.go (gets stubbed). Recent region: foo.go read
	// again → one re-read-after-stub event.
	messages := makeMessages(t,
		readToolUseMsg("t1", "/repo/foo.go"),
		toolResultMsg("t1", big(50)),
		filler(), filler(), filler(), filler(),
		readToolUseMsg("t2", "/repo/foo.go"),
		toolResultMsg("t2", big(50)),
	)
	got := AnalyzeReReads(messages, 2, tokens.Cl100k)
	if got.ReReads != 1 {
		t.Fatalf("ReReads = %d, want 1", got.ReReads)
	}
	if got.ReReadTokens <= 0 {
		t.Fatalf("ReReadTokens = %d, want > 0", got.ReReadTokens)
	}
}

func TestAnalyzeReReads_NoRecurrenceNoSignal(t *testing.T) {
	// foo.go read once in the old region, never re-read → no signal.
	messages := makeMessages(t,
		readToolUseMsg("t1", "/repo/foo.go"),
		toolResultMsg("t1", big(50)),
		filler(), filler(), filler(), filler(),
		readToolUseMsg("t2", "/repo/bar.go"),
		toolResultMsg("t2", big(50)),
	)
	if got := AnalyzeReReads(messages, 2, tokens.Cl100k); got.ReReads != 0 {
		t.Fatalf("ReReads = %d, want 0", got.ReReads)
	}
}

func TestAnalyzeReReads_ShortOldReadNotCounted(t *testing.T) {
	// The old read is below minPruneChars, so PruneHistory leaves it verbatim —
	// re-reading it is not an over-pruning cost.
	messages := makeMessages(t,
		readToolUseMsg("t1", "/repo/foo.go"),
		toolResultMsg("t1", "tiny"),
		filler(), filler(), filler(), filler(),
		readToolUseMsg("t2", "/repo/foo.go"),
		toolResultMsg("t2", big(50)),
	)
	if got := AnalyzeReReads(messages, 2, tokens.Cl100k); got.ReReads != 0 {
		t.Fatalf("ReReads = %d, want 0 (old read below minPruneChars)", got.ReReads)
	}
}

func TestAnalyzeReReads_FitsInWindowIsNoOp(t *testing.T) {
	// Everything inside keepRecent — nothing gets stubbed, nothing to flag.
	messages := makeMessages(t,
		readToolUseMsg("t1", "/repo/foo.go"),
		toolResultMsg("t1", big(50)),
		readToolUseMsg("t2", "/repo/foo.go"),
		toolResultMsg("t2", big(50)),
	)
	if got := AnalyzeReReads(messages, 10, tokens.Cl100k); got.ReReads != 0 {
		t.Fatalf("ReReads = %d, want 0 (fits in keep-window)", got.ReReads)
	}
}

func TestAnalyzeReReadsBody_FailOpenOnGarbage(t *testing.T) {
	if got := AnalyzeReReadsBody([]byte("not json"), 10); got != (ReReadStats{}) {
		t.Fatalf("garbage body = %+v, want zero", got)
	}
}

func TestAnalyzeReReadsBody_EndToEnd(t *testing.T) {
	messages := makeMessages(t,
		readToolUseMsg("t1", "/repo/foo.go"),
		toolResultMsg("t1", big(50)),
		filler(), filler(), filler(), filler(),
		readToolUseMsg("t2", "/repo/foo.go"),
		toolResultMsg("t2", big(50)),
	)
	body, err := json.Marshal(map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": messages,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	if got := AnalyzeReReadsBody(body, 2); got.ReReads != 1 {
		t.Fatalf("ReReads = %d, want 1", got.ReReads)
	}
}
