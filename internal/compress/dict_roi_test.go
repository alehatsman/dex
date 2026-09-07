package compress_test

import (
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/tokens"
)

// refCount counts substitution refs across all three dictionary namespaces
// (§N Codebook, αN SymbolMap, ©N NgramCodebook).
func refCount(s string) int {
	return strings.Count(s, "§") + strings.Count(s, "α") + strings.Count(s, "©")
}

// TestDictPassesNeverGrowOutput pins #866: a dictionary pass is kept only when
// it actually costs fewer tokens.
//
// Each pass prepends a legend and swaps content tokens for refs. Its own ROI
// gate estimates the trade from occurrence counts, and that estimate can be
// wrong — measured on real files, the passes ADD tokens to Markdown and JSON
// while removing them from YAML, go.sum and Go. A pass that grows the output is
// a pure loss: the reader pays the legibility cost of substituted content and a
// legend, and gets nothing back. This is the property that makes aggressive
// output safe to hand a reader regardless of file type.
func TestDictPassesNeverGrowOutput(t *testing.T) {
	// Repetitive key/value prose — the shape that makes the n-gram codebook fire
	// hardest while saving nothing, because there is no code structure to strip.
	var md strings.Builder
	md.WriteString("# Config reference\n\n")
	for i := range 60 {
		md.WriteString("- `watch_path_")
		md.WriteString(string(rune('a' + i%26)))
		md.WriteString("` — the directory the watcher observes for changes.\n")
	}

	cases := map[string]struct{ content, ext string }{
		"markdown prose": {md.String(), ".md"},
		"json config":    {strings.Repeat(`{"enabled": true, "path": "/var/log/app"},`+"\n", 60), ".json"},
		"yaml config":    {strings.Repeat("service_name: api-gateway\n  retries: 3\n", 60), ".yml"},
		"go source":      {strings.Repeat("func handleRequest(ctx context.Context) error { return nil }\n", 60), ".go"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for _, strict := range []bool{false, true} {
				out := compress.CompressCode(tc.content, tc.ext, strict)
				in, got := tokens.Count(tc.content), tokens.Count(out)
				t.Logf("DBG strict=%v ext=%s in=%d got=%d refs=%d", strict, tc.ext, in, got, refCount(out))
				if got > in {
					t.Errorf("strict=%v: compression GREW the input, %d -> %d tokens (%d refs) — a dictionary pass that does not pay for its legend must be discarded",
						strict, in, got, refCount(out))
				}
				// The stronger half of the contract: if nothing was gained, nothing
				// may be substituted. Refs with no saving is the #866 failure mode.
				if got == in && refCount(out) > 0 {
					t.Errorf("strict=%v: %d refs substituted for zero token saving — the reader pays the legibility cost and gets nothing",
						strict, refCount(out))
				}
			}
		})
	}
}

// TestDictPassesStillFireWhenProfitable is the counterweight: the gate must not
// be so eager that it disables the passes wherever they earn their keep. Go
// source with heavy identifier repetition is the case they exist for.
func TestDictPassesStillFireWhenProfitable(t *testing.T) {
	var b strings.Builder
	for i := range 80 {
		b.WriteString("\tresultCollection = appendToResultCollection(resultCollection, transformInputRecord(inputRecord))\n")
		if i%10 == 0 {
			b.WriteString("\tlogger.Debug(\"appendToResultCollection\", resultCollection)\n")
		}
	}
	src := "package main\n\nfunc process() {\n" + b.String() + "}\n"

	out := compress.CompressCode(src, ".go", false)
	if tokens.Count(out) >= tokens.Count(src) {
		t.Fatalf("aggressive compression saved nothing on heavily repetitive Go: %d -> %d tokens", tokens.Count(src), tokens.Count(out))
	}
	if refCount(out) == 0 {
		t.Error("no dictionary refs on heavily repetitive Go — the ROI gate is now too strict to ever pay off")
	}
}
