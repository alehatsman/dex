package main

import (
	"testing"

	"github.com/alehatsman/dex/internal/health"
)

// TestCollectEndpointsLean proves doctor/setup don't panic in the lean profile
// (DEX_EMBED_ENGINE=none): newEmbedClient returns nil, and collectEndpoints
// must report the embed probe as "not configured" rather than dereferencing it
// (#545). Before the fix this nil-deref crashed `dex doctor` and `dex setup`,
// the cold stranger's first commands.
func TestCollectEndpointsLean(t *testing.T) {
	t.Setenv("DEX_EMBED_ENGINE", "none")

	probes := collectEndpoints() // must not panic

	var embed *health.Probe
	for i := range probes {
		if probes[i].Name == "embed" {
			embed = &probes[i]
		}
	}
	if embed == nil {
		t.Fatal("collectEndpoints: no embed probe in lean profile")
	}
	if embed.Health != nil {
		t.Error("lean embed probe must have a nil health func (nothing to probe)")
	}
	if embed.Status != "not configured" {
		t.Errorf("lean embed probe status = %q, want %q", embed.Status, "not configured")
	}
}
