package summarize

import (
	"strings"
	"testing"
)

func TestDetectSemanticChunks_basic(t *testing.T) {
	src := `import "fmt"

type Foo struct {
	X int
}

func Bar() {
	fmt.Println("hi")
}
`
	chunks := detectSemanticChunks(src)
	kinds := make(map[chunkKind]int)
	for _, c := range chunks {
		kinds[c.kind]++
	}
	if kinds[chunkKindImports] == 0 {
		t.Error("expected at least one Imports chunk")
	}
	if kinds[chunkKindTypeDef] == 0 {
		t.Error("expected at least one TypeDef chunk")
	}
	if kinds[chunkKindFuncDef] == 0 {
		t.Error("expected at least one FuncDef chunk")
	}
}

func TestDetectSemanticChunks_identifiers(t *testing.T) {
	src := `func (s *Server) HandleAuth(w http.ResponseWriter) {
	s.doAuth(w)
}

type AuthResult struct {
	Token string
}
`
	chunks := detectSemanticChunks(src)
	var funcIdent, typeIdent string
	for _, c := range chunks {
		switch c.kind {
		case chunkKindFuncDef:
			funcIdent = c.identifier
		case chunkKindTypeDef:
			typeIdent = c.identifier
		}
	}
	if funcIdent != "HandleAuth" {
		t.Errorf("func identifier = %q, want HandleAuth", funcIdent)
	}
	if typeIdent != "AuthResult" {
		t.Errorf("type identifier = %q, want AuthResult", typeIdent)
	}
}

func TestScoreChunks_keywordBoost(t *testing.T) {
	src := `func Authenticate(token string) bool {
	return token == "secret"
}

func Ping() {
	fmt.Println("pong")
}
`
	chunks := detectSemanticChunks(src)
	scoreChunks(chunks, []string{"authenticate", "token"})

	var authScore, pingScore float64
	for _, c := range chunks {
		if c.identifier == "Authenticate" {
			authScore = c.relevance
		}
		if c.identifier == "Ping" {
			pingScore = c.relevance
		}
	}
	if authScore <= pingScore {
		t.Errorf("Authenticate (%.2f) should score higher than Ping (%.2f) with auth/token keywords", authScore, pingScore)
	}
}

func TestApplySemanticChunkOrder_fallbackOnFewChunks(t *testing.T) {
	// Single-function file: ≤3 chunks → no reordering, original returned.
	src := `func Foo() {
	return
}
`
	got := SemanticChunkOrder(src, "foo bar baz")
	if got != src {
		t.Errorf("expected passthrough for short file, got reordered output")
	}
}

func TestApplySemanticChunkOrder_fallbackOnEmptyTask(t *testing.T) {
	src := `import "fmt"
import "os"

type Foo struct{ X int }

func Bar() { fmt.Println("hi") }

func Baz() { os.Exit(0) }

func Qux() {}
`
	got := SemanticChunkOrder(src, "")
	if got != src {
		t.Errorf("expected passthrough when task is empty")
	}
}

func TestApplySemanticChunkOrder_reordersWithTask(t *testing.T) {
	src := `import "fmt"

type Config struct {
	Host string
}

func Unrelated() {
	fmt.Println("nothing")
}

func LoadConfig(path string) Config {
	return Config{Host: path}
}

func Helper() {}

func AnotherHelper() {}
`
	got := SemanticChunkOrder(src, "load config from path")
	// LoadConfig should appear before Unrelated in the output
	loadIdx := strings.Index(got, "LoadConfig")
	unrelatedIdx := strings.Index(got, "Unrelated")
	if loadIdx == -1 || unrelatedIdx == -1 {
		t.Fatal("expected both functions in output")
	}
	if loadIdx >= unrelatedIdx {
		t.Errorf("LoadConfig (pos %d) should appear before Unrelated (pos %d) when task is 'load config'", loadIdx, unrelatedIdx)
	}
}

func TestRenderWithBridges_tailAnchor(t *testing.T) {
	chunks := []semChunk{
		{
			lines:      []string{"func Foo() {", `    fmt.Println("x")`, "}"},
			kind:       chunkKindFuncDef,
			identifier: "Foo",
			relevance:  1.0,
		},
	}
	out := renderWithBridges(chunks)
	if !strings.Contains(out, "attention bridge: Foo") {
		t.Errorf("expected tail anchor for Foo in output: %q", out)
	}
}

func TestExtractTaskKeywords(t *testing.T) {
	kw := extractTaskKeywords("load the auth token from config file")
	kwSet := make(map[string]bool)
	for _, k := range kw {
		kwSet[k] = true
	}
	for _, want := range []string{"load", "auth", "token", "config", "file"} {
		if !kwSet[want] {
			t.Errorf("expected keyword %q in %v", want, kw)
		}
	}
	// stop words should be filtered
	for _, stop := range []string{"the", "from"} {
		if kwSet[stop] {
			t.Errorf("stop word %q should not be in keywords", stop)
		}
	}
}
