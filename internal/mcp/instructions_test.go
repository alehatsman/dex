package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
)

// instructionTools are the tool names ServerInstructions() steers agents
// toward. Each must be a really-registered tool, or the instruction block
// sends agents chasing names that don't exist (the #325 drift).
var instructionTools = []string{
	"ask", "map", "find", "trace", "read", "shell", "grep",
}

// deadToolNames are pre-rename names that must never reappear in the
// instructions. They were the bug: ServerInstructions advertised them
// while the registry used the short names above.
var deadToolNames = []string{
	"search_semantic", "search_symbol", "ctx_shell", "file_view",
	"graph_callers", "graph_callees", "graph_deps", "graph_neighbors",
}

// goodParamSignatures are the tool mnemonics whose param names match the real
// input schema. Each MUST appear verbatim in ServerInstructions().
var goodParamSignatures = []string{
	"find(query, path_glob)",   // SearchInput: query + path_glob (no "path")
	"read(path)",               // read takes path, not "file"
	"trace(symbol, direction)", // already correct — pin it so it stays
}

// staleParamSignatures are the pre-#525 param drifts: prose that named params
// the schema never had, so agents' first call failed. They MUST NOT reappear.
// The trailing ')' keeps "find(query, path)" from matching "find(query, path_glob)".
var staleParamSignatures = []string{
	"find(query, path)",
	"impact(symbol)",
	"read(file)",
}

// TestServerInstructionsParamNamesMatchSchema guards #525: the param names in
// the ServerInstructions() tool table must match the registered input schema.
func TestServerInstructionsParamNamesMatchSchema(t *testing.T) {
	instr := ServerInstructions()
	for _, good := range goodParamSignatures {
		if !strings.Contains(instr, good) {
			t.Errorf("ServerInstructions() missing expected signature %q", good)
		}
	}
	for _, stale := range staleParamSignatures {
		if strings.Contains(instr, stale) {
			t.Errorf("ServerInstructions() still shows stale param signature %q (drifted from schema)", stale)
		}
	}
}

// TestServerInstructionsMatchRegistry guards ServerInstructions() against
// tool-name drift: every name it mentions must be advertised by the server,
// and no removed name may linger.
func TestServerInstructionsMatchRegistry(t *testing.T) {
	instr := ServerInstructions()

	for _, dead := range deadToolNames {
		if strings.Contains(instr, dead) {
			t.Errorf("ServerInstructions still references removed tool name %q", dead)
		}
	}

	// Full surface: embedder gates find; chat gates read. Wire both so the
	// whole instruction set is checkable in one pass.
	srv := stubServer(t)
	srv.EmbedClient = embed.New("http://127.0.0.1:0", "fake", 16, 200*time.Millisecond)
	srv.ChatClient = chat.New("http://127.0.0.1:0", "fake", 200*time.Millisecond)
	registered := listToolNames(t, srv)

	for _, name := range instructionTools {
		if !strings.Contains(instr, name) {
			t.Errorf("instructionTools lists %q but ServerInstructions() does not mention it", name)
		}
		if !registered[name] {
			t.Errorf("ServerInstructions() steers agents to %q but it is not a registered tool", name)
		}
	}
}
