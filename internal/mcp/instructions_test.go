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
	"ask", "find", "lookup", "shell", "read", "callers", "callees", "deps",
}

// deadToolNames are pre-rename names that must never reappear in the
// instructions. They were the bug: ServerInstructions advertised them
// while the registry used the short names above.
var deadToolNames = []string{
	"search_semantic", "search_symbol", "ctx_shell", "file_view",
	"graph_callers", "graph_callees", "graph_deps", "graph_neighbors",
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
