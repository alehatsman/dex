package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/summarize"
)

// rollupPromptVersion is baked into every stored dir_summaries row. Bump it
// whenever BuildRollupSystem or buildRollupInput changes meaning — existing
// rollups then read as stale and regenerate. Independent of summaryPromptVersion
// so a file-prompt change and a rollup-prompt change invalidate independently.
const rollupPromptVersion = 1

// runRollupGenerate rolls up hierarchical directory summaries (package →
// subsystem → repo root) from the current file summaries. Each directory's
// staleness signal is a composite hash of its children's source hashes, so
// touching one file re-rolls only its ancestors. Isolated from retrieval, like
// the file pass: nothing on the ask path reads dir_summaries.
func runRollupGenerate(ctx context.Context, st *store.Store, focus string, force, verbose bool, format string) error {
	files, err := st.CodeFilePaths(ctx)
	if err != nil {
		return err
	}

	// A file without a stored summary is skipped — we roll up prose that exists;
	// the next run picks it up once its file summary lands.
	fileHash := map[string]string{}
	fileSummary := map[string]string{}
	for path := range files {
		fs, ok, err := st.GetFileSummary(ctx, path)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		fileHash[path] = fs.SourceHash
		fileSummary[path] = fs.Summary
	}

	// The directory tree and every node's composite hash are pure functions of
	// the file hashes (buildDirTree) — the invalidation property lives there and
	// is unit-tested independently of the chat backend.
	tree := buildDirTree(fileHash)

	client := newChatClient()
	system := summarize.BuildRollupSystem(focus)
	// results[dir] = generated/reused prose, read by parents for their input.
	// Deeper dirs are processed first, so a parent always finds its children here.
	results := map[string]string{}

	var done, skipped, failed, empty int
	for _, dir := range tree.order {
		composite := tree.composite[dir]
		if composite == "" {
			empty++ // no summarized descendants yet — nothing to roll up
			continue
		}
		members := rollupMembers(tree.childFiles[dir], tree.subdirs[dir], fileSummary, results)

		if !force {
			h, pv, ok, err := st.DirSummaryMeta(ctx, dir)
			if err != nil {
				return err
			}
			if ok && pv == rollupPromptVersion && h == composite {
				// Fresh: reuse the stored prose so ancestors summarize from it.
				ds, _, err := st.GetDirSummary(ctx, dir)
				if err != nil {
					return err
				}
				results[dir] = ds.Summary
				skipped++
				continue
			}
		}

		msgs := []chat.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: buildRollupInput(dir, members)},
		}
		resp, err := client.Generate(ctx, msgs, chat.Options{})
		if err != nil {
			if errors.Is(err, chat.ErrUnreachable) {
				return fmt.Errorf("chat service offline (%w) — start it or set DEX_CHAT_URL", err)
			}
			fmt.Fprintf(os.Stderr, "dex: rollup %s failed: %v\n", dirLabel(dir), err)
			// No prose recorded: the parent omits this member's block but its hash
			// still counts in the composite (owned by the tree), so invalidation
			// stays correct. --force recovers the missing block next run.
			failed++
			continue
		}
		summaryText := strings.TrimSpace(resp.Content)
		if err := st.UpsertDirSummary(ctx, store.DirSummary{
			Path:          dir,
			SourceHash:    composite,
			PromptVersion: rollupPromptVersion,
			Model:         client.ModelName(),
			Summary:       summaryText,
			GeneratedAt:   time.Now(),
		}); err != nil {
			return err
		}
		results[dir] = summaryText
		done++
		if verbose {
			fmt.Printf("rolled up %s\n", dirLabel(dir))
		}
	}

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]int{
			"rolled_up": done, "up_to_date": skipped, "failed": failed,
			"empty": empty, "directories": len(tree.order),
		})
	}
	fmt.Printf("rollups: %d generated, %d up-to-date, %d failed (%d directories)\n",
		done, skipped, failed, len(tree.order))
	return nil
}

// rollupMember is one child's labeled summary block feeding a directory rollup's
// model input (hashes are owned by the dir tree, not carried here).
type rollupMember struct {
	name    string
	summary string
}

