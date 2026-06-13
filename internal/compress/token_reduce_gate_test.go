package compress

import (
	"strconv"
	"testing"

	"github.com/alehatsman/dex/internal/tokens"
)

// allRuleGroups pairs each rule slice with an extension that activates it, so
// tests can drive the live tables. New rules added to any slice are covered
// with no extra wiring.
var allRuleGroups = []struct {
	name  string
	ext   string
	rules []tokenRule
}{
	{"global", ".go", globalTokenRules},
	{"rust", ".rs", rustTokenRules},
	{"jsts", ".ts", jstsTokenRules},
}

// gateFamilies spans both BPE encodings in play (o200k_base for O200kBase /
// Gemini, cl100k_base for Cl100k / Llama) plus the heuristic-backed Gemini, so
// a rule's eligibility is checked under every target_model class.
var gateFamilies = []tokens.Family{tokens.O200kBase, tokens.Cl100k, tokens.Llama, tokens.Gemini}

// TestTokenReductionsGatedByActiveTokenizer is the #292 acceptance: every
// substitution rule fires if and only if its replacement tokenizes strictly
// shorter than its source under the active tokenizer family. A rule that nets
// no reduction (or would cost tokens) for a given encoder is suppressed — so a
// rule curated for o200k can never silently bloat a Qwen/Llama (cl100k) target.
// Table-driven over the live rule slices × families, so future rules inherit
// the gate automatically.
func TestTokenReductionsGatedByActiveTokenizer(t *testing.T) {
	for _, fam := range gateFamilies {
		for _, g := range allRuleGroups {
			for _, r := range g.rules {
				fam, g, r := fam, g, r
				t.Run(fam.String()+"/"+g.name+"/"+strconv.Quote(r.from), func(t *testing.T) {
					content := "lead " + r.from + " trail"
					out := applyTokenReductions(content, g.ext, fam, AnchorSet{})

					applied := out != content
					reduces := tokens.CountFor(r.to, fam) < tokens.CountFor(r.from, fam)
					if applied != reduces {
						t.Errorf("rule %q->%q under %s: applied=%v but reduces=%v (from=%d to=%d tokens)\n in:  %q\n out: %q",
							r.from, r.to, fam, applied, reduces,
							tokens.CountFor(r.from, fam), tokens.CountFor(r.to, fam), content, out)
					}
				})
			}
		}
	}
}

// TestTokenReductionsNeverIncreaseTokens guards the net effect the gate exists
// to guarantee: across every family, running the full reduction pass over a
// mixed sample never raises the token count under that family — the
// "no negative-savings substitution" floor from the #292 acceptance.
func TestTokenReductionsNeverIncreaseTokens(t *testing.T) {
	samples := []struct {
		ext, src string
	}{
		{".rs", "pub(crate) fn f() -> std::sync::Arc<std::collections::HashMap<String, std::io::Result<u8>>> { todo!() }"},
		{".ts", "export default function greet(name: string): boolean { return true => false; }"},
		{".go", "func f(a int) int { return a -> a }\n\n\nx"},
	}
	for _, fam := range gateFamilies {
		for _, s := range samples {
			fam, s := fam, s
			t.Run(fam.String()+"/"+s.ext, func(t *testing.T) {
				out := applyTokenReductions(s.src, s.ext, fam, AnchorSet{})
				before := tokens.CountFor(s.src, fam)
				after := tokens.CountFor(out, fam)
				if after > before {
					t.Errorf("token count increased under %s: %d -> %d\n in:  %q\n out: %q",
						fam, before, after, s.src, out)
				}
			})
		}
	}
}

// TestActiveFamilyDrivesGate confirms the exported entry points read the gate
// family from the tokens package default (set from the #204 target_model
// profile), not a hard-coded encoder — so swapping the active family changes
// which rules fire without threading a tokenizer through the call sites.
func TestActiveFamilyDrivesGate(t *testing.T) {
	prev := tokens.ActiveFamily()
	t.Cleanup(func() { tokens.SetDefaultFamily(prev) })

	// A rule whose eligibility could differ by family is the ideal probe; here
	// every current rule agrees across families, so assert the wiring instead:
	// ApplyTokenReductions must equal the explicit-family core under whatever
	// family is active.
	const src = "let v: boolean = a => b;"
	for _, fam := range gateFamilies {
		tokens.SetDefaultFamily(fam)
		got := ApplyTokenReductions(src, ".ts")
		want := applyTokenReductions(src, ".ts", fam, AnchorSet{})
		if got != want {
			t.Errorf("ApplyTokenReductions ignored active family %s: got %q want %q", fam, got, want)
		}
	}
}
