package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/alehatsman/dex/internal/embed"
)

var ensureOllamaOnce sync.Once

// ensureOllamaRunning best-effort starts a local ollama daemon when it is
// installed but not listening — the common "I have ollama but forgot to start
// it" failure. It runs at most once per process and is a no-op when:
//   - DEX_NO_AUTO_OLLAMA is set (explicit opt-out),
//   - ollama is already reachable, or
//   - the `ollama` binary is not on PATH (nothing to start; the actionable
//     embedding-service-unreachable error then guides the operator).
//
// Best-effort by design: any failure is logged to stderr, never fatal — dex
// continues in degraded (BM25 + symbol + graph) mode.
func ensureOllamaRunning() {
	ensureOllamaOnce.Do(func() {
		if os.Getenv("DEX_NO_AUTO_OLLAMA") != "" {
			return
		}
		ctx := context.Background()
		if _, ok := embed.ScanOllama(ctx); ok {
			return // already up
		}
		bin, err := exec.LookPath("ollama")
		if err != nil {
			return // not installed
		}
		fmt.Fprintln(os.Stderr, "dex: ollama installed but not running — starting `ollama serve` (set DEX_NO_AUTO_OLLAMA=1 to disable)")
		cmd := exec.Command(bin, "serve")
		// Detach into its own process group so the daemon outlives this
		// (possibly short-lived) dex process.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "dex: could not start ollama: %v\n", err)
			return
		}
		_ = cmd.Process.Release()

		// Poll for readiness — bounded so startup never hangs.
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if _, ok := embed.ScanOllama(ctx); ok {
				fmt.Fprintln(os.Stderr, "dex: ollama is up")
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
		fmt.Fprintln(os.Stderr, "dex: ollama did not become ready in time — continuing in degraded mode")
	})
}
