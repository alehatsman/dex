package compress

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/tokens"
)

// TestSymmapCoverage measures how many source files in the repo produce a
// non-empty SymbolMap under the current gate. Run with -v for per-file detail.
// This is a measurement test, not a pass/fail check — it reports stats only.
func TestSymmapCoverage(t *testing.T) {
	root := "../../"
	var total, nonEmpty, totalSymbols int

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".ts" && ext != ".py" {
			return nil
		}
		if strings.Contains(path, "vendor/") || strings.Contains(path, "node_modules/") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		total++
		sm := BuildSymbolMap(string(data))
		if !sm.Empty() {
			nonEmpty++
			totalSymbols += len(sm.entries)
			if testing.Verbose() {
				fmt.Printf("  %s — %d symbols\n", filepath.Base(path), len(sm.entries))
			}
		}
		return nil
	})

	pct := 0.0
	if total > 0 {
		pct = float64(nonEmpty) / float64(total) * 100
	}
	t.Logf("files scanned: %d, non-empty map: %d (%.1f%%), total symbols registered: %d",
		total, nonEmpty, pct, totalSymbols)
}

// TestSymmapSavingsBreakdown prints per-identifier savings under the 1-token
// ref scheme. Useful for calibrating the ROI gate; always passes.
func TestSymmapSavingsBreakdown(t *testing.T) {
	cases := []struct {
		ident string
		n     int
	}{
		{"handleRequest", 5},
		{"handleRequestError", 5},
		{"get_user_by_id", 8},
		{"parseConfigFile", 4},
		{"NewHTTPRequestHandler", 3},
		{"RequestHandler", 6},
		{"responseWriter", 6},
		{"ErrNotFound", 10},
	}

	fmt.Println()
	fmt.Printf("  %-26s  ident-toks  ref-toks  occ  net-save  register?\n", "ident")
	fmt.Println("  " + strings.Repeat("-", 70))
	for _, c := range cases {
		identToks := tokens.Count(c.ident)
		net, reg := symROI(c.ident, c.n)
		fmt.Printf("  %-26s  %10d  %8d  %3d  %8d  %v\n",
			c.ident, identToks, 1, c.n, net, reg)
	}
	fmt.Println()
}
