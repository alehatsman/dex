package codemap

import (
	"strings"
	"testing"
)

func TestClassifyExternals(t *testing.T) {
	imports := []string{
		"github.com/mattn/go-sqlite3",
		"database/sql",
		"net/http",
		"google.golang.org/grpc",
		"gopkg.in/yaml.v3",
		"encoding/json",
		"github.com/yalue/onnxruntime_go",
		"os/exec",
		"crypto/sha256",
		// noise — classifies to nothing:
		"fmt", "strings", "errors", "context",
	}
	buckets := classifyExternals(imports)

	got := map[string][]string{}
	for _, b := range buckets {
		got[b.Name] = b.Pkgs
	}
	// Database bucket holds both the driver and database/sql, deduped+sorted.
	if db := got["database"]; len(db) != 2 || db[0] != "go-sqlite3" || db[1] != "sql" {
		t.Errorf("database bucket = %v, want [go-sqlite3 sql]", db)
	}
	if net := got["network"]; len(net) != 2 || net[0] != "grpc" || net[1] != "http" {
		t.Errorf("network bucket = %v, want [grpc http]", net)
	}
	if got["gpu/ml"] == nil || got["gpu/ml"][0] != "onnxruntime_go" {
		t.Errorf("gpu/ml bucket = %v, want onnxruntime_go", got["gpu/ml"])
	}
	if ser := got["serialization"]; len(ser) != 2 || ser[0] != "json" || ser[1] != "yaml.v3" {
		t.Errorf("serialization bucket = %v, want [json yaml.v3]", ser)
	}
	if got["process"] == nil || got["process"][0] != "exec" {
		t.Errorf("process bucket = %v, want exec", got["process"])
	}
	if got["crypto"] == nil {
		t.Error("expected a crypto bucket for crypto/sha256")
	}
	// Pure noise produces no buckets.
	if pure := classifyExternals([]string{"fmt", "strings", "errors"}); len(pure) != 0 {
		t.Errorf("noise-only imports should classify to nothing, got %v", pure)
	}
}

func TestRenderExternals(t *testing.T) {
	out := RenderExternals([]string{
		"github.com/mattn/go-sqlite3", "net/http", "gopkg.in/yaml.v3",
	}, 0)
	if !strings.Contains(out, "external dependencies") {
		t.Errorf("missing header:\n%s", out)
	}
	for _, want := range []string{"database: go-sqlite3", "network: http", "serialization: yaml.v3"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// No classifiable imports → empty (section omitted).
	if RenderExternals([]string{"fmt", "strings"}, 0) != "" {
		t.Error("noise-only imports should render no section")
	}
	if RenderExternals(nil, 0) != "" {
		t.Error("nil imports should render no section")
	}
	// Deterministic.
	a := RenderExternals([]string{"net/http", "gopkg.in/yaml.v3"}, 0)
	b := RenderExternals([]string{"net/http", "gopkg.in/yaml.v3"}, 0)
	if a != b {
		t.Error("RenderExternals must be deterministic")
	}
}

func TestRenderExternalsCapsPackages(t *testing.T) {
	many := []string{"sqlite", "postgres", "mysql", "redis", "mongo", "badger", "etcd", "dynamodb"}
	out := RenderExternals(many, 0)
	if !strings.Contains(out, "more)") {
		t.Errorf("expected a (+N more) cap on a large bucket:\n%s", out)
	}
}

// TestRenderOrientAppendsExternals verifies the section is appended to the
// bundle when externals exist, and omitted (byte-identical) when they don't.
func TestRenderOrientAppendsExternals(t *testing.T) {
	cs := []Cluster{{ID: 1, Size: 2, Symbols: []Symbol{
		{QualifiedName: "Run", Kind: "function", Pkg: "main", Path: "main.go", Line: 1, PageRank: 0.9},
	}}}
	withExt := RenderOrient(cs, []string{"github.com/mattn/go-sqlite3"}, 1000, 1000)
	without := RenderOrient(cs, nil, 1000, 1000)
	if !strings.Contains(withExt, "external dependencies") {
		t.Errorf("externals section missing:\n%s", withExt)
	}
	if strings.Contains(without, "external dependencies") {
		t.Errorf("nil externals must omit the section:\n%s", without)
	}
	// The bundle without externals is exactly the L0+L1 prefix of the one with.
	if !strings.HasPrefix(withExt, without) {
		t.Error("externals section must be APPENDED, not interleaved")
	}
}
