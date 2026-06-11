package compress

import "strings"

// tokenRule is a safe ASCII substitution that reduces BPE token count.
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

// ApplyTokenReductions applies BPE-validated ASCII substitutions that reduce
// token count without altering code semantics. Intended as a post-compression
// step in aggressive mode. ext is the file extension (e.g. ".rs", ".ts").
func ApplyTokenReductions(content, ext string) string {
	for _, r := range globalTokenRules {
		content = strings.ReplaceAll(content, r.from, r.to)
	}
	switch ext {
	case ".rs":
		for _, r := range rustTokenRules {
			content = strings.ReplaceAll(content, r.from, r.to)
		}
	case ".ts", ".tsx", ".js", ".jsx":
		for _, r := range jstsTokenRules {
			content = strings.ReplaceAll(content, r.from, r.to)
		}
	}
	return content
}

// applyTokenReductionsExcept is ApplyTokenReductions with anchor protection: a
// rule whose source text overlaps an anchor is skipped, so the anchor is never
// rewritten. This holds the Rust std::sync::Arc → Arc rule (and friends) when
// std::sync::Arc is a qualified-identifier anchor under a strict target_model.
func applyTokenReductionsExcept(content, ext string, a AnchorSet) string {
	apply := func(rules []tokenRule) {
		for _, r := range rules {
			if a.blocksText(r.from) {
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
