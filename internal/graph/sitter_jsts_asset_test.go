package graph

import "testing"

// TestClassifySpecifierAssets locks #157: a JS/TS static-asset import (stylesheet,
// image, font) is not a code module and must classify as external-to-the-code-graph
// — never unresolved. A workspace-prefixed asset like "@scope/ui/theme.css" must
// not become a `workspace-subpath` unresolved with a pkg_dir (that would surface a
// stylesheet as a phantom caller in unresolved_inbound); a relative asset must not
// become `relative` unresolved. The asset check runs before any workspace lookup,
// so an empty engine with a nil workspace exercises it directly.
func TestClassifySpecifierAssets(t *testing.T) {
	e := &jstsBase{lang: "typescript"}

	assets := []string{
		"@scope/ui/theme.css",       // workspace-prefixed stylesheet
		"@scope/ui/icons/logo.svg",  // workspace-prefixed image
		"./styles/app.scss",         // relative stylesheet
		"../fonts/inter.woff2",      // relative font
		"./logo.png?raw",            // bundler query suffix
		"@scope/ui/sprite.svg#icon", // hash fragment suffix
		"some-pkg/reset.css",        // bare-dep-looking asset
	}
	for _, spec := range assets {
		target, class, reason, pkgDir := e.classifySpecifier(spec, "apps/x/src/main.ts")
		if class != specExternal {
			t.Errorf("%q: class = %d, want specExternal (%d)", spec, class, specExternal)
		}
		if target != "" || reason != "" || pkgDir != "" {
			t.Errorf("%q: asset must carry no target/reason/pkg_dir, got target=%q reason=%q pkgDir=%q",
				spec, target, reason, pkgDir)
		}
	}

	// A code specifier with a resolvable-looking extension is NOT an asset — the
	// asset rule must not swallow real code imports (regression guard).
	for _, spec := range []string{"./util", "./util.ts", "@scope/ui/Button.tsx", "react"} {
		_, _, _, pkgDir := e.classifySpecifier(spec, "apps/x/src/main.ts")
		_ = pkgDir // exercised; class assertions live in the fixture test with a real workspace
		if isAssetSpecifier(spec) {
			t.Errorf("%q wrongly detected as an asset", spec)
		}
	}
}
