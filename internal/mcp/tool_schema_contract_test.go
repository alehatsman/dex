package mcp

// Golden contract for the compatibility-significant subset of every advertised
// MCP tool's input schema (#93). The SDK generates each schema from a Go struct
// and advertises it with additionalProperties:false, so an accidental field
// removal (e.g. the ShellInput.Description regression, #88) is a hard runtime
// break for agents. This test pins tool names, property names, requiredness,
// property types, enum values, and additionalProperties across the default,
// expert, and lean (no-embedder) registration paths — but NOT prose
// descriptions, so copy edits don't churn the golden.
//
// A schema change is allowed but must be explicit in review:
//   - additive optional property → update the golden intentionally
//   - removed/renamed property, optional→required, enum narrowing, or an
//     additionalProperties change → treat as breaking unless compatibility is
//     preserved
//
// To intentionally update after a reviewed schema change:
//   go test ./internal/mcp -run TestToolSchemaContract -update-tool-contract

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
)

var updateToolContract = flag.Bool("update-tool-contract", false, "rewrite the MCP tool-schema contract golden")

const toolContractGolden = "testdata/tool_schema_contract.json"

type propContract struct {
	Type string   `json:"type,omitempty"`
	Enum []string `json:"enum,omitempty"`
}

type toolContract struct {
	Name                 string                  `json:"name"`
	AdditionalProperties *bool                   `json:"additionalProperties,omitempty"`
	Required             []string                `json:"required,omitempty"`
	Properties           map[string]propContract `json:"properties,omitempty"`
}

