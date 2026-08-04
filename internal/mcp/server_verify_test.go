package mcp

import (
	"reflect"
	"testing"
)

func TestImpactFiles(t *testing.T) {
	imp := ImpactOutput{
		Targets:    []TargetMatch{{Path: "a.go"}},
		Nodes:      []ImpactNode{{Path: "b.go"}},
		TestsToRun: []string{"a_test.go"},
	}
	got := impactFiles(imp)
	want := []string{"a.go", "b.go", "a_test.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("impactFiles = %v, want %v", got, want)
	}
}
