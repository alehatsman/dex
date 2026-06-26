package review

import (
	"reflect"
	"testing"
)

func TestParseUnified_Modified(t *testing.T) {
	diff := `diff --git a/internal/proxy/proxy.go b/internal/proxy/proxy.go
index 1111111..2222222 100644
--- a/internal/proxy/proxy.go
+++ b/internal/proxy/proxy.go
@@ -220,3 +220,5 @@ func (p *Proxy) Run() error {
 	a := 1
-	old := 2
+	new := 3
+	extra := 4
 	return nil
@@ -300 +302,2 @@ func helper() {
+	added
+	more`

	files := ParseUnified(diff)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Path != "internal/proxy/proxy.go" || f.Status != "modified" || f.OldPath != "" {
		t.Fatalf("file meta wrong: %+v", f)
	}
	if len(f.Hunks) != 2 {
		t.Fatalf("want 2 hunks, got %d", len(f.Hunks))
	}
	h0 := f.Hunks[0]
	if h0.OldStart != 220 || h0.OldLines != 3 || h0.NewStart != 220 || h0.NewLines != 5 {
		t.Errorf("hunk0 range wrong: %+v", h0)
	}
	if h0.Heading != "func (p *Proxy) Run() error {" {
		t.Errorf("hunk0 heading wrong: %q", h0.Heading)
	}
	// Second hunk: omitted old count defaults to 1.
	h1 := f.Hunks[1]
	if h1.OldStart != 300 || h1.OldLines != 1 || h1.NewStart != 302 || h1.NewLines != 2 {
		t.Errorf("hunk1 range wrong: %+v", h1)
	}
}

func TestParseUnified_AddedAndDeleted(t *testing.T) {
	diff := `diff --git a/new.go b/new.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/new.go
@@ -0,0 +1,2 @@
+package x
+// hi
diff --git a/gone.go b/gone.go
deleted file mode 100644
index 4444444..0000000
--- a/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package y
-// bye`

	files := ParseUnified(diff)
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	added := files[0]
	if added.Path != "new.go" || added.Status != "added" {
		t.Errorf("added meta wrong: %+v", added)
	}
	deleted := files[1]
	if deleted.Path != "gone.go" || deleted.Status != "deleted" {
		t.Errorf("deleted meta wrong: %+v", deleted)
	}
	// Deleted file's hunk has NewLines==0 → TouchedLines anchors at NewStart.
	if got := deleted.Hunks[0].TouchedLines(); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("deleted TouchedLines = %v, want [0]", got)
	}
}

func TestParseUnified_Renamed(t *testing.T) {
	diff := `diff --git a/old/name.go b/new/name.go
similarity index 92%
rename from old/name.go
rename to new/name.go
index 5555555..6666666 100644
--- a/old/name.go
+++ b/new/name.go
@@ -10,2 +10,3 @@ func F() {
 	keep := 1
+	add := 2
 	keep2 := 3`

	files := ParseUnified(diff)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Status != "renamed" || f.Path != "new/name.go" || f.OldPath != "old/name.go" {
		t.Fatalf("rename meta wrong: %+v", f)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(f.Hunks))
	}
}

func TestHunk_TouchedLines(t *testing.T) {
	cases := []struct {
		h    Hunk
		want []int
	}{
		{Hunk{NewStart: 220, NewLines: 3}, []int{220, 221, 222}},
		{Hunk{NewStart: 50, NewLines: 1}, []int{50}},
		{Hunk{NewStart: 50, NewLines: 0}, []int{50}}, // pure deletion anchor
	}
	for i, c := range cases {
		if got := c.h.TouchedLines(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("case %d: TouchedLines = %v, want %v", i, got, c.want)
		}
	}
}

func TestParseUnified_Empty(t *testing.T) {
	if got := ParseUnified(""); len(got) != 0 {
		t.Errorf("empty diff → %d files, want 0", len(got))
	}
}

func TestParseUnified_MalformedHunkHeaderSkipped(t *testing.T) {
	diff := `diff --git a/x.go b/x.go
--- a/x.go
+++ b/x.go
@@ this is not a hunk header @@
@@ -1,1 +1,2 @@
 keep
+add`
	files := ParseUnified(diff)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	// Only the valid hunk survives.
	if len(files[0].Hunks) != 1 || files[0].Hunks[0].NewStart != 1 {
		t.Errorf("hunks = %+v, want one valid hunk at NewStart 1", files[0].Hunks)
	}
}
