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

// OllamaEmbedBatch is the default chunks-per-request for auto-detected ollama.
// Pinned by a reindex grid on qwen3-embedding:0.6b (#77 §4): under the default
// concurrency 4, batch 16 and 128 tie at the GPU-bound throughput ceiling, so
// 16 is the smaller of the two co-optima (less VRAM, safer conc=1 fallback).
const OllamaEmbedBatch = 16

// BackendDefaults picks the default embed batch size and client concurrency for
// the resolved backend (#77 §4). For auto-detected ollama the dominant lever is
// *concurrency*, not batch size: a reindex grid on qwen3-embedding:0.6b showed
// concurrency=4 flattens the large-batch collapse and pins throughput at a
// ~16.7 c/s GPU-bound ceiling for batch 16 and 128 alike, while sequential
// dispatch is batch-sensitive (7.8 c/s @128 → 14.7 @8) and always slower. So
// ollama gets concurrency 4 (the lever #96 made live) plus a small-ish batch
// (16) — chosen to cap VRAM and keep a conc=1 override off the collapse, not for
// throughput. A true-batching server (infinity/TEI/vLLM) saturates on a large
// batch and is *hurt* by client concurrency (bge-large −23% at conc=4), so it
// gets a VRAM-sized batch and sequential dispatch — which also matches the
// effective pre-#96 chunk-pass behaviour for explicit-URL deployments.
// DEX_EMBED_BATCH / DEX_EMBED_CONCURRENCY override both. vramGB is only consulted
// for the non-ollama path (pass 0 for ollama).
func BackendDefaults(isOllama bool, vramGB float64) (batch, conc int) {
	if isOllama {
		return OllamaEmbedBatch, 4
	}
	batch = 32
	if vramGB > 0 {
		batch = BatchSizeForVRAM(vramGB, 32)
	}
	return batch, 1
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
