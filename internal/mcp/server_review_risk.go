package mcp

import (
	"fmt"
	"strings"
)

// hunkRisk tiers a hunk from its symbols' caller blast radius and export status.
// Thresholds (from the S2 proposal): >=10 callers → medium, >=30 → high, an
// exported symbol bumps one tier. When the graph isn't indexed callers are
// unknown, so risk falls back to export status only and the reason says so.
func hunkRisk(maxCallers int, exported, hadGraph bool) (tier, reason string) {
	tier = "low"
	switch {
	case maxCallers >= reviewCallerHigh:
		tier = "high"
	case maxCallers >= reviewCallerMed:
		tier = "medium"
	}
	if exported {
		tier = bumpTier(tier)
	}

	switch {
	case !hadGraph && exported:
		reason = "exported symbol; callers unknown (graph not indexed)"
	case !hadGraph:
		reason = "callers unknown (graph not indexed)"
	case exported && maxCallers > 0:
		reason = fmt.Sprintf("exported symbol with %d callers", maxCallers)
	case maxCallers > 0:
		reason = fmt.Sprintf("%d callers", maxCallers)
	case exported:
		reason = "exported symbol, no indexed callers"
	default:
		reason = "no exported symbols or callers touched"
	}
	return tier, reason
}

func bumpTier(t string) string {
	switch t {
	case "low":
		return "medium"
	case "medium":
		return "high"
	default:
		return "high"
	}
}

// dropLowRiskHunks keeps only medium/high-risk hunks (the `compact` flag).
func dropLowRiskHunks(hunks []ReviewHunk) []ReviewHunk {
	var out []ReviewHunk
	for _, h := range hunks {
		if h.RiskTier != "low" {
			out = append(out, h)
		}
	}
	return out
}

// ─── time-travel helpers (#644) ──────────────────────────────────────────

// extractNewRef returns the right-hand side of a git range (the new-side ref),
// or "" when it is HEAD/empty (meaning the live index is correct).
func extractNewRef(rng string) string {
	i := strings.LastIndex(rng, "..")
	if i < 0 {
		return ""
	}
	ref := strings.TrimSpace(rng[i+2:])
	if ref == "" || ref == "HEAD" || ref == "@" {
		return ""
	}
	return ref
}
