package ignore

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// indexConfig is the parsed `[index]` section of .dex/config.toml. Both
// lists carry gitignore-grammar patterns (see Matcher): Include is the
// opt-in allow-list of paths to index; Ignore composes on top of the
// .gitignore/.dexignore exclude set.
type indexConfig struct {
	Include []string
	Ignore  []string
}

// loadIndexConfig reads .dex/config.toml under root. A missing file is
// not an error — a zero-value config is returned (which, under the
// opt-in model, means "index nothing"). The parser handles a
// deliberately tiny TOML subset — [sections] and string arrays, single-
// or multi-line — so dex doesn't pull in a TOML dependency for one small
// file. It mirrors the sibling scalar-only parser in
// internal/guide/config.go.
//
//	[index]
//	include = [
//	  "cmd/",
//	  "internal/",
//	  "*.md",
//	]
//	ignore = ["testdata/", "benchmark/results/"]
func loadIndexConfig(root string) (indexConfig, error) {
	var cfg indexConfig
	path := filepath.Join(root, ".dex", "config.toml")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	defer func() { _ = f.Close() }()

	var (
		section string
		curKey  string   // key whose array is being accumulated across lines
		curVals []string // items gathered so far for curKey
		inArray bool
	)

	commit := func(key string, vals []string) {
		if section != "index" {
			return
		}
		switch key {
		case "include":
			cfg.Include = append(cfg.Include, vals...)
		case "ignore":
			cfg.Ignore = append(cfg.Ignore, vals...)
		}
	}

	sc := bufio.NewScanner(f)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimSpace(stripComment(sc.Text()))
		if line == "" {
			continue
		}

		if inArray {
			vals, closed := parseArrayItems(line)
			curVals = append(curVals, vals...)
			if closed {
				commit(curKey, curVals)
				inArray, curKey, curVals = false, "", nil
			}
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}

		rawKey, rawVal, ok := strings.Cut(line, "=")
		if !ok {
			return cfg, fmt.Errorf("%s:%d: expected key = value", path, ln)
		}
		key := strings.TrimSpace(rawKey)
		val := strings.TrimSpace(rawVal)

		if !strings.HasPrefix(val, "[") {
			// Scalar — no scalar keys are meaningful under [index] today;
			// skip quietly so the file can carry other settings later.
			continue
		}

		vals, closed := parseArrayItems(strings.TrimSpace(val[1:]))
		if closed {
			commit(key, vals)
			continue
		}
		inArray, curKey, curVals = true, key, vals
	}
	if err := sc.Err(); err != nil {
		return cfg, err
	}
	if inArray {
		return cfg, fmt.Errorf("%s: unterminated array for key %q", path, curKey)
	}
	return cfg, nil
}

// parseArrayItems pulls quoted string items out of one line of a TOML
// array body. It stops at the first closing ']' (reporting closed=true)
// and tolerates trailing commas and surrounding whitespace.
func parseArrayItems(s string) (items []string, closed bool) {
	if i := strings.IndexByte(s, ']'); i >= 0 {
		closed = true
		s = s[:i]
	}
	for part := range strings.SplitSeq(s, ",") {
		part = strings.Trim(strings.TrimSpace(part), `"`)
		if part != "" {
			items = append(items, part)
		}
	}
	return items, closed
}

// stripComment removes a trailing '#' comment, ignoring '#' inside
// double quotes. gitignore-style patterns don't carry meaningful '#'
// (a leading '#' is a comment there too), so this naive scan is enough.
func stripComment(line string) string {
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case '#':
			if !inQuote {
				return line[:i]
			}
		}
	}
	return line
}
