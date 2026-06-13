package mcp

import (
	"runtime/debug"
	"testing"
)

func TestVersionFromBuildInfo(t *testing.T) {
	bi := func(mainVer string, settings ...[2]string) *debug.BuildInfo {
		out := &debug.BuildInfo{}
		out.Main.Version = mainVer
		for _, s := range settings {
			out.Settings = append(out.Settings, debug.BuildSetting{Key: s[0], Value: s[1]})
		}
		return out
	}

	tests := []struct {
		name string
		bi   *debug.BuildInfo
		want string
	}{
		{
			name: "module version from go install @v1.0.0 wins",
			bi:   bi("v1.0.0", [2]string{"vcs.revision", "abc123def456789"}),
			want: "v1.0.0",
		},
		{
			name: "devel module version falls through to vcs",
			bi:   bi("(devel)", [2]string{"vcs.revision", "abc123def456789"}),
			want: "abc123def456",
		},
		{
			name: "vcs revision truncated to 12 chars",
			bi:   bi("", [2]string{"vcs.revision", "abc123def456789ghijkl"}),
			want: "abc123def456",
		},
		{
			name: "dirty tree suffixed",
			bi:   bi("", [2]string{"vcs.revision", "abc123"}, [2]string{"vcs.modified", "true"}),
			want: "abc123-dirty",
		},
		{
			name: "no version info yields empty",
			bi:   bi(""),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := versionFromBuildInfo(tt.bi); got != tt.want {
				t.Errorf("versionFromBuildInfo() = %q, want %q", got, tt.want)
			}
		})
	}
}
