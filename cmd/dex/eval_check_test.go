package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/eval"
)

func writeRefReport(t *testing.T, rep eval.Report) string {
	t.Helper()
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func manifest() *eval.EvalManifest {
	return &eval.EvalManifest{
		SchemaVersion: eval.ManifestSchemaVersion, GoldenMode: "git-history",
		QuerySetSHA256: "corpus-A", Lane: "full", EmbedModel: "m", EmbedDim: 2560,
		FusionMode: "linear", FusionAlpha: 0.7, GraphWeight: 1.0, K: 10,
	}
}

func TestCheckEvalRegression_ManifestGate(t *testing.T) {
	ref := eval.Report{K: 10, N: 40, MeanNDCG: 0.6, MeanRecall: 0.7, MRR: 0.75, Manifest: manifest()}
	good := ref // identical metrics + manifest

	// Compatible + no drop → pass.
	if err := checkEvalRegression(good, writeRefReport(t, ref), false); err != nil {
		t.Fatalf("compatible run failed: %v", err)
	}

	// Incompatible (k differs) → fail closed.
	bad := good
	bm := *manifest()
	bm.K = 20
	bad.Manifest = &bm
	err := checkEvalRegression(bad, writeRefReport(t, ref), false)
	if err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("expected incompatible-manifest error, got %v", err)
	}

	// Same mismatch but override set → proceeds (metrics ok) → pass.
	if err := checkEvalRegression(bad, writeRefReport(t, ref), true); err != nil {
		t.Fatalf("--allow-incompatible should permit comparison: %v", err)
	}

	// Reference predates manifests → warn and compare metrics only.
	legacy := eval.Report{K: 10, N: 40, MeanNDCG: 0.6, MeanRecall: 0.7, MRR: 0.75}
	if err := checkEvalRegression(good, writeRefReport(t, legacy), false); err != nil {
		t.Fatalf("manifest-less reference should fall back to metric-only: %v", err)
	}
}

func TestCheckEvalRegression_PerTypeGate(t *testing.T) {
	ref := eval.Report{
		K: 10, N: 40, MeanNDCG: 0.6, MeanRecall: 0.7, MRR: 0.75, Manifest: manifest(),
		ByType: map[string]eval.Report{"symbol": {N: 10, MeanNDCG: 0.8, MeanRecall: 0.85, MRR: 0.9}},
	}
	// Flat aggregate, but the symbol bucket sinks → must fail despite compatible manifest.
	now := eval.Report{
		K: 10, N: 40, MeanNDCG: 0.6, MeanRecall: 0.7, MRR: 0.75, Manifest: manifest(),
		ByType: map[string]eval.Report{"symbol": {N: 10, MeanNDCG: 0.50, MeanRecall: 0.85, MRR: 0.9}},
	}
	err := checkEvalRegression(now, writeRefReport(t, ref), false)
	if err == nil || !strings.Contains(err.Error(), "symbol/NDCG@k") {
		t.Fatalf("expected per-type symbol/NDCG@k regression, got %v", err)
	}
}