func TestToolSchemaContract(t *testing.T) {
	profiles := []struct {
		name   string
		expert string
		full   bool // full = embedder + chat wired; lean = neither
	}{
		{"default", "", true},
		{"expert", "1", true},
		{"lean", "", false},
	}

	got := map[string][]toolContract{}
	for _, p := range profiles {
		t.Setenv("DEX_EXPERT", p.expert)
		srv := stubServer(t)
		if p.full {
			// Fake clients toggle chatAvailable/embedAvailable at registration;
			// ListTools never calls them, so an unused address is fine.
			srv.EmbedClient = embed.New("http://127.0.0.1:0", "fake", 16, 200*time.Millisecond)
			srv.ChatClient = chat.New("http://127.0.0.1:0", "fake", 200*time.Millisecond)
		}
		got[p.name] = collectToolContracts(t, srv)
	}

	data, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')

	if *updateToolContract {
		if err := os.WriteFile(toolContractGolden, data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", toolContractGolden)
		return
	}

	want, err := os.ReadFile(toolContractGolden)
	if err != nil {
		t.Fatalf("read golden (create it with: go test ./internal/mcp -run TestToolSchemaContract -update-tool-contract): %v", err)
	}
	if !bytes.Equal(want, data) {
		reportToolContractDrift(t, want, data)
	}
}

func collectToolContracts(t *testing.T, srv *Server) []toolContract {
	t.Helper()
	tools := listToolSchemas(t, srv)
	out := make([]toolContract, 0, len(tools))
	for _, tool := range tools {
		out = append(out, normalizeToolSchema(t, tool))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// normalizeToolSchema reduces an advertised tool to the compatibility-significant
// subset of its input schema.
func normalizeToolSchema(t *testing.T, tool *sdk.Tool) toolContract {
	t.Helper()
	var schema struct {
		Properties map[string]struct {
			Type json.RawMessage `json:"type"` // string, or an array like ["null","array"]
			Enum []any           `json:"enum"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties *bool    `json:"additionalProperties"`
	}
	raw, _ := json.Marshal(tool.InputSchema)
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("normalize %s: %v (raw=%s)", tool.Name, err, raw)
	}

	var props map[string]propContract
	if len(schema.Properties) > 0 {
		props = make(map[string]propContract, len(schema.Properties))
		for name, p := range schema.Properties {
			pc := propContract{Type: normalizeSchemaType(p.Type)}
			for _, e := range p.Enum {
				pc.Enum = append(pc.Enum, fmt.Sprint(e))
			}
			sort.Strings(pc.Enum)
			props[name] = pc
		}
	}

	var req []string
	if len(schema.Required) > 0 {
		req = append(req, schema.Required...)
		sort.Strings(req)
	}

	return toolContract{
		Name:                 tool.Name,
		AdditionalProperties: schema.AdditionalProperties,
		Required:             req,
		Properties:           props,
	}
}

// listToolSchemas registers the tool surface for srv and returns the advertised
// tools (name + input schema) via a real ListTools round-trip — the same path
// as listToolNames, but keeping the full schema.
func listToolSchemas(t *testing.T, srv *Server) []*sdk.Tool {
	t.Helper()
	ctx := context.Background()
	id, _, projects := oneProjectRegistry(t)
	ts := startTestHTTPServer(t, srv, RunHTTPOptions{Projects: projects})
	cs := mcpConnect(t, ctx, ts.URL, "/v1/projects/"+id+"/mcp", ts.Client())
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	return tools.Tools
}

// reportToolContractDrift emits a targeted, reviewable summary of what changed
// so a failure is actionable without eyeballing the whole golden.
func reportToolContractDrift(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g map[string][]toolContract
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("golden is not valid JSON: %v", err)
	}
	_ = json.Unmarshal(got, &g)

	for profile, gc := range g {
		wm := indexContracts(w[profile])
		gm := indexContracts(gc)
		for name, got := range gm {
			prev, ok := wm[name]
			if !ok {
				t.Errorf("[%s] tool %q ADDED — update the golden if intentional", profile, name)
				continue
			}
			diffToolContract(t, profile, name, prev, got)
		}
		for name := range wm {
			if _, ok := gm[name]; !ok {
				t.Errorf("[%s] tool %q REMOVED — breaking unless renamed with compatibility preserved", profile, name)
			}
		}
	}
	t.Errorf("MCP tool-schema contract drift. If the change is intentional and reviewed, re-run:\n"+
		"  go test ./internal/mcp -run TestToolSchemaContract -update-tool-contract")
}

func diffToolContract(t *testing.T, profile, name string, prev, got toolContract) {
	t.Helper()
	if !boolEq(prev.AdditionalProperties, got.AdditionalProperties) {
		t.Errorf("[%s] %s: additionalProperties %v → %v (review required)", profile, name,
			derefBool(prev.AdditionalProperties), derefBool(got.AdditionalProperties))
	}
	for _, s := range setDiff(got.Required, prev.Required) {
		t.Errorf("[%s] %s: property %q became required (optional→required is breaking)", profile, name, s)
	}
	for _, s := range setDiff(prev.Required, got.Required) {
		t.Errorf("[%s] %s: property %q no longer required", profile, name, s)
	}
	for p, gp := range got.Properties {
		pp, ok := prev.Properties[p]
		if !ok {
			t.Errorf("[%s] %s: property %q ADDED (optional additive — update golden if intentional)", profile, name, p)
			continue
		}
		if pp.Type != gp.Type {
			t.Errorf("[%s] %s: property %q type %q → %q (breaking)", profile, name, p, pp.Type, gp.Type)
		}
		if strings.Join(pp.Enum, ",") != strings.Join(gp.Enum, ",") {
			t.Errorf("[%s] %s: property %q enum %v → %v (narrowing is breaking)", profile, name, p, pp.Enum, gp.Enum)
		}
	}
	for p := range prev.Properties {
		if _, ok := got.Properties[p]; !ok {
			t.Errorf("[%s] %s: property %q REMOVED (breaking under additionalProperties:false)", profile, name, p)
		}
	}
}

// normalizeSchemaType renders a JSON Schema "type" (a string, or an array like
// ["null","array"] for optional collections) into a deterministic string.
func normalizeSchemaType(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		sort.Strings(arr)
		return strings.Join(arr, "|")
	}
	return string(raw)
}

func indexContracts(cs []toolContract) map[string]toolContract {
	m := make(map[string]toolContract, len(cs))
	for _, c := range cs {
		m[c.Name] = c
	}
	return m
}

// setDiff returns elements in a not in b.
func setDiff(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func boolEq(a, b *bool) bool  { return derefBool(a) == derefBool(b) }
func derefBool(b *bool) bool  { return b != nil && *b }
