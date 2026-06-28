package main

import (
	"os"
	"regexp"
	"testing"
)

// dexCmdRef matches `dex <verb>` command invocations in the usage text — only
// where "dex" begins a line (the help tables) or follows a backtick (inline
// code spans), never mid-sentence prose like "a remote dex server".
var dexCmdRef = regexp.MustCompile("(?m)(?:^\\s*|`)dex (-{0,2}[a-z][a-z_-]*)")

// usageReferencedCommands scans main_usage.go for every `dex <verb>` mention.
func usageReferencedCommands(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("main_usage.go")
	if err != nil {
		t.Fatalf("read main_usage.go: %v", err)
	}
	out := map[string]bool{}
	for _, m := range dexCmdRef.FindAllStringSubmatch(string(src), -1) {
		out[m[1]] = true
	}
	return out
}

// TestUsageReferencesAreRealCommands fails if the usage text mentions a
// `dex <verb>` that the registry doesn't know — i.e. a command was renamed or
// removed but the help text still advertises the old name (#469).
func TestUsageReferencesAreRealCommands(t *testing.T) {
	known := map[string]bool{}
	for _, n := range allDispatchNames() {
		known[n] = true
	}
	for n := range metaCommands {
		known[n] = true
	}
	for cmd := range usageReferencedCommands(t) {
		if !known[cmd] {
			t.Errorf("usage text references `dex %s` but it is not a registered command (stale help?)", cmd)
		}
	}
}

// TestAdvertisedCommandsAreDocumented fails if a non-hidden registry command
// is never mentioned in the usage text — i.e. a command was added to the
// dispatch + registry but nobody documented it (the `orient` gap that prompted
// this guard).
func TestAdvertisedCommandsAreDocumented(t *testing.T) {
	referenced := usageReferencedCommands(t)
	for _, v := range verbs {
		if v.group == groupHidden {
			continue
		}
		if !referenced[v.name] {
			t.Errorf("command %q is advertised in the registry but absent from the usage text (main_usage.go)", v.name)
		}
	}
}
