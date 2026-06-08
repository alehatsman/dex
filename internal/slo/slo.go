// Package slo implements session-level service level objective (SLO)
// monitoring for dex. It tracks per-project token usage, tool call counts,
// and compression ratios, then evaluates configurable thresholds to emit
// warn, throttle, or block signals.
//
// The Tracker is a process-local singleton per project root — callers obtain
// one via [ForProject]. Counters reset when the dex process restarts; they
// intentionally do not persist to disk (a session boundary).
//
// Config lives in .dex/config.yml under the `slo:` key and is read once per
// [ForProject] call (hot-reload is intentionally not supported — restart dex
// to pick up SLO config changes; the overhead does not justify the complexity).
//
// Violation debouncing: once a threshold fires, the same SLO will not fire
// again for the next [debounceWindow] to avoid flooding the agent with
// repeated annotations on every tool call.
package slo

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

const debounceWindow = 30 * time.Second

// Action is what happens when an SLO threshold is crossed.
type Action string

const (
	// ActionWarn appends a "[SLO: …]" annotation to the tool response.
	ActionWarn Action = "warn"
	// ActionThrottle forces the next file_view call to use a cheaper mode.
	ActionThrottle Action = "throttle"
	// ActionBlock refuses the tool call with an error response.
	ActionBlock Action = "block"
)

// Metric names understood by Config.
const (
	MetricContextTokens = "context_tokens" // total output tokens returned across all tool calls
	MetricToolCalls     = "tool_calls"     // total MCP tool calls this session
	MetricShellCalls    = "shell_calls"    // ctx_shell invocations
)

// SLOEntry is one threshold entry from .dex/config.yml.
type SLOEntry struct {
	// Name is a human-readable label used in annotations. Required.
	Name string `yaml:"name"`
	// Metric is one of the MetricXxx constants. Required.
	Metric string `yaml:"metric"`
	// Threshold is the numeric limit. Crossed means metric >= threshold (max
	// direction) or metric <= threshold (min direction).
	Threshold float64 `yaml:"threshold"`
	// Direction is "max" (default) or "min".
	Direction string `yaml:"direction"`
	// Action is "warn" (default), "throttle", or "block".
	Action Action `yaml:"action"`
	// Percent, when > 0, fires a warn at this percentage of Threshold (0–100).
	// Useful for early warnings (e.g. Percent=80 fires at 80 % of the hard limit).
	Percent float64 `yaml:"percent"`
}

// Config holds the SLO configuration loaded from .dex/config.yml.
type Config struct {
	SLOs []SLOEntry `yaml:"slo"`
}

// dexConfigSLO is the subset of .dex/config.yml the slo package reads.
type dexConfigSLO struct {
	SLO []SLOEntry `yaml:"slo"`
}

// LoadConfig reads the slo: section of .dex/config.yml under root.
// A missing file or missing slo: key returns an empty Config (no SLOs = no-op).
func LoadConfig(root string) (Config, error) {
	path := filepath.Join(root, ".dex", "config.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("slo: read %s: %w", path, err)
	}
	var f dexConfigSLO
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return Config{}, fmt.Errorf("slo: parse %s: %w", path, err)
	}
	return Config{SLOs: f.SLO}, nil
}

// Snapshot is a read-consistent view of the tracker counters.
type Snapshot struct {
	ContextTokens uint64
	ToolCalls     uint64
	ShellCalls    uint64
}

// Violation is a single SLO breach.
type Violation struct {
	// SLO is the entry that fired.
	SLO SLOEntry
	// Current is the observed metric value.
	Current float64
	// Limit is the effective threshold that was crossed.
	Limit float64
	// Warning is true when this is a percent-based early warning (not the
	// hard limit yet).
	Warning bool
}

// Annotation returns the inline text appended to a tool response for a warn
// or throttle violation.
func (v Violation) Annotation() string {
	if v.Warning {
		return fmt.Sprintf("[SLO: %s at %.0f%% of limit (%.0f / %.0f)]",
			v.SLO.Name, v.SLO.Percent, v.Current, v.Limit)
	}
	return fmt.Sprintf("[SLO: %s limit reached (%.0f / %.0f)]",
		v.SLO.Name, v.Current, v.Limit)
}

// BlockMessage returns the error message returned when action=block fires.
func (v Violation) BlockMessage() string {
	return fmt.Sprintf("SLO block: %s — limit %.0f reached (current %.0f). "+
		"Use ctx_session action=clear to reset the session counter.",
		v.SLO.Name, v.Limit, v.Current)
}

