package compress

import (
	"fmt"
	"strings"
	"testing"
)

func TestIsControlFlowLine(t *testing.T) {
	pos := []string{
		"if x > 0 {",
		"  return nil",
		"return",
		"} else {",
		"} else if y {",
		"for i := range xs {",
		"while (n > 0) {",
		"switch x {",
		"case 1:",
		"default:",
		"defer cleanup()",
		"break",
		"continue",
		"} catch (e) {",
		"} finally {",
		"guard let x = y else {",
		"raise ValueError",
		"elif x:",
		"throw new Error()",
	}
	for _, l := range pos {
		if !isControlFlowLine(l) {
			t.Errorf("isControlFlowLine(%q) = false, want true", l)
		}
	}

	neg := []string{
		"format(x)",
		"iffy := true",
		"x := compute()",
		"returnValue := 3",
		"// if this is a comment",
		"continueFlag = false",
		"result := forEach()",
		"caseload += 1",
	}
	for _, l := range neg {
		if isControlFlowLine(l) {
			t.Errorf("isControlFlowLine(%q) = true, want false", l)
		}
	}
}

// TestDropLowEntropyLines_ProtectsControlFlow is the #540 regression: the
// aggressive (relaxed) entropy pass must never drop a control-flow line on
// low-novelty grounds — dropping a repeated `if`/`return` silently rewrites
// the logic, the "lossless"-but-lossy behavior reported in the issue.
func TestDropLowEntropyLines_ProtectsControlFlow(t *testing.T) {
	var lines []string
	// Distinct filler builds up the `seen` trigram set so the repeated
	// control-flow lines below score as zero-novelty (drop candidates).
	for i := 0; i < 8; i++ {
		lines = append(lines, fmt.Sprintf("alpha%d := beta%d + gamma%d", i, i, i))
	}
	for i := 0; i < 6; i++ {
		lines = append(lines, "if err != nil {")
		lines = append(lines, "return err")
		lines = append(lines, "}")
	}

	got := dropLowEntropyLines(lines, EntropyThresholdMax)
	joined := strings.Join(got, "\n")

	// All 6 repeated control-flow statements must survive — none dropped on
	// entropy grounds (without the guard, only the first novel copy survives).
	if n := strings.Count(joined, "if err != nil {"); n != 6 {
		t.Errorf("want 6 `if` lines kept, got %d:\n%s", n, joined)
	}
	if n := strings.Count(joined, "return err"); n != 6 {
		t.Errorf("want 6 `return` lines kept, got %d:\n%s", n, joined)
	}
}
