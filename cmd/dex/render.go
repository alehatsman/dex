// Output rendering for the CLI subcommands. Splitting these out keeps
// main.go focused on dispatch + env wiring, and makes it obvious which
// pieces are "presentation only" vs "real work".
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/health"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/output"
	"github.com/alehatsman/dex/internal/store"
)

// collectEndpoints builds the probe list the status and doctor commands
// consume. This is the injection seam for internal/health: it does the env
// wiring (embed/chat always present via defaults; rerank opt-in) and hands
// health.CheckEndpoints / CheckEndpointsDeep a ready list of probes with their
// liveness + deep-capability closures bound.
func collectEndpoints() []health.Probe {
	probes := []health.Probe{}

	// A nil embedder is the lean profile (DEX_EMBED_ENGINE=none / bm25-only):
	// no embedding backend is wired, so probe nothing rather than dereferencing
	// it (#545). Mirrors the opt-in rerank branch below.
	if em := newEmbedClient(""); em != nil {
		probes = append(probes, health.Probe{Name: "embed", URL: em.Endpoint(), Model: em.ModelName(), Health: em.Health,
			Deep: func(ctx context.Context) error {
				vecs, err := em.Embed(ctx, []string{health.DeepProbeText})
				if err != nil {
					return err
				}
				if len(vecs) != 1 || len(vecs[0]) == 0 {
					return fmt.Errorf("returned %d vectors (want 1 non-empty)", len(vecs))
				}
				return nil
			}})
	} else {
		probes = append(probes, health.Probe{Name: "embed", Status: "not configured"})
	}

	// Chat is opt-in like rerank: probe it only when a chat model was actually
	// wired. A bare DEX_CHAT_URL pointing at an embed-only ollama would otherwise
	// probe a fabricated default model and report DEGRADED for a capability the
	// user never configured (#133). Mirrors the embed/rerank "not configured" arm.
	if cc, ok := newChatClientConfigured(); ok {
		probes = append(probes, health.Probe{Name: "chat", URL: cc.BaseURL, Model: cc.Model, Health: cc.Health,
			Deep: func(ctx context.Context) error {
				_, err := cc.Generate(ctx, []chat.Message{{Role: "user", Content: health.DeepProbeText}}, chat.Options{MaxTokens: 1})
				return err
			}})
	} else {
		probes = append(probes, health.Probe{Name: "chat", Status: "not configured"})
	}

	if rc := newRerankClient(); rc != nil {
		probes = append(probes, health.Probe{Name: "rerank", URL: rc.Endpoint(), Model: rc.ModelName(), Health: rc.Health,
			Deep: func(ctx context.Context) error {
				scores, err := rc.Rerank(ctx, health.DeepProbeText, []string{"a candidate document"})
				if err != nil {
					return err
				}
				if len(scores) == 0 {
					return fmt.Errorf("returned no scores for a 1-document rerank")
				}
				return nil
			}})
	} else {
		probes = append(probes, health.Probe{Name: "rerank", Status: "not configured"})
	}

	return probes
}

