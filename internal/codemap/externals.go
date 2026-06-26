package codemap

import (
	"fmt"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// DefaultExternalsBudget caps the "external dependencies" orientation section
// (#581) so it stays a glanceable line or two on any repo.
const DefaultExternalsBudget = 160

// capability is one coarse functional bucket for an external dependency.
type capability struct {
	name     string
	keywords []string // case-insensitive substrings of the import path
}

// capabilities is the ordered classification table. Order is BOTH the display
// order and the match priority: the first capability whose keyword appears in
// an import path claims it. Deliberately small and high-signal — an unmatched
// import (fmt, strings, errors, …) is dropped as noise, so the section answers
// "what does this repo TALK TO?" not "list every import".
var capabilities = []capability{
	{"database", []string{"sqlite", "postgres", "pgx", "mysql", "redis", "mongo", "badger", "bbolt", "/bolt", "etcd", "dynamodb", "gorm", "database/sql"}},
	{"network", []string{"net/http", "grpc", "websocket", "/gin", "/echo", "fiber", "fasthttp", "gorilla", "graphql", "golang.org/x/net"}},
	{"gpu/ml", []string{"onnx", "tokenizer", "cuda", "tensor", "ggml", "llama", "openai", "gguf", "huggingface"}},
	{"serialization", []string{"yaml", "toml", "protobuf", "msgpack", "cbor", "encoding/json", "encoding/xml"}},
	{"crypto", []string{"crypto", "jwt", "/tls", "bcrypt", "nacl"}},
	{"process", []string{"os/exec", "syscall", "os/signal"}},
	{"cloud", []string{"aws-sdk", "cloud.google", "azure", "kubernetes", "k8s.io"}},
}

// capBucket is a capability and the external packages classified into it.
type capBucket struct {
	Name string
	Pkgs []string
}

// classifyExternals buckets external import paths by capability, dropping any
// that match none. Buckets are returned in display order; each bucket's Pkgs
// are the deduped, sorted short names — fully deterministic for a cache-stable
// orientation render.
func classifyExternals(imports []string) []capBucket {
	seen := make([]map[string]bool, len(capabilities))
	pkgs := make([][]string, len(capabilities))
	for i := range capabilities {
		seen[i] = map[string]bool{}
	}
	for _, imp := range imports {
		lower := strings.ToLower(imp)
		for i, c := range capabilities {
			if matchesAny(lower, c.keywords) {
				if name := shortPkg(imp); name != "" && !seen[i][name] {
					seen[i][name] = true
					pkgs[i] = append(pkgs[i], name)
				}
				break // first matching capability wins
			}
		}
	}
	out := make([]capBucket, 0, len(capabilities))
	for i, c := range capabilities {
		if len(pkgs[i]) == 0 {
			continue
		}
		sort.Strings(pkgs[i])
		out = append(out, capBucket{Name: c.name, Pkgs: pkgs[i]})
	}
	return out
}

func matchesAny(s string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// shortPkg is the display name for an import path: its last path segment
// ("github.com/mattn/go-sqlite3" → "go-sqlite3", "encoding/json" → "json",
// "gopkg.in/yaml.v3" → "yaml.v3").
func shortPkg(path string) string {
	path = strings.TrimRight(path, "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

const externalsHeader = "## external dependencies\n"

// maxPkgsPerCapability caps how many example packages a single capability line
// names before it summarises the rest as "(+N more)".
const maxPkgsPerCapability = 6

// RenderExternals renders the "external dependencies by capability" section
// (#581): one line per capability the repo touches, with example packages,
// greedily fit to budget. Returns "" when no import classifies — the section is
// then omitted entirely, leaving callers byte-identical to the pre-#581 bundle.
func RenderExternals(imports []string, budget int) string {
	if budget <= 0 {
		budget = DefaultExternalsBudget
	}
	buckets := classifyExternals(imports)
	if len(buckets) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(externalsHeader)
	for _, bk := range buckets {
		line := externalsLine(bk)
		if b.Len() > len(externalsHeader) && tokens.Count(b.String()+line) > budget {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

// externalsLine renders one capability row, capping the named packages.
func externalsLine(bk capBucket) string {
	shown := bk.Pkgs
	extra := 0
	if len(shown) > maxPkgsPerCapability {
		extra = len(shown) - maxPkgsPerCapability
		shown = shown[:maxPkgsPerCapability]
	}
	line := fmt.Sprintf("- %s: %s", bk.Name, strings.Join(shown, ", "))
	if extra > 0 {
		line += fmt.Sprintf(" (+%d more)", extra)
	}
	return line + "\n"
}
