package perf

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Report holds the full perf bench output.
type Report struct {
	Dim     int         `json:"dim"`
	Results []RunResult `json:"results"`
}

// JSON serialises the report.
func (r Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// Markdown renders a human-readable result table.
func (r Report) Markdown() string {
	var sb strings.Builder
	sb.WriteString("## dex bench perf\n\n")
	sb.WriteString(fmt.Sprintf("vector dim: %d\n\n", r.Dim))

	// Compress passes
	sb.WriteString("### Compress pass latency\n\n")
	sb.WriteString("| pass | p50 | p95 | p99 | iters |\n")
	sb.WriteString("|------|----:|----:|----:|------:|\n")
	for _, rr := range r.Results {
		if !strings.HasPrefix(rr.Name, "compress/") {
			continue
		}
		sb.WriteString(fmt.Sprintf("| %-24s | %8s | %8s | %8s | %5d |\n",
			strings.TrimPrefix(rr.Name, "compress/"),
			fmtDur(rr.P50), fmtDur(rr.P95), fmtDur(rr.P99), rr.Iterations))
	}
	sb.WriteByte('\n')

	// KNN scaling curves
	sb.WriteString("### KNN vector search scaling\n\n")
	sb.WriteString("| corpus | p50 | p95 | p99 |\n")
	sb.WriteString("|-------:|----:|----:|----:|\n")
	for _, rr := range r.Results {
		if !strings.HasPrefix(rr.Name, "knn/") {
			continue
		}
		sb.WriteString(fmt.Sprintf("| %6d | %8s | %8s | %8s |\n",
			rr.CorpusSize, fmtDur(rr.P50), fmtDur(rr.P95), fmtDur(rr.P99)))
	}
	sb.WriteByte('\n')

	// BM25
	sb.WriteString("### BM25/FTS5 search\n\n")
	sb.WriteString("| corpus | p50 | p95 | p99 |\n")
	sb.WriteString("|-------:|----:|----:|----:|\n")
	for _, rr := range r.Results {
		if !strings.HasPrefix(rr.Name, "bm25/") {
			continue
		}
		sb.WriteString(fmt.Sprintf("| %6d | %8s | %8s | %8s |\n",
			rr.CorpusSize, fmtDur(rr.P50), fmtDur(rr.P95), fmtDur(rr.P99)))
	}
	sb.WriteByte('\n')

	// Storage
	sb.WriteString("### Storage footprint\n\n")
	sb.WriteString("| corpus | db size |\n")
	sb.WriteString("|-------:|--------:|\n")
	for _, rr := range r.Results {
		if !strings.HasPrefix(rr.Name, "storage/") {
			continue
		}
		sb.WriteString(fmt.Sprintf("| %6d | %s |\n",
			rr.CorpusSize, fmtBytes(rr.StorageBytes)))
	}
	sb.WriteByte('\n')

	return sb.String()
}

// Regressions compares current results against baseline and reports any
// p99 latency increases above tol (as a multiplier, e.g. 1.5 = 50% slower).
func (r Report) Regressions(baseline Report, tolMultiplier float64) []string {
	base := make(map[string]RunResult, len(baseline.Results))
	for _, rr := range baseline.Results {
		base[rr.Name] = rr
	}
	var out []string
	for _, cur := range r.Results {
		if cur.StorageBytes > 0 {
			// storage probe — compare bytes
			b, ok := base[cur.Name]
			if !ok {
				continue
			}
			if b.StorageBytes > 0 && float64(cur.StorageBytes)/float64(b.StorageBytes) > tolMultiplier {
				out = append(out, fmt.Sprintf("%s: storage grew %.0f → %.0f bytes (%.1fx > tol %.1fx)",
					cur.Name, float64(b.StorageBytes), float64(cur.StorageBytes),
					float64(cur.StorageBytes)/float64(b.StorageBytes), tolMultiplier))
			}
			continue
		}
		b, ok := base[cur.Name]
		if !ok {
			continue
		}
		if b.P99 > 0 && float64(cur.P99)/float64(b.P99) > tolMultiplier {
			out = append(out, fmt.Sprintf("%s: p99 regressed %s→%s (%.1fx > tol %.1fx)",
				cur.Name, fmtDur(b.P99), fmtDur(cur.P99),
				float64(cur.P99)/float64(b.P99), tolMultiplier))
		}
	}
	return out
}

// CheckRegression loads a baseline JSON and fails if p99 latencies regressed.
func CheckRegression(current Report, baselinePath string) error {
	data, err := os.ReadFile(baselinePath)
	if err != nil {
		return fmt.Errorf("read baseline %q: %w", baselinePath, err)
	}
	var baseline Report
	if err := json.Unmarshal(data, &baseline); err != nil {
		return fmt.Errorf("parse baseline: %w", err)
	}
	const tolMultiplier = 4.0 // allow up to 4× before flagging — p99 is noisy on shared boxes
	regs := current.Regressions(baseline, tolMultiplier)
	if len(regs) == 0 {
		return nil
	}
	return fmt.Errorf("%s (tol %.1fx)", strings.Join(regs, "; "), tolMultiplier)
}

func fmtDur(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d >= time.Microsecond:
		return fmt.Sprintf("%.1fµs", float64(d)/float64(time.Microsecond))
	default:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	}
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
