package compress

import (
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// tokenRule is a candidate ASCII substitution. Whether it actually fires is
// decided per-call against the active tokenizer (see ruleReducesTokens): a
// swap is applied only when it nets a real token reduction for that encoder
// (#292). The tables below are curated to save under o200k; the gate keeps a
// rule from firing where it would not pay off (e.g. a Qwen/Llama BPE where the
// replacement tokenizes no shorter), so adding a rule can never make output
// worse for some target.
type tokenRule struct{ from, to string }

// globalTokenRules apply to every file type.
// All rules use ASCII only — Unicode symbols increase token count on GPT-4/o200k_base.
var globalTokenRules = []tokenRule{
	{" -> ", "->"},     // space-padded arrow: saves ~1 token per occurrence
	{" => ", "=>"},     // space-padded fat arrow
	{"\n\n\n", "\n\n"}, // triple blank line → double
}

// rustTokenRules apply only to .rs files.
var rustTokenRules = []tokenRule{
	{"pub(crate) ", "pub "},
	{"pub(super) ", "pub "},
	{"std::collections::HashMap", "HashMap"},
	{"std::collections::HashSet", "HashSet"},
	{"std::sync::Arc", "Arc"},
	{"std::sync::Mutex", "Mutex"},
	{"std::path::PathBuf", "PathBuf"},
	{"std::io::Result", "io::Result"},
}

// jstsTokenRules apply to .js, .ts, .jsx, .tsx files.
var jstsTokenRules = []tokenRule{
	{"function ", "fn "}, // trailing space prevents matching inside identifiers
	{"boolean", "bool"},  // TS/JS type keyword; lowercase so safe in camelCase contexts
	{"export default ", "export "},
}

// ApplyTokenReductions applies tokenizer-gated ASCII substitutions that reduce
// BPE token count without altering code semantics, using the active tokenizer
// family — the target_model profile (#204), or DefaultFamily until one is set.
// Intended as a post-compression step in aggressive mode. ext is the file
// extension (e.g. ".rs", ".ts").
func ApplyTokenReductions(content, ext string) string {
	return applyTokenReductions(content, ext, tokens.ActiveFamily(), AnchorSet{})
}

// applyTokenReductionsExcept is ApplyTokenReductions with anchor protection: a
// rule whose source text overlaps an anchor is skipped, so the anchor is never
// rewritten (#291). This holds the Rust std::sync::Arc → Arc rule (and friends)
// when std::sync::Arc is a qualified-identifier anchor under a strict
// target_model.
func applyTokenReductionsExcept(content, ext string, a AnchorSet) string {
	return applyTokenReductions(content, ext, tokens.ActiveFamily(), a)
}

// applyTokenReductions is the shared core for both the relaxed and strict
// paths. A rule fires only when both gates pass:
//   - it does not overlap an anchor in a (the #291 floor; the zero AnchorSet
//     blocks nothing, so the relaxed path skips this check by construction);
//   - it nets a real token reduction under fam (the #292 verify-before-apply
//     gate).
func applyTokenReductions(content, ext string, fam tokens.Family, a AnchorSet) string {
	apply := func(rules []tokenRule) {
		for _, r := range rules {
			if a.blocksText(r.from) {
				continue
			}
			if !ruleReducesTokens(r, fam) {
				continue
			}
			content = strings.ReplaceAll(content, r.from, r.to)
		}
	}
	apply(globalTokenRules)
	switch ext {
	case ".rs":
		apply(rustTokenRules)
	case ".ts", ".tsx", ".js", ".jsx":
		apply(jstsTokenRules)
	}
	return content
}

// ruleReducesTokens reports whether r.to tokenizes strictly shorter than r.from
// under fam — the #292 eligibility test. tokens.CountFor memoizes per (string,
// family), so re-checking the static rule tables every call is cheap.
func ruleReducesTokens(r tokenRule, fam tokens.Family) bool {
	return tokens.CountFor(r.to, fam) < tokens.CountFor(r.from, fam)
}
