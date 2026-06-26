package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	dexctx "github.com/alehatsman/dex/internal/ctx"
	"github.com/alehatsman/dex/internal/heatmap"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/tokens"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type SessionInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Action      string `json:"action"                 jsonschema:"set_task | add_note | add_file | get | clear | snapshot | recap | budget | heatmap | export | import"`
	Task        string `json:"task,omitempty"         jsonschema:"task description for set_task action"`
	Note        string `json:"note,omitempty"         jsonschema:"note text for add_note action"`
	File        string `json:"file,omitempty"         jsonschema:"relative file path for add_file action"`
	Op          string `json:"op,omitempty"           jsonschema:"file operation: read (default) or write"`
	Bundle      string `json:"bundle,omitempty"       jsonschema:"dex-session-v1 JSON bundle (from a prior export) to restore for the import action"`
	WindowSize  int    `json:"window_size,omitempty"  jsonschema:"context window size in tokens for budget action (default 128000)"`
	Budget      int    `json:"budget,omitempty"       jsonschema:"token budget for the recap action's digest (default 4000); the working set is packed cheapest-first until it is spent"`
}

type SessionFileOutput struct {
	Path      string `json:"path"`
	Op        string `json:"op"`
	TouchedAt string `json:"touched_at"`
}

type SessionOutput struct {
	Status    string              `json:"status"` // "ok" | "no-index" | "error"
	Hint      string              `json:"hint,omitempty"`
	Content   string              `json:"content,omitempty"` // snapshot markdown (action=snapshot only)
	ID        int64               `json:"id,omitempty"`
	StartedAt string              `json:"started_at,omitempty"`
	UpdatedAt string              `json:"updated_at,omitempty"`
	Duration  string              `json:"duration,omitempty"` // human-readable session age
	Task      string              `json:"task,omitempty"`
	Notes     string              `json:"notes,omitempty"`
	NoteCount int                 `json:"note_count,omitempty"`
	FileCount int                 `json:"file_count,omitempty"`
	Files     []SessionFileOutput `json:"files,omitempty"`
	// Budget fields (action=budget only)
	WindowSize      int     `json:"window_size,omitempty"`
	UsedTokens      int     `json:"used_tokens,omitempty"`
	RemainingTokens int     `json:"remaining_tokens,omitempty"`
	Utilization     float64 `json:"utilization,omitempty"`
	Recommendation  string  `json:"recommendation,omitempty"`
}

