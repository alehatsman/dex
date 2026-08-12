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
		// Three build-mediated-export unresolved imports into @acme/common
		// (pkg_dir=packages/acme-common): two of @acme/common/Uuid, one of
		// @acme/common/Other.
		{ID: "i:u1", Kind: "import", Name: "@acme/common/Uuid", QualifiedName: "@acme/common/Uuid",
			FilePath: "apps/a/src/x.ts", ContentHash: "u1",
			MetadataJSON: []byte(`{"unresolved":true,"reason":"workspace-subpath","pkg_dir":"packages/acme-common"}`)},
		{ID: "i:u2", Kind: "import", Name: "@acme/common/Uuid", QualifiedName: "@acme/common/Uuid",
			FilePath: "apps/b/src/y.ts", ContentHash: "u2",
			MetadataJSON: []byte(`{"unresolved":true,"reason":"workspace-subpath","pkg_dir":"packages/acme-common"}`)},
		{ID: "i:o1", Kind: "import", Name: "@acme/common/Other", QualifiedName: "@acme/common/Other",
			FilePath: "apps/a/src/z.ts", ContentHash: "o1",
			MetadataJSON: []byte(`{"unresolved":true,"reason":"workspace-subpath","pkg_dir":"packages/acme-common"}`)},
		// A prefix-collision package that must NOT match packages/acme-common.
		{ID: "i:bp", Kind: "import", Name: "@acme/build/Tool", QualifiedName: "@acme/build/Tool",
			FilePath: "apps/a/src/w.ts", ContentHash: "bp",
			MetadataJSON: []byte(`{"unresolved":true,"reason":"workspace-subpath","pkg_dir":"packages/acme"}`)},
		// An alias-unindexed unresolved import (no pkg_dir) — must never appear.
		{ID: "i:al", Kind: "import", Name: "@mui/icons/Add", QualifiedName: "@mui/icons/Add",
			FilePath: "apps/a/src/x.ts", ContentHash: "al",
			MetadataJSON: []byte(`{"unresolved":true,"reason":"alias-unindexed"}`)},
	}
	if err := st.GraphUpsertNodes(ctx, rows, now); err != nil {
		t.Fatal(err)
	}

	// A symbol file inside packages/acme-common sees both specifiers, Uuid first
	// (higher count), and never the prefix-collision or alias imports.
	got, err := st.UnresolvedInboundForFile(ctx, "packages/acme-common/src/UuidCodec.ts", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []UnresolvedInbound{
		{Specifier: "@acme/common/Uuid", Count: 2},
		{Specifier: "@acme/common/Other", Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A file in packages/acme (the prefix-collision package) must see only its
	// own import — proving the `|| '/'` boundary guard works.
	got, err = st.UnresolvedInboundForFile(ctx, "packages/acme/src/main.ts", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Specifier != "@acme/build/Tool" {
		t.Errorf("prefix-collision file: got %v, want only @acme/build/Tool", got)
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
