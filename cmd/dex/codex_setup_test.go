package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/mcp"
)

// TestCodexRulesContentSingleSourced guards the #844 invariant: the Codex
// AGENTS.md block carries the FULL ask-first workflow inline (Codex does not
// surface MCP instructions), single-sourced from mcp.CoreWorkflow so it can
// never drift from the Claude path.
func TestCodexRulesContentSingleSourced(t *testing.T) {
	block := codexRulesContent()
	if !strings.Contains(block, mcp.CoreWorkflow()) {
		t.Fatal("codex rules block does not contain mcp.CoreWorkflow() verbatim")
	}
	if !strings.HasPrefix(block, codexRulesMarker) {
		t.Fatalf("block must start with the marker, got %q…", block[:min(40, len(block))])
	}
	if !strings.Contains(block, codexRulesVersion) {
		t.Fatal("block missing version marker")
	}
	if !strings.HasSuffix(block, codexRulesEndMarker) {
		t.Fatal("block must end with the end marker")
	}
	// The Claude-harness ToolSearch tail must NOT leak into the Codex block.
	if strings.Contains(block, "mcp__dex__") {
		t.Fatal("codex block leaked the Claude-specific mcp__dex__ ToolSearch tail")
	}
}

// TestServerInstructionsExtendsCoreWorkflow guards the mirror invariant: Claude's
// live instructions are CoreWorkflow plus the harness-specific tail.
func TestServerInstructionsExtendsCoreWorkflow(t *testing.T) {
	si := mcp.ServerInstructions()
	if !strings.HasPrefix(si, mcp.CoreWorkflow()) {
		t.Fatal("ServerInstructions no longer begins with CoreWorkflow()")
	}
	if !strings.Contains(si, "ToolSearch") {
		t.Fatal("ServerInstructions lost the Claude ToolSearch tail")
	}
}

func TestCodexEnvSnapshot(t *testing.T) {
	// Clear all core vars, then set two.
	for _, k := range codexCoreVars {
		t.Setenv(k, "")
	}
	t.Setenv("DEX_EMBED_URL", "http://localhost:11434")
	t.Setenv("DEX_CHAT_MODEL", "qwen2.5-coder:14b")

	got := codexEnvSnapshot()
	want := map[string]bool{
		"DEX_EMBED_URL=http://localhost:11434": true,
		"DEX_CHAT_MODEL=qwen2.5-coder:14b":     true,
	}
	if len(got) != len(want) {
		t.Fatalf("snapshot = %v, want exactly the 2 set vars", got)
	}
	for _, kv := range got {
		if !want[kv] {
			t.Fatalf("unexpected entry %q (empty/unset vars must be skipped)", kv)
		}
	}
}

func TestCodexDexConfigured(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want bool
	}{
		{"absent", "[mcp_servers.other]\ncommand = \"x\"\n", false},
		{"wired", "[mcp_servers.dex]\ncommand = \"dex\"\nargs = [\"mcp\"]\n", true},
		{
			"wired with env sub-table after it",
			"[mcp_servers.dex]\ncommand = \"dex\"\nargs = [\"mcp\"]\n\n[mcp_servers.dex.env]\nDEX_EMBED_URL = \"http://x\"\n",
			true,
		},
		{"header only, no command", "[mcp_servers.dex]\n", false},
		{
			"dex command bleeds from a later table is not counted",
			"[mcp_servers.dex]\nfoo = 1\n\n[mcp_servers.other]\ncommand = \"dex\"\nargs = [\"mcp\"]\n",
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codexDexConfigured([]byte(c.toml)); got != c.want {
				t.Fatalf("codexDexConfigured = %v, want %v", got, c.want)
			}
		})
	}
}

// TestCodexEnvDrift confirms the snapshot-vs-live comparison reads the
// [mcp_servers.dex.env] sub-table (regression: a naive cut at the first table
// header excluded the env table and reported every var stale).
func TestCodexEnvDrift(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	for _, k := range codexCoreVars {
		t.Setenv(k, "")
	}
	t.Setenv("DEX_EMBED_URL", "http://live:11434")
	t.Setenv("DEX_CHAT_MODEL", "qwen2.5-coder:14b")

	toml := "[mcp_servers.dex]\ncommand = \"dex\"\nargs = [\"mcp\"]\n\n" +
		"[mcp_servers.dex.env]\n" +
		"DEX_EMBED_URL = \"http://live:11434\"\n" +
		"DEX_CHAT_MODEL = \"qwen2.5-coder:14b\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	if drift := codexEnvDrift(); len(drift) != 0 {
		t.Fatalf("in-sync snapshot reported drift: %v", drift)
	}

	// Change the live value → that var (only) is stale.
	t.Setenv("DEX_EMBED_URL", "http://moved:9999")
	drift := codexEnvDrift()
	if len(drift) != 1 || drift[0] != "DEX_EMBED_URL" {
		t.Fatalf("drift = %v, want [DEX_EMBED_URL]", drift)
	}
}

// TestBuildBlockContentCodex confirms the shared writer inserts the Codex block
// into an existing AGENTS.md without clobbering surrounding content and is
// idempotent on re-run.
func TestBuildBlockContentCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	preamble := "# My project agent notes\nbe careful\n"
	if err := os.WriteFile(path, []byte(preamble), 0o644); err != nil {
		t.Fatal(err)
	}

	action, content, err := buildBlockContent(path, codexRulesMarker, codexRulesEndMarker, codexRulesContent())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != "created" {
		t.Fatalf("action = %q, want created", action)
	}
	if !strings.HasPrefix(content, preamble) {
		t.Fatal("writer clobbered existing content")
	}
	if extractBlock(content, codexRulesMarker, codexRulesEndMarker) != codexRulesContent() {
		t.Fatal("inserted block does not round-trip")
	}

	// Idempotent: writing the now-canonical file again is a no-op.
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	action2, _, err := buildBlockContent(path, codexRulesMarker, codexRulesEndMarker, codexRulesContent())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action2 != "already up to date" {
		t.Fatalf("second run action = %q, want already up to date", action2)
	}
}
