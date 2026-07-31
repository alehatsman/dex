package main

import "testing"

func TestEmbedBackendDefaults(t *testing.T) {
	tests := []struct {
		name      string
		isOllama  bool
		vramGB    float64
		wantBatch int
		wantConc  int
	}{
		// ollama: fixed small batch + concurrency on; VRAM is ignored (callers
		// pass 0). Small batch under conc=4 matches the ~16.7 c/s ceiling and
		// protects a conc=1 override from the large-batch collapse.
		{"ollama ignores vram", true, 0, ollamaEmbedBatch, 4},
		{"ollama ignores high vram", true, 40, ollamaEmbedBatch, 4},
		// generic true-batching server: VRAM-sized batch + sequential dispatch
		// (concurrency hurts a saturating server, e.g. bge-large -23%).
		{"generic >16GB", false, 24, 256, 1},
		{"generic 4-16GB", false, 8, 64, 1},
		{"generic <4GB", false, 2, 8, 1},
		{"generic unknown vram", false, 0, 32, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			batch, conc := embedBackendDefaults(tt.isOllama, tt.vramGB)
			if batch != tt.wantBatch || conc != tt.wantConc {
				t.Errorf("embedBackendDefaults(%v, %g) = (batch %d, conc %d); want (batch %d, conc %d)",
					tt.isOllama, tt.vramGB, batch, conc, tt.wantBatch, tt.wantConc)
			}
		})
	}
}