// rollupMembers assembles a directory's children (immediate files + immediate
// subdirs) into labeled summary blocks for the model input. Hashes are owned by
// the dir tree; this only gathers prose. Child dir prose is read from `results`
// (already generated, deeper-first); a child with no prose (a failed rollup or
// empty subtree) is omitted from the input but its hash still counts in the tree.
func rollupMembers(files, subdirs []string, fileSummary, results map[string]string) []rollupMember {
	var members []rollupMember
	for _, f := range files { // files/subdirs arrive pre-sorted from the tree
		members = append(members, rollupMember{name: baseName(f), summary: fileSummary[f]})
	}
	for _, d := range subdirs {
		if s := results[d]; s != "" {
			members = append(members, rollupMember{name: baseName(d) + "/", summary: s})
		}
	}
	return members
}

// dirTree is the directory rollup graph derived purely from the set of
// summarized files: which files/subdirs each directory holds, a bottom-up
// processing order, and every node's composite source hash. All fields are a
// deterministic function of the input file→hash map, so the invalidation
// property ("touch one file, re-roll only its ancestors") is unit-testable
// without a store or chat backend.
type dirTree struct {
	order      []string                       // dirs deepest-first
	childFiles map[string][]string            // dir → immediate files, sorted
	childDirs  map[string]map[string]struct{} // dir → immediate subdirs (set)
	subdirs    map[string][]string            // dir → immediate subdirs, sorted
	composite  map[string]string              // dir → composite hash ("" = no summarized descendants)
}

// buildDirTree constructs the rollup tree from file source hashes and computes
// each directory's composite hash bottom-up. A directory's composite is
// RollupHash over its immediate child files' hashes plus its immediate child
// dirs' (already-computed) composites; empty-composite children contribute
// nothing, so a directory with no summarized descendants stays "".
func buildDirTree(fileHash map[string]string) dirTree {
	t := dirTree{
		childFiles: map[string][]string{},
		childDirs:  map[string]map[string]struct{}{},
		subdirs:    map[string][]string{},
		composite:  map[string]string{},
	}
	dirSet := map[string]struct{}{"": {}}
	for path := range fileHash {
		dir := dirOf(path)
		t.childFiles[dir] = append(t.childFiles[dir], path)
		for d := dir; ; d = dirOf(d) { // register d and every ancestor + the edges
			dirSet[d] = struct{}{}
			if d == "" {
				break
			}
			parent := dirOf(d)
			if t.childDirs[parent] == nil {
				t.childDirs[parent] = map[string]struct{}{}
			}
			t.childDirs[parent][d] = struct{}{}
		}
	}

	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
		sort.Strings(t.childFiles[d])
		subs := make([]string, 0, len(t.childDirs[d]))
		for s := range t.childDirs[d] {
			subs = append(subs, s)
		}
		sort.Strings(subs)
		t.subdirs[d] = subs
	}
	sort.Slice(dirs, func(i, j int) bool {
		if di, dj := dirDepth(dirs[i]), dirDepth(dirs[j]); di != dj {
			return di > dj // deepest first
		}
		return dirs[i] < dirs[j]
	})
	t.order = dirs

	for _, dir := range dirs { // bottom-up: child composites ready before parent
		var hashes []string
		for _, f := range t.childFiles[dir] {
			hashes = append(hashes, fileHash[f])
		}
		for _, d := range t.subdirs[dir] {
			if h := t.composite[d]; h != "" {
				hashes = append(hashes, h)
			}
		}
		t.composite[dir] = store.RollupHash(hashes)
	}
	return t
}

// buildRollupInput renders a directory's member summaries into the model input.
// Capped at maxSummaryInputBytes (shared with the file pass).
func buildRollupInput(dir string, members []rollupMember) string {
	var b strings.Builder
	fmt.Fprintf(&b, "DIRECTORY: %s\n\nMember summaries:\n\n", dirLabel(dir))
	for _, m := range members {
		if b.Len() >= maxSummaryInputBytes {
			b.WriteString("\n[...truncated...]\n")
			break
		}
		fmt.Fprintf(&b, "## %s\n%s\n\n", m.name, m.summary)
	}
	return b.String()
}

// dirOf returns the parent directory of an index-relative path (forward-slashed,
// no trailing slash). The root's parent is "" (root). Mirrors path.Dir but
// yields "" — not "." — for top-level entries, matching the "" root key.
func dirOf(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ""
	}
	return p[:i]
}

// baseName returns the final path segment.
func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// dirDepth counts path segments; "" (root) is depth 0, "a" is 1, "a/b" is 2.
func dirDepth(d string) int {
	if d == "" {
		return 0
	}
	return strings.Count(d, "/") + 1
}

// dirLabel renders a directory key for human output; "" becomes "(repo root)".
func dirLabel(d string) string {
	if d == "" {
		return "(repo root)"
	}
	return d
}
