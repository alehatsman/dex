package lsprecall

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/eval/trace"
	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// ProbeResult is the recall outcome for one trace.Probe.
type ProbeResult struct {
	Symbol    string   // probe symbol
	Package   string   // probe package
	Direction string   // callers | callees
	LSPCount  int      // what LSP found
	GraphHits int      // how many of those the graph also found
	Missing   []string // LSP results absent from the graph (normalized names)
	Error     string   // non-empty if LSP query failed
}

// Recall returns graph hits / LSP total (1.0 vacuously when LSP found nothing).
func (r ProbeResult) Recall() float64 {
	if r.LSPCount == 0 {
		return 1.0
	}
	return float64(r.GraphHits) / float64(r.LSPCount)
}

// RunProbes evaluates every probe in gold against both the graph (via view) and
// the LSP server spawned at lspCmd. corpusRoot is the absolute corpus checkout
// path; langID is the LSP languageId (e.g. "typescript").
func RunProbes(
	ctx context.Context,
	gold trace.Gold,
	corpusRoot string,
	view *graphquery.View,
	lspCmd []string,
	langID string,
	timeout time.Duration,
) ([]ProbeResult, error) {
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	rootURI := pathToURI(corpusRoot)
	client, err := Spawn(ctx, lspCmd, rootURI)
	if err != nil {
		return nil, fmt.Errorf("lsprecall: spawn LSP: %w", err)
	}
	defer client.Shutdown() //nolint:errcheck

	var results []ProbeResult
	opened := make(map[string]bool) // track didOpen per file — LSP allows open once
	for _, p := range gold.Probes {
		r := runOneProbe(ctx, client, p, corpusRoot, view, langID, timeout, opened)
		results = append(results, r)
	}
	return results, nil
}

func runOneProbe(
	ctx context.Context,
	client *Client,
	p trace.Probe,
	corpusRoot string,
	view *graphquery.View,
	langID string,
	timeout time.Duration,
	opened map[string]bool,
) ProbeResult {
	res := ProbeResult{Symbol: p.Symbol, Package: p.Package, Direction: p.Direction}

	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 1. Find the definition file and line for this symbol.
	defFile, defLine, defChar, err := findDefinition(corpusRoot, p.Package, p.Symbol, langID)
	if err != nil {
		res.Error = fmt.Sprintf("definition not found: %v", err)
		return res
	}

	// 2. Open the file in the LSP server (once per file — protocol forbids re-opening).
	fileURI := pathToURI(defFile)
	if !opened[fileURI] {
		text, err := os.ReadFile(defFile)
		if err != nil {
			res.Error = fmt.Sprintf("read file: %v", err)
			return res
		}
		if err := client.DidOpen(pctx, fileURI, langID, string(text)); err != nil {
			res.Error = fmt.Sprintf("didOpen: %v", err)
			return res
		}
		opened[fileURI] = true
	}

	// 3. Prepare call hierarchy at the definition.
	items, err := client.PrepareCallHierarchy(pctx, fileURI, defLine, defChar)
	if err != nil || len(items) == 0 {
		if err != nil {
			res.Error = fmt.Sprintf("prepareCallHierarchy: %v", err)
		} else {
			res.Error = "prepareCallHierarchy: no items returned"
		}
		return res
	}
	item := items[0]

	// 4. Query callers or callees from LSP.
	// Only keep intra-corpus results — stdlib/node_modules calls are out of
	// scope (same as trace.Gold notes: "Scope = INTRA-REPO call targets only").
	callers := p.Direction == trace.DirectionCallers
	var lspNames []string
	if callers {
		calls, err := client.IncomingCalls(pctx, item)
		if err != nil {
			res.Error = fmt.Sprintf("incomingCalls: %v", err)
			return res
		}
		for _, c := range calls {
			if name, ok := normalizeLSPName(c.From.Name, c.From.URI, corpusRoot); ok {
				lspNames = append(lspNames, name)
			}
		}
	} else {
		calls, err := client.OutgoingCalls(pctx, item)
		if err != nil {
			res.Error = fmt.Sprintf("outgoingCalls: %v", err)
			return res
		}
		for _, c := range calls {
			if name, ok := normalizeLSPName(c.To.Name, c.To.URI, corpusRoot); ok {
				lspNames = append(lspNames, name)
			}
		}
	}

	// 5. Get graph peers for the same symbol and build a lookup set.
	targets := graphquery.ResolveCallTargets(view, p.Symbol, p.Package)
	graphPeers := graphPeerKeys(view, targets, callers)
	graphSet := make(map[string]bool, len(graphPeers))
	for _, k := range graphPeers {
		// Index both full peerKey ("util.extend") and bare name ("extend").
		graphSet[k] = true
		if i := strings.LastIndex(k, "."); i >= 0 {
			graphSet[k[i+1:]] = true
		}
	}

	// 6. Score LSP results against the graph set.
	res.LSPCount = len(lspNames)
	seen := map[string]bool{}
	for _, name := range lspNames {
		if seen[name] {
			continue
		}
		seen[name] = true
		baseName := name
		if i := strings.LastIndex(name, "."); i >= 0 {
			baseName = name[i+1:]
		}
		if graphSet[name] || graphSet[baseName] {
			res.GraphHits++
		} else {
			res.Missing = append(res.Missing, name)
		}
	}
	return res
}