// printEndpoints fans out concurrent health checks for every probe with
// a configured URL, then renders an aligned table under a section
// header.
//
// Column order is (NAME, STATUS, MODEL, URL) — status sits next to
// the name so a quick glance scans down a single column to spot any
// failures, instead of having to skip past two wide columns first.
func printEndpoints(ctx context.Context) {
	probes := collectEndpoints()

	var wg sync.WaitGroup
	for i := range probes {
		if probes[i].Health == nil {
			continue
		}
		wg.Add(1)
		go func(p *health.Probe) {
			defer wg.Done()
			err := p.Health(ctx)
			switch {
			case err == nil:
				p.Status = "ok"
			case errors.Is(err, chat.ErrModelNotFound):
				p.Status = fmt.Sprintf("DEGRADED model not found: %s", p.Model)
			default:
				p.Status = "UNREACHABLE"
			}
		}(&probes[i])
	}
	wg.Wait()

	// Count reachable for the section heading so users can spot a
	// degraded backend without reading the full table.
	reachable := 0
	for _, p := range probes {
		if p.Status == "ok" {
			reachable++
		}
	}

	// Column widths derived from the data PLUS the literal header
	// labels so the heading row aligns under the data even when the
	// widest data cell is narrower than the label.
	headers := struct{ name, status, model, url string }{"NAME", "STATUS", "MODEL", "URL"}
	nameW := len(headers.name)
	statusW := len(headers.status)
	modelW := len(headers.model)
	urlW := len(headers.url)
	for _, p := range probes {
		nameW = max(nameW, len(p.Name))
		statusW = max(statusW, len(p.Status))
		modelW = max(modelW, len(displayCell(p.Model)))
		urlW = max(urlW, len(displayCell(p.URL)))
	}

	fmt.Printf("endpoints (%d reachable)\n", reachable)
	fmt.Printf("  %-*s  %-*s  %-*s  %s\n",
		nameW, headers.name,
		statusW, headers.status,
		modelW, headers.model,
		headers.url)
	for _, p := range probes {
		fmt.Printf("  %-*s  %-*s  %-*s  %s\n",
			nameW, p.Name,
			statusW, p.Status,
			modelW, displayCell(p.Model),
			displayCell(p.URL))
	}

	// When the embed endpoint is unreachable and ollama is running but has no
	// embed models, offer a one-liner to fix it.
	var embedUnreachable bool
	for _, p := range probes {
		if p.Name == "embed" && p.Status == "UNREACHABLE" {
			embedUnreachable = true
			break
		}
	}
	if embedUnreachable {
		if scan, ok := embed.ScanOllama(context.Background()); ok && len(scan.EmbedModels) == 0 {
			fmt.Printf("\nhint: ollama is running but has no embedding model — run:\n")
			fmt.Printf("  ollama pull %s\n", embed.DefaultPullModel)
			fmt.Printf("or: dex reindex --pull-model <path>\n")
		}
	}
}

func displayCell(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// projectStats is the set of fields the project status renderer
// needs. Decoupled from store.Stats so the formatter doesn't depend
// on the internal package layout.
type projectStats struct {
	lastIndex time.Time
	files     int
	chunks    int
	nodes     int64
	edges     int64
	dim       int  // optional — emitted only when > 0
	stale     bool // embed model mismatch — reindex required
}

// printProjectStatLines emits the labelled key:value rows that
// describe one project, all sharing the given indent prefix. Labels
// are padded to a fixed width so values line up vertically inside
// the block. Optional rows (graph, summaries, dim) are skipped when
// there's nothing to show — better than emitting "graph: 0 nodes 0
// edges" which would just be noise.
func printProjectStatLines(indent string, st projectStats) {
	const labelWidth = 8 // "indexed:" is the longest label
	field := func(label, value string) {
		fmt.Printf("%s%-*s %s\n", indent, labelWidth, label+":", value)
	}
	field("indexed", formatProjectAge(st.lastIndex))
	field("files", fmt.Sprintf("%d", st.files))
	field("chunks", fmt.Sprintf("%d", st.chunks))
	if st.nodes > 0 || st.edges > 0 {
		field("graph", fmt.Sprintf("%d nodes  %d edges", st.nodes, st.edges))
	}
	if st.stale {
		field("embed", "changed — reindex required")
	}
	if st.dim > 0 {
		field("dim", fmt.Sprintf("%d", st.dim))
	}
}

// formatProjectAge renders the indexed-time string for one project.
// Zero timestamps return "—". Stale entries (>24h since last index)
// carry an explicit " stale" suffix instead of relying on the ⚠
// symbol — easier to scan, no glyph dependency.
func formatProjectAge(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	rel := relativeTime(t)
	if time.Since(t) > 24*time.Hour {
		return rel + "  stale"
	}
	return rel
}

type queryJSONHit struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	// SortScore is the authoritative key the hits are ordered by — compare
	// this across hits, not Score. Folds rerank/cross-encoder/RRF into one
	// monotonic value; the per-lane fields below are diagnostics.
	SortScore   float32  `json:"sort_score"`
	Score       float32  `json:"score"`
	BM25Score   float32  `json:"bm25_score,omitempty"`
	RRFScore    float32  `json:"rrf_score,omitempty"`
	RerankScore float32  `json:"rerank_score,omitempty"`
	Lanes       []string `json:"lanes,omitempty"` // retrieval lanes that surfaced this hit (#707)
	Content     string   `json:"content"`
}

// findJSONOutput wraps the ranked hits in the shared output envelope (#816).
// Hits stay the rich, find-specific payload (scores, lanes, content); the
// embedded envelope is the uniform machine contract a consumer reads without
// knowing find's bespoke shape — evidence carries the lean spans, confidence
// reports trust, stale flags index age, next_calls suggests the follow-up.
type findJSONOutput struct {
	Hits []queryJSONHit `json:"hits"`
	output.Envelope
}

