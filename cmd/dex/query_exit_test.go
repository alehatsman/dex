package main

import (
	"testing"

	"github.com/alehatsman/dex/internal/mcp"
)

// TestQueryFailedEnvelopeStatus locks #857: every lane's Status=="error" must
// fail the process (queryFailed==true), not just kind=check's per-claim
// verification failures — matching the documented exit-code contract (`dex
// help all`: "1 error — runtime ... or usage"). Before the fix, queryFailed
// looked at nothing but out.Result.Check, so a bad regex, a path outside the
// project root, or an unknown --kind all silently exited 0.
func TestQueryFailedEnvelopeStatus(t *testing.T) {
	cases := []struct {
		name string
		out  mcp.QueryOutput
		want bool
	}{
		{"ok envelope", mcp.QueryOutput{Status: "ok"}, false},
		{"not-found envelope is not a failure", mcp.QueryOutput{Status: "not-found"}, false},
		{"no-index envelope is not a failure", mcp.QueryOutput{Status: "no-index"}, false},
		{"use-native-read envelope is not a failure", mcp.QueryOutput{Status: "use-native-read"}, false},
		{"error envelope — bad regex/unknown kind/path outside root/...", mcp.QueryOutput{Status: "error"}, true},
		{
			"check with no failing claims stays ok even if envelope status is ok",
			mcp.QueryOutput{Status: "ok", Result: mcp.QueryResult{Check: &mcp.CheckOutput{
				Results: []mcp.ClaimResult{{Status: "ok"}},
			}}},
			false,
		},
		{
			"check with a failing claim fails even though envelope status is ok",
			mcp.QueryOutput{Status: "ok", Result: mcp.QueryResult{Check: &mcp.CheckOutput{
				Results: []mcp.ClaimResult{{Status: "ok"}, {Status: "gone"}},
			}}},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := queryFailed(tc.out); got != tc.want {
				t.Errorf("queryFailed(%+v) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}
