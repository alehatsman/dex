package retrieve

import (
	"reflect"
	"testing"
)

// TestContextPackShape pins the domain schema (docs/design/95-context-pack.md
// §2) the way #93 pins the MCP wire contract: an accidental field rename or
// removal breaks the build here, forcing an intentional spec update. When the
// schema legitimately changes, edit the want lists below alongside the doc.
func TestContextPackShape(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		want []string
	}{
		{
			name: "ContextPack",
			typ:  reflect.TypeOf(ContextPack{}),
			want: []string{
				"Selection", // embedded currency (#95f) — carries Refs/Trust/Stages/Budget
				"Intent", "Question",
				"Symbols", "SemanticHits", "SuggestedReads", "Graph", "References", "Annotations", "RelatedFiles",
				"Concerns",
				"NextAction", "Avoid",
				"ContentBytesInlined", "Expanded",
			},
		},
		{
			name: "Selection",
			typ:  reflect.TypeOf(Selection{}),
			want: []string{"Refs", "Trust", "Stages", "Budget"},
		},
		{
			name: "Ref",
			typ:  reflect.TypeOf(Ref{}),
			want: []string{"Kind", "ID", "Path", "Span", "Prov", "Score", "Meta"},
		},
		{
			name: "Trust",
			typ:  reflect.TypeOf(Trust{}),
			want: []string{
				"Stale", "Indexing", "IndexedAt",
				"TopScore", "LowConf", "Confidence",
				"GraphResolved", "RecallPartial", "Caveat",
			},
		},
		{
			name: "Concerns",
			typ:  reflect.TypeOf(Concerns{}),
			want: []string{"Covered", "Dropped"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := exportedFields(c.typ)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s fields drifted from the spec:\n got:  %v\n want: %v", c.name, got, c.want)
			}
		})
	}
}

// TestContextPackIsWireFree guards the domain/wire split: the L2 pack must carry
// no json struct tags. A tag here means a wire concern leaked down into the
// retrieval engine — exactly the inversion #95 §6.1 forbids.
func TestContextPackIsWireFree(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf(ContextPack{}),
		reflect.TypeOf(Selection{}),
		reflect.TypeOf(Ref{}),
		reflect.TypeOf(Trust{}),
		reflect.TypeOf(Concerns{}),
		reflect.TypeOf(SymbolHit{}),
		reflect.TypeOf(RefHit{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if tag, ok := f.Tag.Lookup("json"); ok {
				t.Errorf("%s.%s carries a json tag %q — domain types are wire-free", typ.Name(), f.Name, tag)
			}
		}
	}
}

func exportedFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		if f := t.Field(i); f.IsExported() {
			out = append(out, f.Name)
		}
	}
	return out
}