// buildFindEnvelope derives the envelope from the ranked hits and the index's
// last-indexed time. Evidence mirrors the hit locations as lean spans (no
// content — the hits carry that); next_calls points at reading the top hit.
func buildFindEnvelope(hits []store.Hit, lastIndexed time.Time) output.Envelope {
	ev := make([]output.EvidenceSpan, 0, len(hits))
	for _, h := range hits {
		ev = append(ev, output.EvidenceSpan{
			Path: h.Path, Start: h.StartLine, End: h.EndLine,
			Symbol: h.Name, Kind: output.SpanExact,
		})
	}
	env := output.Envelope{
		Confidence: output.Confidence{
			Level: output.LevelHigh,
			Basis: []string{"hybrid retrieval: semantic + BM25 + symbol fusion"},
		},
		Evidence: ev,
		Stale:    output.AgeStale(lastIndexed),
	}
	if len(hits) > 0 {
		top := hits[0]
		env.NextCalls = []output.NextCall{{
			Tool:   "read",
			Args:   top.Path,
			Reason: "read the top-ranked hit in full",
		}}
		if top.Name != "" {
			env.NextCalls = append(env.NextCalls, output.NextCall{
				Tool:   "trace",
				Args:   top.Name,
				Reason: "follow the call graph around the top symbol",
			})
		}
	}
	env.Normalize()
	return env
}

// readJSONOutput wraps the read payload in the shared envelope (#816). The
// scalar fields are the read-specific payload; the embedded envelope is the
// uniform contract.
type readJSONOutput struct {
	Path        string `json:"path"`
	Mode        string `json:"mode"`
	Start       int    `json:"start,omitempty"`
	End         int    `json:"end,omitempty"`
	TotalLines  int    `json:"total_lines"`
	OutputLines int    `json:"output_lines"`
	Content     string `json:"content"`
	output.Envelope
}

// buildReadEnvelope derives the envelope for a read. Evidence is the single
// span actually returned (the requested range, or the whole file). Lossy modes
// (signatures/aggressive/entropy) drop to medium confidence with a gap naming
// the compression. The local read path serves live working-tree (or --ref)
// bytes and never consults the index, so staleness coverage is unknown and
// is_stale is false — the returned bytes are current, whatever the index age.
func buildReadEnvelope(path, mode string, start, end, totalLines int) output.Envelope {
	spanStart, spanEnd := start, end
	if spanStart <= 0 {
		spanStart = 1
	}
	if spanEnd <= 0 {
		spanEnd = totalLines
	}
	if spanEnd < spanStart {
		spanEnd = spanStart
	}
	conf := output.Confidence{Level: output.LevelHigh, Basis: []string{"exact working-tree bytes"}}
	var next []output.NextCall
	switch mode {
	case "signatures", "aggressive", "entropy":
		conf.Level = output.LevelMedium
		conf.Gaps = []string{"content compressed (mode=" + mode + ") — bodies elided"}
		next = []output.NextCall{{
			Tool:   "read",
			Args:   path + " --mode full",
			Reason: "read the full uncompressed content",
		}}
	}
	env := output.Envelope{
		Confidence: conf,
		Evidence: []output.EvidenceSpan{{
			Path: path, Start: spanStart, End: spanEnd, Kind: output.SpanExact,
		}},
		Stale:     output.StaleStatus{Coverage: output.CoverageUnknown},
		NextCalls: next,
	}
	env.Normalize()
	return env
}

func hitsToJSON(hits []store.Hit) []queryJSONHit {
	out := make([]queryJSONHit, len(hits))
	for i, h := range hits {
		out[i] = queryJSONHit{
			Path:        h.Path,
			Kind:        h.Kind,
			StartLine:   h.StartLine,
			EndLine:     h.EndLine,
			SortScore:   h.DisplayScore(),
			Score:       h.Score,
			BM25Score:   h.BM25Score,
			RRFScore:    h.RRFScore,
			RerankScore: h.RerankScore,
			Lanes:       h.Lanes.Names(),
			Content:     h.Content,
		}
	}
	return out
}

// truncate clips s to n bytes (n=0 means no limit), snapping back to a UTF-8
// boundary so we don't emit a half-rune sequence to the terminal.
// The truncation indicator names the byte limit so users know what to pass
// to --max-content-bytes to see more.
func truncate(s string, n int) string {
	if n == 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n…(truncated at %d bytes; pass --max-content-bytes to override)", n)
}

