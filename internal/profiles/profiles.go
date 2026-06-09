// Package profiles loads named context profiles that tune dex's default
// behaviour per task type (explore, bugfix, ci, or custom).
//
// A profile is a small YAML file that overrides a fixed set of knobs.
// Resolution order (first match wins):
//
//  1. $DEX_PROFILE (profile name, not a path) — looked up under:
//     a. <project_root>/.dex/profiles/<name>.yml
//     b. ~/.dex/profiles/<name>.yml
//  2. No active profile → zero-value Profile (no overrides).
//
// The package also provides three built-in profiles that callers can
// embed by name without writing a YAML file.
package profiles

import (
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/alehatsman/dex/internal/tokens"
)

// Profile holds the subset of knobs that context profiles can tune.
// Zero values mean "use the handler's own default".
type Profile struct {
	// TargetModel names the LLM that will consume dex's output. It selects the
	// BPE tokenizer family (so token counts and savings match the real model)
	// and drives aggressiveness defaults. Accepted values: "claude", "gpt",
	// "gemini", "llama", or any model name substring recognised by tokens.Detect.
	// Empty = use the package default (o200k_base, conservative).
	TargetModel string `yaml:"target_model"`

	// Read is applied to file_view (summarize) calls.
	Read struct {
		// DefaultMode overrides the mode field when the caller omits it.
		// Accepted values: "full", "signatures", "map".
		DefaultMode string `yaml:"default_mode"`
	} `yaml:"read"`

	// Compression tunes ctx_shell output density.
	Compression struct {
		// OutputDensity controls how aggressively shell output is compressed.
		// Accepted values: "normal" (default), "tight", "minimal".
		OutputDensity string `yaml:"output_density"`
	} `yaml:"compression"`

	// Budget limits how many resources a single query consumes.
	Budget struct {
		// ContextFraction caps how much of the context window one response
		// may use, expressed as a fraction in (0, 1].  0 = no cap.
		ContextFraction float64 `yaml:"context_fraction"`
		// MaxFiles caps the K parameter for search_semantic / search_symbol.
		// 0 = no cap.
		MaxFiles int `yaml:"max_files"`
	} `yaml:"budget"`
}

// TokenFamily returns the BPE tokenizer family implied by the profile's
// TargetModel field. Falls back to tokens.DefaultFamily when TargetModel is
// empty (i.e. no profile or no target_model set).
func (p Profile) TokenFamily() tokens.Family {
	if p.TargetModel == "" {
		return tokens.DefaultFamily
	}
	return tokens.Detect(p.TargetModel)
}

// builtins holds the hard-coded profiles for the three standard task types.
var builtins = map[string]Profile{
	// claude is the primary consumer profile: Claude Code / Claude API.
	// Selects cl100k_base tokenizer (~3% of Claude's real tokeniser) and
	// enables tight compression — Claude tolerates symmap handles and
	// aggressive entropy pruning well.
	"claude": func() Profile {
		var p Profile
		p.TargetModel = "claude"
		p.Read.DefaultMode = "full"
		p.Compression.OutputDensity = "tight"
		p.Budget.ContextFraction = 0.7
		p.Budget.MaxFiles = 10
		return p
	}(),
	"explore": func() Profile {
		var p Profile
		p.Read.DefaultMode = "full"
		p.Compression.OutputDensity = "normal"
		p.Budget.ContextFraction = 0.8
		p.Budget.MaxFiles = 12
		return p
	}(),
	"bugfix": func() Profile {
		var p Profile
		p.Read.DefaultMode = "full"
		p.Compression.OutputDensity = "tight"
		p.Budget.ContextFraction = 0.6
		p.Budget.MaxFiles = 8
		return p
	}(),
	"ci": func() Profile {
		var p Profile
		p.Read.DefaultMode = "signatures"
		p.Compression.OutputDensity = "minimal"
		p.Budget.ContextFraction = 0.4
		p.Budget.MaxFiles = 5
		return p
	}(),
}

// Load resolves the named profile: built-ins first, then
// <projectRoot>/.dex/profiles/<name>.yml, then ~/.dex/profiles/<name>.yml.
// Returns a zero-value Profile (not nil) when the name is "" or not found.
func Load(name, projectRoot string) Profile {
	if name == "" {
		return Profile{}
	}
	if p, ok := builtins[name]; ok {
		return p
	}
	// Search on disk.
	candidates := []string{
		filepath.Join(projectRoot, ".dex", "profiles", name+".yml"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".dex", "profiles", name+".yml"))
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var p Profile
		if err := yaml.Unmarshal(data, &p); err == nil {
			return p
		}
	}
	return Profile{}
}

var (
	activeOnce    sync.Once
	activeProfile Profile
)

// Active returns the profile named by $DEX_PROFILE, resolved against
// projectRoot. The result is cached after the first call per process.
// Returns a zero-value Profile when $DEX_PROFILE is unset or the named
// profile is not found.
//
// Side effect: on first call, configures the package-level token counter in
// internal/tokens to match the profile's target_model. This ensures that
// token counts reported throughout the server match the real serving model.
func Active(projectRoot string) Profile {
	activeOnce.Do(func() {
		activeProfile = Load(os.Getenv("DEX_PROFILE"), projectRoot)
		tokens.SetDefaultFamily(activeProfile.TokenFamily())
	})
	return activeProfile
}
