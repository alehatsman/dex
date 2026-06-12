package main

import (
	"reflect"
	"testing"
)

func TestSplitTraceArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantDir string
		wantFwd []string
		help    bool
		wantErr bool
	}{
		{"default callers", []string{"Foo"}, "callers", []string{"Foo"}, false, false},
		{"path with cwd", []string{"--dir", "callees", "Foo"}, "callees", []string{"Foo"}, false, false},
		{"direction long", []string{"Foo", "--direction", "path", "--to", "Bar"}, "path", []string{"Foo", "Bar"}, false, false},
		{"direction equals", []string{"--direction=callees", "Foo"}, "callees", []string{"Foo"}, false, false},
		{"dir equals + to equals", []string{"A", "--dir=path", "--to=B"}, "path", []string{"A", "B"}, false, false},
		{"short -d", []string{"-d", "callees", "Foo"}, "callees", []string{"Foo"}, false, false},
		{"forwards passthrough flags", []string{"Foo", "-k", "5", "--package", "x"}, "callers", []string{"Foo", "-k", "5", "--package", "x"}, false, false},
		{"unknown dir passes through to dispatch", []string{"--dir", "sideways", "Foo"}, "sideways", []string{"Foo"}, false, false},
		{"help flag", []string{"-h"}, "callers", []string{}, true, false},
		{"dangling --dir errors", []string{"Foo", "--dir"}, "", nil, false, true},
		{"dangling --to errors", []string{"Foo", "--to"}, "", nil, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, fwd, help, err := splitTraceArgs(c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if dir != c.wantDir {
				t.Errorf("dir=%q, want %q", dir, c.wantDir)
			}
			if help != c.help {
				t.Errorf("help=%v, want %v", help, c.help)
			}
			if !reflect.DeepEqual(fwd, c.wantFwd) {
				t.Errorf("fwd=%v, want %v", fwd, c.wantFwd)
			}
		})
	}
}
