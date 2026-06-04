package embed

import (
	"bytes"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// FreeVRAMGB returns an estimate of available GPU VRAM in gigabytes.
// Returns 0 when no GPU is detected or the probe fails — callers treat 0 as
// "unknown" and should fall back to conservative defaults.
//
// Detection strategy:
//   - Linux/Windows: nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits (reports MiB)
//   - macOS: system_profiler SPDisplaysDataType — parses "VRAM (Total): N MB/GB"
func FreeVRAMGB() float64 {
	switch runtime.GOOS {
	case "linux", "windows":
		return nvidiaSMIFreeGB()
	case "darwin":
		return macVRAMGB()
	}
	return 0
}

// BatchSizeForVRAM returns a suggested embedding batch size based on available
// VRAM. When vramGB is 0 (unknown), the fallback is returned.
//
// Thresholds:
//   - >16 GB  → 256
//   - 4–16 GB → 64
//   - >0 GB   → 8
//   - 0 (unknown) → fallback
func BatchSizeForVRAM(vramGB float64, fallback int) int {
	switch {
	case vramGB > 16:
		return 256
	case vramGB >= 4:
		return 64
	case vramGB > 0:
		return 8
	default:
		return fallback
	}
}

func nvidiaSMIFreeGB() float64 {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=memory.free",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0
	}
	// Each line is free MiB for one GPU; take the max.
	var maxMiB float64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		mib, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
		if err != nil {
			continue
		}
		if mib > maxMiB {
			maxMiB = mib
		}
	}
	return maxMiB / 1024
}

func macVRAMGB() float64 {
	out, err := exec.Command("system_profiler", "SPDisplaysDataType").Output()
	if err != nil {
		return 0
	}
	// Look for "VRAM (Total):" or "VRAM (Dynamic, Max):"
	for _, line := range bytes.Split(out, []byte("\n")) {
		lower := strings.ToLower(string(line))
		if !strings.Contains(lower, "vram") {
			continue
		}
		// Extract the first number on the line.
		parts := strings.Fields(string(line))
		for i, p := range parts {
			v, err := strconv.ParseFloat(p, 64)
			if err != nil {
				continue
			}
			unit := ""
			if i+1 < len(parts) {
				unit = strings.ToLower(strings.Trim(parts[i+1], ",:()"))
			}
			switch unit {
			case "gb":
				return v
			case "mb":
				return v / 1024
			}
		}
	}
	return 0
}
