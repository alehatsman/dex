package mcp

import "runtime/debug"

// init recovers a meaningful version for builds that did not inject one via
// -ldflags (e.g. a plain `go install github.com/alehatsman/dex/cmd/dex`). Go
// stamps VCS metadata into the build info, so the binary can report its source
// revision instead of the bare "dev" sentinel. A release build (mooncake task
// install) sets Version explicitly and this fallback is skipped.
func init() {
	if Version != "dev" {
		return
	}
	if v := vcsVersion(); v != "" {
		Version = v
	}
}

// vcsVersion derives a version string from the embedded build info. It prefers
// the module version Go stamps for `go install <path>@<version>` (e.g.
// "v1.0.0"), which is the public install path. When that is absent — a local
// `go build` from a checkout — it falls back to the short VCS revision,
// suffixed with "-dirty" for an uncommitted tree. Returns "" when neither is
// present.
func vcsVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return versionFromBuildInfo(bi)
}

// versionFromBuildInfo is the pure resolution logic, split out for testing.
func versionFromBuildInfo(bi *debug.BuildInfo) string {
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
