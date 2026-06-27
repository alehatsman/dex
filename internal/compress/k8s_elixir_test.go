package compress

import (
	"strings"
	"testing"
)

// ── kubectl ──────────────────────────────────────────────────────────────────

func TestCompressKubectl_DropsHealthyPods(t *testing.T) {
	lines := []string{
		"NAME       READY   STATUS    RESTARTS   AGE",
		"web-0      1/1     Running   0          5d",
		"db-0       0/1     Pending   3          1h",
	}
	out := CompressKubectl(lines)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "Running   0") {
		t.Error("healthy Running/0-restart pods should be dropped")
	}
	if !strings.Contains(joined, "Pending") {
		t.Error("problem pod should be retained")
	}
}

func TestCompressKubectl_DeduplicatesLogLines(t *testing.T) {
	ts := "2024-01-02T10:00:00Z "
	lines := []string{
		ts + "connection refused",
		ts + "connection refused",
		ts + "connection refused",
		ts + "different error",
	}
	out := CompressKubectl(lines)
	count := 0
	for _, l := range out {
		if strings.Contains(l, "connection refused") {
			count++
		}
	}
	if count > 1 {
		t.Errorf("duplicate log lines should be deduplicated; got %d", count)
	}
	if !strings.Contains(strings.Join(out, "\n"), "different error") {
		t.Error("distinct line should be retained")
	}
}

// ── helm ─────────────────────────────────────────────────────────────────────

func TestCompressHelm_Template(t *testing.T) {
	lines := []string{
		"---",
		"apiVersion: v1",
		"kind: ConfigMap",
		"---",
		"apiVersion: apps/v1",
		"kind: Deployment",
		"---",
		"apiVersion: apps/v1",
		"kind: Deployment",
	}
	out := CompressHelm("helm template myapp .", lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "ConfigMap") {
		t.Error("ConfigMap kind should appear in summary")
	}
	if !strings.Contains(joined, "Deployment") {
		t.Error("Deployment kind should appear in summary")
	}
	// Should be condensed to a resource count summary, not the raw YAML.
	if strings.Contains(joined, "apiVersion") {
		t.Error("raw YAML should be replaced by summary")
	}
}

func TestCompressHelm_Install(t *testing.T) {
	lines := []string{
		"NAME: my-release",
		"LAST DEPLOYED: Mon Jan  2 10:00:00 2024",
		"NAMESPACE: default",
		"STATUS: deployed",
		"REVISION: 1",
		"",
		"NOTES:",
		"Application deployed successfully.",
		"",
		"Lorem ipsum ... (long notes content that adds noise)",
	}
	out := CompressHelm("helm install my-release ./chart", lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "STATUS:") {
		t.Error("STATUS line should be retained")
	}
}

// ── elixir / mix ─────────────────────────────────────────────────────────────

func TestCompressMix_EmptyOutputReturnsOk(t *testing.T) {
	out := CompressMix("mix compile", []string{"  ", ""})
	if len(out) != 1 || out[0] != "ok" {
		t.Errorf("empty output should return [\"ok\"], got %v", out)
	}
}

func TestCompressMixTest_SummaryExtracted(t *testing.T) {
	lines := []string{
		"",
		"Finished in 0.1 seconds (0.07s async, 0.03s sync)",
		"5 tests, 0 failures",
		"",
	}
	out := CompressMix("mix test", lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "5 tests") {
		t.Errorf("test summary not extracted: %q", joined)
	}
}

func TestCompressMixTest_FailuresListed(t *testing.T) {
	lines := []string{
		"  1) test fails (MyTest)",
		"",
		"3 tests, 1 failure",
	}
	out := CompressMix("mix test", lines)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "1)") {
		t.Errorf("failure entry should be listed: %q", joined)
	}
}

func TestCompressMixDeps_Summary(t *testing.T) {
	lines := []string{
		"* Getting ecto (Hex package)",
		"Resolving ecto ~> 3.10",
		"Compiling 12 files (.ex)",
		"Compiled lib/ecto.ex",
	}
	out := CompressMix("mix deps.get", lines)
	joined := strings.Join(out, "\n")
	// Should condense; not explode with every line.
	if len(out) > len(lines) {
		t.Errorf("deps output should condense; got %d lines from %d", len(out), len(lines))
	}
	_ = joined
}