func (s *Server) session(ctx context.Context, _ *sdk.CallToolRequest, in SessionInput) (*sdk.CallToolResult, SessionOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, SessionOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, SessionOutput{
			Status: "no-index",
			Hint:   fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root),
		}, nil
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, SessionOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	switch in.Action {
	case "set_task":
		if err := st.SessionSetTask(ctx, in.Task); err != nil {
			return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
		}
	case "add_note":
		if in.Note == "" {
			return nil, SessionOutput{Status: "error", Hint: "note is empty"}, nil
		}
		if err := st.SessionAddNote(ctx, in.Note); err != nil {
			return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
		}
	case "add_file":
		if in.File == "" {
			return nil, SessionOutput{Status: "error", Hint: "file is empty"}, nil
		}
		op := in.Op
		if op == "" {
			op = "read"
		}
		if err := st.SessionAddFile(ctx, in.File, op); err != nil {
			return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
		}
	case "clear":
		if err := st.SessionClear(ctx); err != nil {
			return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
		}
		return nil, SessionOutput{Status: "ok"}, nil
	case "snapshot":
		return s.sessionSnapshot(ctx, st, p.Root)
	case "recap":
		return s.sessionRecap(ctx, st, p.Root, in.Budget)
	case "budget":
		return s.sessionBudget(ctx, st, p.Root, in.WindowSize)
	case "heatmap":
		return s.sessionHeatmap(p.CacheDir)
	case "export":
		return s.sessionExport(ctx, st, p.Root)
	case "import":
		return s.sessionImport(ctx, st, p.Root, in.Bundle)
	case "get", "":
		// fall through to the read below
	default:
		return nil, SessionOutput{Status: "error", Hint: fmt.Sprintf("unknown action %q — want: set_task | add_note | add_file | get | clear | snapshot | recap | budget | heatmap | export | import", in.Action)}, nil
	}

	ss, ok, err := st.SessionGet(ctx)
	if err != nil {
		return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
	}
	if !ok {
		return nil, SessionOutput{Status: "ok", Hint: "no session started — use action=set_task to begin"}, nil
	}

	noteCount := 0
	if ss.Notes != "" {
		noteCount = strings.Count(ss.Notes, "\n") + 1
	}
	out := SessionOutput{
		Status:    "ok",
		ID:        ss.ID,
		StartedAt: ss.StartedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt: ss.UpdatedAt.Format("2006-01-02 15:04:05"),
		Duration:  formatDuration(time.Since(ss.StartedAt)),
		Task:      ss.Task,
		Notes:     ss.Notes,
		NoteCount: noteCount,
		FileCount: len(ss.Files),
	}
	for _, f := range ss.Files {
		out.Files = append(out.Files, SessionFileOutput{
			Path:      f.Path,
			Op:        f.Op,
			TouchedAt: f.TouchedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return nil, out, nil
}

// sessionBudget estimates context window utilization for the current session.
// It reads file sizes from disk to approximate tokens consumed, then returns
// utilization, remaining capacity, and a pressure-level recommendation.
func (s *Server) sessionBudget(ctx context.Context, st *store.Store, projectRoot string, windowSize int) (*sdk.CallToolResult, SessionOutput, error) {
	if windowSize <= 0 {
		windowSize = dexctx.DefaultWindowSize
	}

	ss, ok, err := st.SessionGet(ctx)
	if err != nil {
		return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
	}
	if !ok {
		ledger := dexctx.Ledger{WindowSize: windowSize}
		return nil, SessionOutput{
			Status:          "ok",
			Hint:            "no active session",
			WindowSize:      ledger.WindowSize,
			UsedTokens:      0,
			RemainingTokens: ledger.Remaining(),
			Utilization:     0,
			Recommendation:  string(ledger.Pressure()),
		}, nil
	}

	var used int
	// task + notes: rough token estimate from byte length
	used += dexctx.BytesToTokens(int64(len(ss.Task) + len(ss.Notes)))

	// files: estimate from actual file size on disk (deduped by path)
	seen := make(map[string]struct{}, len(ss.Files))
	for _, f := range ss.Files {
		if _, dup := seen[f.Path]; dup {
			continue
		}
		seen[f.Path] = struct{}{}
		abs := filepath.Join(projectRoot, f.Path)
		if info, statErr := os.Stat(abs); statErr == nil {
			used += dexctx.BytesToTokens(info.Size())
		}
	}

	ledger := dexctx.Ledger{WindowSize: windowSize, UsedTokens: used}
	return nil, SessionOutput{
		Status:          "ok",
		WindowSize:      ledger.WindowSize,
		UsedTokens:      ledger.UsedTokens,
		RemainingTokens: ledger.Remaining(),
		Utilization:     ledger.Utilization(),
		Recommendation:  string(ledger.Pressure()),
	}, nil
}

// sessionSnapshot generates a structured recovery block for use after context
// compaction. It lists file_view and search_semantic calls that will
// cheaply re-establish context for the declared task and touched files.
func (s *Server) sessionSnapshot(ctx context.Context, st *store.Store, projectRoot string) (*sdk.CallToolResult, SessionOutput, error) {
	ss, ok, err := st.SessionGet(ctx)
	if err != nil {
		return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
	}
	if !ok {
		return nil, SessionOutput{Status: "ok", Hint: "no active session — start one with action=set_task"}, nil
	}

	var b strings.Builder
	b.WriteString("# Session Recovery Snapshot\n\n")

	if ss.Task != "" {
		b.WriteString("## Task\n")
		b.WriteString(ss.Task)
		b.WriteString("\n\n")
		b.WriteString("## Re-establish context\n\n")
		fmt.Fprintf(&b, "```\nfind: {\"query\": %q, \"project_root\": %q}\n```\n\n", ss.Task, projectRoot)
	}

	if len(ss.Files) > 0 {
		b.WriteString("## Files to re-read\n\n")
		seen := make(map[string]struct{}, len(ss.Files))
		for _, f := range ss.Files {
			if _, dup := seen[f.Path]; dup {
				continue
			}
			seen[f.Path] = struct{}{}
			mode := ReadModeSignatures
			if f.Op == "write" {
				mode = ReadModeMap
			}
			fmt.Fprintf(&b, "```\nread: {\"path\": %q, \"mode\": %q, \"project_root\": %q}\n```\n",
				f.Path, mode, projectRoot)
		}
		b.WriteString("\n")
	}

	if ss.Notes != "" {
		b.WriteString("## Session notes\n\n")
		b.WriteString(ss.Notes)
		b.WriteString("\n")
	}

	return nil, SessionOutput{
		Status:  "ok",
		Content: b.String(),
	}, nil
}

// recapDefaultBudget mirrors navReorientRecapBudget (cmd/dex/bench_nav.go): the
// token budget the re-orientation lane prices recap() against. Keeping the live
// default in step with the modeled one means the gate measures what ships.
const recapDefaultBudget = 4000

// sessionRecap renders a budget-bounded digest of the session's working set —
// #346's live counterpart to the benchnav re-orientation lane. Where
// sessionSnapshot suggests find/read calls to RE-RUN, recap delivers the
// content inline: for each working-set file, its path plus a compressed
// signature skeleton (the qualified names it defines, from the graph). Entries
// are packed cheapest-first so a fixed budget restores as many files as
// possible — an oversized working set truncates, exactly as the reorient model
// prices it (buildRecapModel). The thesis: restore WHERE you were after
// compaction from one compact digest, not by re-running the exploration.
func (s *Server) sessionRecap(ctx context.Context, st *store.Store, projectRoot string, budget int) (*sdk.CallToolResult, SessionOutput, error) {
	_ = projectRoot // working set is self-describing; kept for signature parity with sessionSnapshot
	if budget <= 0 {
		budget = recapDefaultBudget
	}
	ss, ok, err := st.SessionGet(ctx)
	if err != nil {
		return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
	}
	if !ok {
		return nil, SessionOutput{Status: "ok", Hint: "no active session — start one with action=set_task"}, nil
	}

	// Dedup the working set, preserving touch order.
	var files []string
	seen := make(map[string]struct{}, len(ss.Files))
	for _, f := range ss.Files {
		if _, dup := seen[f.Path]; dup {
			continue
		}
		seen[f.Path] = struct{}{}
		files = append(files, f.Path)
	}

	// Group the qualified names each file defines (its signature skeleton). One
	// GraphAllNodes scan mirrors buildRecapModel — there is no per-file node
	// query, and a session's working set is small enough that the single scan is
	// cheap relative to the digest it yields. A missing graph degrades to
	// path-only entries rather than failing the recap.
	symsByFile := map[string][]string{}
	if nodes, nerr := st.GraphAllNodes(ctx); nerr == nil {
		for _, n := range nodes {
			if n.FilePath == "" || n.QualifiedName == "" {
				continue
			}
			symsByFile[n.FilePath] = append(symsByFile[n.FilePath], n.QualifiedName)
		}
		for _, syms := range symsByFile {
			sort.Strings(syms)
		}
	}

	// Price each file's digest entry; pack cheapest-first so the fixed budget
	// covers as many files as possible (the coverage the reorient lane gates).
	type recapEntry struct {
		path string
		syms []string
		cost int
	}
	entries := make([]recapEntry, 0, len(files))
	for _, p := range files {
		syms := symsByFile[p]
		entries = append(entries, recapEntry{path: p, syms: syms, cost: tokens.Count(recapEntryText(p, syms))})
	}
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return entries[order[a]].cost < entries[order[b]].cost })
	fit := make(map[int]bool, len(entries))
	spent := 0
	for _, i := range order {
		if spent+entries[i].cost > budget {
			continue
		}
		spent += entries[i].cost
		fit[i] = true
	}

	// Render the fitted entries in touch order (selection was cheapest-first).
	var b strings.Builder
	b.WriteString("# Session Recap\n\n")
	b.WriteString("Restore your working set without re-reading. Each file lists the symbols it defines.\n\n")
	if ss.Task != "" {
		b.WriteString("## Task\n")
		b.WriteString(ss.Task)
		b.WriteString("\n\n")
	}
	b.WriteString("## Working set\n\n")
	omitted := 0
	for i, e := range entries {
		if !fit[i] {
			omitted++
			continue
		}
		fmt.Fprintf(&b, "### %s\n", e.path)
		if len(e.syms) == 0 {
			b.WriteString("_(no indexed symbols)_\n\n")
			continue
		}
		for _, sym := range e.syms {
			fmt.Fprintf(&b, "- %s\n", sym)
		}
		b.WriteString("\n")
	}
	if len(entries) == 0 {
		b.WriteString("_(no files touched this session)_\n\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "_%d file(s) omitted to fit the %d-token budget — raise `budget` to include them._\n\n", omitted, budget)
	}
	if ss.Notes != "" {
		b.WriteString("## Session notes\n\n")
		b.WriteString(ss.Notes)
		b.WriteString("\n")
	}

	return nil, SessionOutput{
		Status:     "ok",
		Content:    b.String(),
		FileCount:  len(entries) - omitted,
		UsedTokens: spent,
	}, nil
}

// recapEntryText is the digest slot a working-set file occupies — its path plus
// the symbol names it defines, one per line. Its token count is the price the
// budget pays for that file; it matches benchnav's reorient Entry cost exactly
// (buildRecapModel), so the live digest and the gated model price a file alike.
func recapEntryText(path string, syms []string) string {
	if len(syms) == 0 {
		return path
	}
	return path + "\n" + strings.Join(syms, "\n")
}

// formatDuration produces a compact human-readable duration string.
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// sessionBundleSchema versions the handoff bundle wire format (#603). Bump it
// on any shape change so import can fail closed on an unknown schema rather than
// silently mis-restoring.
const sessionBundleSchema = "dex-session-v1"

// sessionBundle is the export/import wire format for cross-reset session handoff
// (#603). It carries only paths + content etags — never file content — so it is
// safe to commit (e.g. .dex/session.json) for team handoff.
type sessionBundle struct {
	Schema     string       `json:"schema"`
	Project    string       `json:"project,omitempty"` // sha256[:16] of the project root (privacy: hashed)
	ExportedAt string       `json:"exported_at,omitempty"`
	Task       string       `json:"task,omitempty"`
	Notes      string       `json:"notes,omitempty"`
	Files      []bundleFile `json:"files,omitempty"`
}

// bundleFile is one working-set file in a handoff bundle: its path, content etag
// at export time (sha256[:16], for staleness detection on import), and op.
type bundleFile struct {
	Path string `json:"path"`
	Etag string `json:"etag,omitempty"`
	Op   string `json:"op,omitempty"`
}

// fileEtag returns sha256[:16] of a file's content — the same etag scheme the
// read tool emits (server_summarize_setup.go) so an exported etag compares
// equal to a later read's etag when the file is unchanged. ok=false when the
// file can't be read (missing/permission), which import treats as "missing".
func fileEtag(abs string) (etag string, ok bool) {
	d, err := os.ReadFile(abs)
	if err != nil {
		return "", false
	}
	h := sha256.Sum256(d)
	return hex.EncodeToString(h[:])[:16], true
}

// hashRoot is the privacy-preserving project identifier stamped into a bundle —
// sha256[:16] of the absolute root, never the path itself.
func hashRoot(root string) string {
	h := sha256.Sum256([]byte(root))
	return hex.EncodeToString(h[:])[:16]
}

// sessionExport serialises the current session into a dex-session-v1 bundle:
// task, notes, and the deduped working-set files each stamped with its current
// content etag. No file content is included — the bundle is paths + etags only,
// safe to persist or hand off. Returns the JSON in Content.
func (s *Server) sessionExport(ctx context.Context, st *store.Store, projectRoot string) (*sdk.CallToolResult, SessionOutput, error) {
	ss, ok, err := st.SessionGet(ctx)
	if err != nil {
		return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
	}
	if !ok {
		return nil, SessionOutput{Status: "ok", Hint: "no active session to export — start one with action=set_task"}, nil
	}

	b := sessionBundle{
		Schema:     sessionBundleSchema,
		Project:    hashRoot(projectRoot),
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Task:       ss.Task,
		Notes:      ss.Notes,
	}
	seen := make(map[string]struct{}, len(ss.Files))
	for _, f := range ss.Files {
		if _, dup := seen[f.Path]; dup {
			continue
		}
		seen[f.Path] = struct{}{}
		bf := bundleFile{Path: f.Path, Op: f.Op}
		if et, ok := fileEtag(filepath.Join(projectRoot, f.Path)); ok {
			bf.Etag = et
		}
		b.Files = append(b.Files, bf)
	}

	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, SessionOutput{Status: "error", Hint: fmt.Sprintf("marshal bundle: %v", err)}, nil
	}
	return nil, SessionOutput{Status: "ok", Content: string(data), FileCount: len(b.Files)}, nil
}

