package benchcompress

import (
	"fmt"
	"testing"
)

// #448: applyReverseMap must substitute longest key first, else §1 matches
// inside §10 and corrupts the reverse map for any run with ≥10 refs. Map
// iteration order is randomized per range, so loop enough times to reliably
// exercise a colliding order — the fixed implementation is correct every time.
func TestApplyReverseMapMultiDigitRefs(t *testing.T) {
	const n = 13 // §0..§12 — exercises the §1 / §10 prefix collision
	rev := make(map[string]string, n)
	for i := 0; i < n; i++ {
		rev[fmt.Sprintf("§%d", i)] = fmt.Sprintf("R%02d", i) // values carry no § marker
	}

	// Body interleaves multi-digit markers next to their single-digit prefixes.
	body := "§1 §10 §11 §12 §2 §0"
	want := "R01 R10 R11 R12 R02 R00"

	for iter := 0; iter < 300; iter++ {
		if got := applyReverseMap(body, rev); got != want {
			t.Fatalf("iter %d: prefix collision in reverse map\n got:  %q\n want: %q", iter, got, want)
		}
	}
}

// roundTripCheck must report lossless reconstruction through a ≥10-ref legend.
// Values are chosen so R10 != R01+"0" — a prefix collision would mangle §10.
func TestRoundTripCheckTenPlusRefs(t *testing.T) {
	original := "R01 R10 R11 R12 R02 R00"
	compressed := "§MAP:\n" +
		"  §0=R00\n  §1=R01\n  §2=R02\n  §10=R10\n  §11=R11\n  §12=R12\n" +
		"\n" +
		"§1 §10 §11 §12 §2 §0"
	if !roundTripCheck("symmap", original, compressed) {
		t.Fatal("roundTripCheck reported loss on a lossless ≥10-ref legend")
	}
}
