package main

import "testing"

func TestRangeText(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}
	cases := []struct {
		name       string
		start, end int
		want       string
	}{
		{"no range", 0, 0, "a\nb\nc\nd\ne"},
		{"start only", 3, 0, "c\nd\ne"},
		{"end only", 0, 2, "a\nb"},
		{"both", 2, 4, "b\nc\nd"},
		{"single line", 3, 3, "c"},
		{"end past eof clamps", 4, 99, "d\ne"},
		{"start past eof empty", 99, 0, ""},
		{"start beyond end empty", 4, 2, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rangeText(lines, c.start, c.end); got != c.want {
				t.Errorf("rangeText(%d,%d) = %q, want %q", c.start, c.end, got, c.want)
			}
		})
	}
}