// Tracker accumulates per-session metrics for one project and evaluates SLOs.
type Tracker struct {
	contextTokens atomic.Uint64
	toolCalls     atomic.Uint64
	shellCalls    atomic.Uint64

	cfg Config

	mu          sync.Mutex
	lastFired   map[string]time.Time // SLO name → last time it fired (for debounce)
	throttleSet bool                 // true when throttle action fired and not yet consumed
}

// newTracker creates a Tracker with the given config.
func newTracker(cfg Config) *Tracker {
	return &Tracker{
		cfg:       cfg,
		lastFired: make(map[string]time.Time),
	}
}

// RecordTokens adds n output tokens to the running total.
func (t *Tracker) RecordTokens(n int) {
	if n > 0 {
		t.contextTokens.Add(uint64(n))
	}
}

// RecordToolCall increments the tool call counter.
func (t *Tracker) RecordToolCall() {
	t.toolCalls.Add(1)
}

// RecordShellCall increments the shell call counter.
func (t *Tracker) RecordShellCall() {
	t.shellCalls.Add(1)
}

// ConsumeThrottle returns true (and clears the flag) if a throttle violation
// is pending. Callers use this to force a cheaper summarize mode.
func (t *Tracker) ConsumeThrottle() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.throttleSet {
		t.throttleSet = false
		return true
	}
	return false
}

// Snapshot returns the current counter values.
func (t *Tracker) Snapshot() Snapshot {
	return Snapshot{
		ContextTokens: t.contextTokens.Load(),
		ToolCalls:     t.toolCalls.Load(),
		ShellCalls:    t.shellCalls.Load(),
	}
}

// Check evaluates all configured SLOs against the current snapshot and
// returns any violations. Violations that have fired within the last
// [debounceWindow] are suppressed to avoid flooding.
func (t *Tracker) Check() []Violation {
	if len(t.cfg.SLOs) == 0 {
		return nil
	}
	snap := t.Snapshot()
	now := time.Now()
	var out []Violation

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, slo := range t.cfg.SLOs {
		current := t.metricValue(snap, slo.Metric)
		dir := slo.Direction
		if dir == "" {
			dir = "max"
		}
		action := slo.Action
		if action == "" {
			action = ActionWarn
		}

		// Hard threshold check.
		crossed := (dir == "max" && current >= slo.Threshold) ||
			(dir == "min" && current <= slo.Threshold)

		// Percent warning check (fires before the hard limit, warn only).
		var warnCrossed bool
		var warnLimit float64
		if slo.Percent > 0 && slo.Percent < 100 && dir == "max" {
			warnLimit = slo.Threshold * slo.Percent / 100
			warnCrossed = current >= warnLimit && !crossed
		}

		if !crossed && !warnCrossed {
			continue
		}

		// Key for debounce includes whether it's a warning or hard breach.
		key := slo.Name
		if warnCrossed {
			key += ":warn"
		}
		if last, ok := t.lastFired[key]; ok && now.Sub(last) < debounceWindow {
			continue
		}
		t.lastFired[key] = now

		effectiveLimit := slo.Threshold
		if warnCrossed {
			effectiveLimit = warnLimit
		}

		v := Violation{
			SLO:     slo,
			Current: current,
			Limit:   effectiveLimit,
			Warning: warnCrossed,
		}
		out = append(out, v)

		// Set throttle flag immediately when that action fires.
		if !warnCrossed && action == ActionThrottle {
			t.throttleSet = true
		}
	}
	return out
}

// metricValue maps a metric name to the current counter value.
func (t *Tracker) metricValue(snap Snapshot, metric string) float64 {
	switch metric {
	case MetricContextTokens:
		return float64(snap.ContextTokens)
	case MetricToolCalls:
		return float64(snap.ToolCalls)
	case MetricShellCalls:
		return float64(snap.ShellCalls)
	default:
		return 0
	}
}

// registry is the process-global map of project root → *Tracker.
var (
	registryMu sync.Mutex
	registry   = make(map[string]*Tracker)
)

// ForProject returns (or creates) the Tracker for the given project root.
// Config is loaded from .dex/config.yml on first creation; subsequent calls
// for the same root return the existing tracker (config is not re-read).
// Errors loading config are logged to stderr and a no-op tracker is returned.
func ForProject(root string) *Tracker {
	registryMu.Lock()
	defer registryMu.Unlock()
	if t, ok := registry[root]; ok {
		return t
	}
	cfg, err := LoadConfig(root)
	if err != nil {
		// Config errors are non-fatal; run with no SLOs rather than breaking
		// every tool call.
		fmt.Fprintf(os.Stderr, "dex slo: config error for %s: %v\n", root, err)
		cfg = Config{}
	}
	t := newTracker(cfg)
	registry[root] = t
	return t
}