// relativeTime formats a timestamp as a human-friendly relative string
// ("just now", "5m ago", "2h ago", "3d ago", or a date for old entries).
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// printSearchHitResult renders the shared status/hint/hits shape used
// by the search-style MCP tools (search_semantic, search_symbol,
// graph_neighbors). Single helper keeps the CLI's text output for all
// three surfaces visually identical.
// maxBytes controls content truncation (0 = no limit; 1500 is the default
// that callers pass when no --max-content-bytes flag is set).
func printSearchHitResult(status, hint, project string, hits []mcp.SearchHit, maxBytes int) {
	if status != "" && status != "ok" {
		fmt.Fprintf(os.Stderr, "status: %s\n", status)
		if hint != "" {
			fmt.Fprintf(os.Stderr, "hint:   %s\n", hint)
		}
		return
	}
	if project != "" {
		fmt.Printf("project: %s\n", project)
	}
	if hint != "" {
		fmt.Printf("hint: %s\n", hint)
	}
	if len(hits) == 0 {
		fmt.Fprintln(os.Stderr, "no results")
		return
	}
	for i, h := range hits {
		loc := fmt.Sprintf("%s:%d-%d", h.Path, h.StartLine, h.EndLine)
		header := fmt.Sprintf("─── #%d %s  (%s)  score=%.4f", i+1, loc, h.Kind, h.Score)
		if h.RerankScore > 0 {
			header += fmt.Sprintf("  rerank=%.4f", h.RerankScore)
		}
		if h.Role != "" {
			header += "  role=" + h.Role
		}
		fmt.Println(header)
		if h.Content != "" {
			fmt.Println(truncate(h.Content, maxBytes))
		}
		fmt.Println()
	}
}

// printContextText emits a human-readable rendering of a ContextOutput.
// maxBytes limits content display (0 = no limit). Mirrors the layout
// cmdQuery uses for hits so the two surfaces feel like the same tool.
// printContextText renders the bundle. When answerHandled is true the
// answer was already streamed to stdout by the caller, so the answer
// block is skipped here to avoid printing it twice.
func printContextText(out mcp.ContextOutput, maxBytes int, answerHandled bool) {
	if out.Status != "ok" {
		printContextError(out)
		return
	}
	printContextHeader(out)
	if !answerHandled && out.Answer != "" {
		if out.AnswerModel != "" {
			fmt.Printf("answer (%s):\n", out.AnswerModel)
		}
		fmt.Printf("%s\n\n", out.Answer)
	}
	printSuggestedReads(out.SuggestedReads, maxBytes)
	printSymbols(out.Symbols)
	printReferences(out.References)
	printAnnotations(out.Annotations)
	printSemanticHits(out.SemanticHits)
	printGraph(out.Graph)
	printRelatedFiles(out.RelatedFiles)
	printConcerns(out.Concerns)
	printNextActionAndAvoid(out)
}

// printConcerns renders the assemble completeness signal (#725): which query
// concerns the working set covers vs which the byte budget dropped.
func printConcerns(c *mcp.AssembleConcerns) {
	if c == nil || (len(c.Covered) == 0 && len(c.Dropped) == 0) {
		return
	}
	fmt.Printf("Concerns: covered %d, dropped %d\n", len(c.Covered), len(c.Dropped))
	if len(c.Dropped) > 0 {
		fmt.Printf("  ⚠ dropped (no symbol body in the set): %s\n", strings.Join(c.Dropped, ", "))
	}
	fmt.Println()
}

func printContextError(out mcp.ContextOutput) {
	fmt.Fprintf(os.Stderr, "status: %s\n", out.Status)
	if out.Hint != "" {
		fmt.Fprintf(os.Stderr, "hint:   %s\n", out.Hint)
	}
	if out.Endpoint != "" {
		fmt.Fprintf(os.Stderr, "endpoint: %s\n", out.Endpoint)
	}
}

func printContextHeader(out mcp.ContextOutput) {
	fmt.Printf("intent: %s  project: %s\n", out.Intent, out.Project)
	if out.Trust != nil && out.Trust.Stale {
		fmt.Println("⚠ index is stale — refresh recommended")
	}
	if out.Hint != "" {
		fmt.Printf("hint: %s\n", out.Hint)
	}
	fmt.Println()
}

