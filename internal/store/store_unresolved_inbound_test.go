package store

import (
	"testing"
	"time"
)

// TestUnresolvedInboundForFile verifies the #130 attribution query: unresolved
// import nodes carrying pkg_dir are matched to a symbol file in the same package
// (path-prefix, boundary-safe), grouped and counted; a file in a different
// package sees none, and unresolved imports without pkg_dir never participate.
func TestUnresolvedInboundForFile(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	rows := []GraphNodeRow{
		// Three build-mediated-export unresolved imports into @bright/common
		// (pkg_dir=packages/bright-common): two of @bright/common/Uuid, one of
		// @bright/common/Other.
		{ID: "i:u1", Kind: "import", Name: "@bright/common/Uuid", QualifiedName: "@bright/common/Uuid",
			FilePath: "apps/a/src/x.ts", ContentHash: "u1",
			MetadataJSON: []byte(`{"unresolved":true,"reason":"workspace-subpath","pkg_dir":"packages/bright-common"}`)},
		{ID: "i:u2", Kind: "import", Name: "@bright/common/Uuid", QualifiedName: "@bright/common/Uuid",
			FilePath: "apps/b/src/y.ts", ContentHash: "u2",
			MetadataJSON: []byte(`{"unresolved":true,"reason":"workspace-subpath","pkg_dir":"packages/bright-common"}`)},
		{ID: "i:o1", Kind: "import", Name: "@bright/common/Other", QualifiedName: "@bright/common/Other",
			FilePath: "apps/a/src/z.ts", ContentHash: "o1",
			MetadataJSON: []byte(`{"unresolved":true,"reason":"workspace-subpath","pkg_dir":"packages/bright-common"}`)},
		// A prefix-collision package that must NOT match packages/bright-common.
		{ID: "i:bp", Kind: "import", Name: "@bright/build/Tool", QualifiedName: "@bright/build/Tool",
			FilePath: "apps/a/src/w.ts", ContentHash: "bp",
			MetadataJSON: []byte(`{"unresolved":true,"reason":"workspace-subpath","pkg_dir":"packages/bright"}`)},
		// An alias-unindexed unresolved import (no pkg_dir) — must never appear.
		{ID: "i:al", Kind: "import", Name: "@mui/icons/Add", QualifiedName: "@mui/icons/Add",
			FilePath: "apps/a/src/x.ts", ContentHash: "al",
			MetadataJSON: []byte(`{"unresolved":true,"reason":"alias-unindexed"}`)},
	}
	if err := st.GraphUpsertNodes(ctx, rows, now); err != nil {
		t.Fatal(err)
	}

	// A symbol file inside packages/bright-common sees both specifiers, Uuid first
	// (higher count), and never the prefix-collision or alias imports.
	got, err := st.UnresolvedInboundForFile(ctx, "packages/bright-common/src/UuidCodec.ts", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []UnresolvedInbound{
		{Specifier: "@bright/common/Uuid", Count: 2},
		{Specifier: "@bright/common/Other", Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A file in packages/bright (the prefix-collision package) must see only its
	// own import — proving the `|| '/'` boundary guard works.
	got, err = st.UnresolvedInboundForFile(ctx, "packages/bright/src/main.ts", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Specifier != "@bright/build/Tool" {
		t.Errorf("prefix-collision file: got %v, want only @bright/build/Tool", got)
	}

	// A file in an unrelated package sees nothing.
	got, err = st.UnresolvedInboundForFile(ctx, "apps/a/src/x.ts", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("unrelated file: got %v, want none", got)
	}
}