// graphPeerKeys returns the peerKey strings for the callers (callers=true) or
// callees (callers=false) of the given target nodes, replicating the logic from
// internal/eval/trace.peers().
func graphPeerKeys(view *graphquery.View, targets []graphquery.Node, callers bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range targets {
		var edges []graphquery.Edge
		if callers {
			edges = view.EdgesByDst[t.ID]
		} else {
			edges = view.EdgesBySrc[t.ID]
		}
		for _, e := range edges {
			if e.Kind != graph.EdgeCalls {
				continue
			}
			peerID := e.DstID
			if callers {
				peerID = e.SrcID
			}
			n, ok := view.NodesByID[peerID]
			if !ok {
				continue
			}
			k := nodePeerKey(n)
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// nodePeerKey mirrors trace.peerKey: "pkgTail.QualifiedName".
func nodePeerKey(n graphquery.Node) string {
	name := n.QualifiedName
	if name == "" {
		name = n.Name
	}
	if n.PackagePath == "" {
		return name
	}
	seg := n.PackagePath
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	return seg + "." + name
}

// findDefinition locates the file and 0-based line/char of the symbol's
// definition within the package directory, using language-specific patterns.
func findDefinition(corpusRoot, pkg, symbol, langID string) (file string, line, char int, err error) {
	pkgPath := filepath.Join(corpusRoot, filepath.FromSlash(packageToPath(pkg, langID)))
	// For TS/JS: the "package path" often refers to a .ts file, not a dir.
	// Resolve pkgPath to the actual directory to walk.
	pkgDir := resolvePkgDir(pkgPath, langID)
	bare := bareSymbol(symbol)

	var exts []string
	var patterns []*regexp.Regexp
	switch langID {
	case "typescript", "javascript":
		exts = []string{".ts", ".tsx", ".js", ".jsx"}
		patterns = []*regexp.Regexp{
			regexp.MustCompile(`(?:export\s+(?:default\s+)?(?:async\s+)?function\s+` + regexp.QuoteMeta(bare) + `\b)`),
			regexp.MustCompile(`(?:export\s+)?(?:const|let|var)\s+` + regexp.QuoteMeta(bare) + `\s*[=:]`),
			regexp.MustCompile(`\b` + regexp.QuoteMeta(bare) + `\s*[=:]\s*(?:async\s+)?function\b`),
		}
	case "java":
		exts = []string{".java"}
		// Strip overload signature: "method(int, String)" → "method"
		if i := strings.Index(bare, "("); i >= 0 {
			bare = bare[:i]
		}
		patterns = []*regexp.Regexp{
			regexp.MustCompile(`(?:public|private|protected|static|\s)+[\w<>\[\]]+\s+` + regexp.QuoteMeta(bare) + `\s*\(`),
		}
	default:
		return "", 0, 0, fmt.Errorf("unsupported language %q", langID)
	}

	return searchFiles(pkgDir, exts, patterns)
}

// searchFiles walks dir for the first match of any pattern in a file with an
// accepted extension. Returns the file path and 0-based line + character,
// where character points to the start of bare within the matched text so that
// an LSP cursor lands on the symbol name (not the `export`/`function` prefix).
func searchFiles(dir string, exts []string, patterns []*regexp.Regexp) (string, int, int, error) {
	var found string
	var foundLine, foundChar int
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return err
		}
		matched := false
		ext := filepath.Ext(path)
		for _, e := range exts {
			if ext == e {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil //nolint:nilerr
		}
		defer f.Close() //nolint:errcheck
		scanner := bufio.NewScanner(f)
		lineN := 0
		for scanner.Scan() {
			text := scanner.Text()
			for _, pat := range patterns {
				if loc := pat.FindStringIndex(text); loc != nil {
					found = path
					foundLine = lineN
					// Position the cursor at the last word boundary in the
					// match (the symbol name itself, after keywords like
					// "export function"). Find the last identifier start.
					matchText := text[loc[0]:loc[1]]
					wordStart := loc[0] + lastIdentStart(matchText)
					foundChar = wordStart
					return filepath.SkipAll
				}
			}
			lineN++
		}
		return nil
	})
	if err != nil {
		return "", 0, 0, err
	}
	if found == "" {
		return "", 0, 0, fmt.Errorf("definition not found under %s", dir)
	}
	return found, foundLine, foundChar, nil
}

// lastIdentStart returns the byte offset of the last word in s that starts
// with a letter or underscore — the symbol name within a "export function Foo"
// pattern match.
func lastIdentStart(s string) int {
	start := -1
	inIdent := false
	for i, ch := range s {
		isStart := ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
		isRest := isStart || (ch >= '0' && ch <= '9')
		if isStart && !inIdent {
			start = i
			inIdent = true
		} else if !isRest {
			inIdent = false
		}
	}
	if start < 0 {
		return 0
	}
	return start
}

// resolvePkgDir converts a raw pkgPath (which may point to a file without
// extension for TS/JS packages like "packages/zod/src/v4/core/util") to the
// directory that should be walked for the symbol search.
func resolvePkgDir(pkgPath, langID string) string {
	info, err := os.Stat(pkgPath)
	if err == nil && info.IsDir() {
		return pkgPath
	}
	// Not a directory — try with common source extensions to see if it is a file.
	var exts []string
	switch langID {
	case "typescript", "javascript":
		exts = []string{".ts", ".tsx", ".js", ".jsx"}
	case "java":
		// For Java, dots in package already converted to slashes; path IS a dir.
		return pkgPath
	}
	for _, ext := range exts {
		if _, serr := os.Stat(pkgPath + ext); serr == nil {
			// pkgPath is actually a file basename — return its parent directory
			// but remember the basename so searchFiles stays within that file.
			// Simplest: return the directory; the pattern will still match.
			return filepath.Dir(pkgPath)
		}
	}
	// Fall back: return as-is (WalkDir will error and surface it clearly).
	return pkgPath
}

// packageToPath converts a package identifier to a relative directory path.
func packageToPath(pkg, langID string) string {
	if langID == "java" {
		// "com.google.common.base" → "com/google/common/base"
		return strings.ReplaceAll(pkg, ".", string(filepath.Separator))
	}
	return pkg // TS/JS paths already use slashes
}

// bareSymbol strips receiver qualification: "(*T).M" → "M", "pkg.Name" → "Name".
func bareSymbol(symbol string) string {
	if idx := strings.Index(symbol, ")."); idx >= 0 {
		return symbol[idx+2:]
	}
	if idx := strings.LastIndex(symbol, "."); idx >= 0 {
		return symbol[idx+1:]
	}
	return symbol
}

// normalizeLSPName converts an LSP item name + file URI to a peerKey-like form
// "basename.name". Returns (name, false) for out-of-corpus files (stdlib,
// node_modules) so callers can skip them — matching the trace.Gold scope rule.
func normalizeLSPName(name, uri, corpusRoot string) (string, bool) {
	filePath := uriToPath(uri)
	if filePath == "" {
		return name, false
	}
	rel, err := filepath.Rel(corpusRoot, filePath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false // outside corpus root
	}
	// Also exclude TypeScript stdlib files (lib.*.d.ts) and node_modules.
	if strings.Contains(rel, "node_modules") ||
		strings.HasPrefix(filepath.Base(rel), "lib.") {
		return "", false
	}
	base := filepath.Base(rel)
	if i := strings.LastIndex(base, "."); i >= 0 {
		base = base[:i]
	}
	// Unwrap index files: "util/index" → "util"
	if base == "index" {
		base = filepath.Base(filepath.Dir(rel))
	}
	return base + "." + name, true
}

func pathToURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String()
}

func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return filepath.FromSlash(u.Path)
}