func printSuggestedReads(reads []mcp.SuggestedRead, maxBytes int) {
	if len(reads) == 0 {
		return
	}
	fmt.Println("Suggested reads:")
	for i, r := range reads {
		loc := r.Path
		if r.StartLine > 0 || r.EndLine > 0 {
			loc = fmt.Sprintf("%s:%d-%d", r.Path, r.StartLine, r.EndLine)
		}
		fmt.Printf("  %d. %s\n     reason: %s\n", i+1, loc, r.Reason)
		if r.Content != "" {
			body := truncate(r.Content, maxBytes)
			for line := range strings.SplitSeq(strings.TrimRight(body, "\n"), "\n") {
				fmt.Printf("     │ %s\n", line)
			}
			if r.Truncated && maxBytes == 0 {
				fmt.Println("     │ … (truncated; Read the file for the rest)")
			}
		}
	}
	fmt.Println()
}

func printSymbols(symbols []mcp.SymbolHit) {
	if len(symbols) == 0 {
		return
	}
	fmt.Println("Relevant symbols:")
	for _, sym := range symbols {
		loc := sym.Path
		if sym.StartLine > 0 {
			loc = fmt.Sprintf("%s:%d", sym.Path, sym.StartLine)
		}
		fmt.Printf("  - %s  (%s)  %s\n", sym.QualifiedName, sym.Kind, loc)
		if sym.Signature != "" {
			fmt.Printf("      sig: %s\n", sym.Signature)
		}
		if sym.Doc != "" {
			for line := range strings.SplitSeq(sym.Doc, "\n") {
				fmt.Printf("      doc: %s\n", line)
			}
		}
	}
	fmt.Println()
}

func printRelatedFiles(paths []string) {
	if len(paths) == 0 {
		return
	}
	fmt.Println("Related files:")
	for _, p := range paths {
		fmt.Printf("  %s\n", p)
	}
	fmt.Println()
}

func printReferences(refs []mcp.RefHit) {
	if len(refs) == 0 {
		return
	}
	fmt.Println("References:")
	for _, r := range refs {
		fmt.Printf("  - %s:%d  %s\n", r.Path, r.Line, r.Snippet)
	}
	fmt.Println()
}

func printAnnotations(anns map[string]mcp.PathMeta) {
	if len(anns) == 0 {
		return
	}
	fmt.Println("Annotations:")
	for path, meta := range anns {
		fmt.Printf("  %s\n", path)
		if meta.LastCommit != "" {
			fmt.Printf("    last:    %s  %s\n", meta.LastCommit, meta.LastAuthor)
		}
		if len(meta.Owners) > 0 {
			fmt.Printf("    owners:  %s\n", strings.Join(meta.Owners, " "))
		}
		if meta.NearestDoc != "" {
			fmt.Printf("    doc:     %s\n", meta.NearestDoc)
		}
		if len(meta.Tests) > 0 {
			fmt.Printf("    tests:   %s\n", strings.Join(meta.Tests, " "))
		}
		if meta.BuildTags != "" {
			fmt.Printf("    build:   %s\n", meta.BuildTags)
		}
		if meta.Package != "" {
			fmt.Printf("    package: %s\n", meta.Package)
		}
	}
	fmt.Println()
}

func printSemanticHits(hits []mcp.SemHit) {
	if len(hits) == 0 {
		return
	}
	fmt.Println("Semantic hits:")
	for i, h := range hits {
		loc := fmt.Sprintf("%s:%d-%d", h.Path, h.StartLine, h.EndLine)
		fmt.Printf("  %d. %s  (%s)  score=%.4f", i+1, loc, h.Kind, h.Score)
		if h.Reason != "" {
			fmt.Printf("  (%s)", h.Reason)
		}
		fmt.Println()
	}
	fmt.Println()
}

func printGraph(gr *mcp.GraphResult) {
	if gr == nil || (len(gr.Nodes) == 0 && len(gr.Edges) == 0) {
		return
	}
	fmt.Println("Graph:")
	for _, n := range gr.Nodes {
		fmt.Printf("  node  %-12s  %s\n", n.Kind, n.ID)
	}
	for _, e := range gr.Edges {
		fmt.Printf("  edge  %-12s  %s → %s\n", e.Kind, e.From, e.To)
	}
	fmt.Println()
}

func printNextActionAndAvoid(out mcp.ContextOutput) {
	if out.NextAction != "" {
		fmt.Printf("Next action:\n  %s\n\n", out.NextAction)
	}
	if out.Avoid != "" {
		fmt.Printf("Avoid:\n  %s\n", out.Avoid)
	}
}