// sessionImport rehydrates a fresh session from a dex-session-v1 bundle (#603):
// it restores task/notes/files into the current session, then returns a recovery
// digest (the recap working-set skeleton) with a handoff header. Etags are used
// for staleness detection only — files whose content changed (or vanished) since
// export are called out so the agent re-reads those first. It does NOT pre-warm
// the read cache: a true reset leaves the agent without the bytes, so the first
// read must deliver full content (and the normal read path marks it from there).
func (s *Server) sessionImport(ctx context.Context, st *store.Store, projectRoot, bundleJSON string) (*sdk.CallToolResult, SessionOutput, error) {
	if strings.TrimSpace(bundleJSON) == "" {
		return nil, SessionOutput{Status: "error", Hint: "import requires the exported bundle in the `bundle` field"}, nil
	}
	var b sessionBundle
	if err := json.Unmarshal([]byte(bundleJSON), &b); err != nil {
		return nil, SessionOutput{Status: "error", Hint: fmt.Sprintf("parse bundle: %v", err)}, nil
	}
	if b.Schema != sessionBundleSchema {
		return nil, SessionOutput{Status: "error", Hint: fmt.Sprintf("unsupported bundle schema %q — want %q", b.Schema, sessionBundleSchema)}, nil
	}

	files := make([]store.SessionFile, 0, len(b.Files))
	for _, f := range b.Files {
		if f.Path == "" {
			continue
		}
		// Reject bundle paths that escape the project root (path-traversal guard).
		if strings.Contains(f.Path, "..") {
			return nil, SessionOutput{Status: "error", Hint: fmt.Sprintf("bundle file path %q is invalid: contains ..", f.Path)}, nil
		}
		files = append(files, store.SessionFile{Path: f.Path, Op: f.Op})
	}
	if err := st.SessionImport(ctx, b.Task, b.Notes, files); err != nil {
		return nil, SessionOutput{Status: "error", Hint: err.Error()}, nil
	}

	// Integrity: which bundled files changed or vanished since export?
	var stale, missing []string
	for _, f := range b.Files {
		if f.Etag == "" || f.Path == "" || strings.Contains(f.Path, "..") {
			continue
		}
		cur, ok := fileEtag(filepath.Join(projectRoot, f.Path))
		switch {
		case !ok:
			missing = append(missing, f.Path)
		case cur != f.Etag:
			stale = append(stale, f.Path)
		}
	}

	// Render the recovery digest from the now-restored session, then prepend a
	// handoff header + staleness warnings.
	_, recap, recapErr := s.sessionRecap(ctx, st, projectRoot, 0)
	if recapErr != nil {
		return nil, SessionOutput{Status: "error", Hint: fmt.Sprintf("generate recap: %v", recapErr)}, nil
	}

	var head strings.Builder
	head.WriteString("# Session Handoff Imported\n\n")
	fmt.Fprintf(&head, "Restored task + %d file(s) + notes", len(files))
	if b.ExportedAt != "" {
		fmt.Fprintf(&head, " from a bundle exported %s", b.ExportedAt)
	}
	head.WriteString(".\n\n")
	if len(stale) > 0 {
		head.WriteString("## Changed since handoff — re-read these first\n\n")
		for _, p := range stale {
			fmt.Fprintf(&head, "- %s\n", p)
		}
		head.WriteString("\n")
	}
	if len(missing) > 0 {
		head.WriteString("## Missing since handoff\n\n")
		for _, p := range missing {
			fmt.Fprintf(&head, "- %s\n", p)
		}
		head.WriteString("\n")
	}

	return nil, SessionOutput{
		Status:    "ok",
		Content:   head.String() + recap.Content,
		Task:      b.Task,
		FileCount: len(files),
	}, nil
}

// sessionHeatmap returns the file access heatmap for a project.
func (s *Server) sessionHeatmap(cacheDir string) (*sdk.CallToolResult, SessionOutput, error) {
	hm := heatmap.Load(cacheDir)
	return nil, SessionOutput{Status: "ok", Content: hm.Format(15)}, nil
}
