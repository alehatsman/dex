package profiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alehatsman/dex/internal/tokens"
)

func TestTokenFamilyZeroProfile(t *testing.T) {
	var p Profile
	if got := p.TokenFamily(); got != tokens.DefaultFamily {
		t.Errorf("zero Profile.TokenFamily() = %v, want %v (DefaultFamily)", got, tokens.DefaultFamily)
	}
}

func TestTokenFamilyClaude(t *testing.T) {
	p := Profile{TargetModel: "claude"}
	if got := p.TokenFamily(); got != tokens.Cl100k {
		t.Errorf("claude target_model TokenFamily() = %v, want %v", got, tokens.Cl100k)
	}
}

func TestStrictAnchors(t *testing.T) {
	weak := []string{"qwen2.5-coder:7b", "deepseek-coder", "llama3.1:8b", "mistral"}
	for _, m := range weak {
		if !(Profile{TargetModel: m}).StrictAnchors() {
			t.Errorf("StrictAnchors(%q) = false, want true (weak local model)", m)
		}
	}
	frontier := []string{"claude", "gpt-4o", "gemini-2.0-flash", ""}
	for _, m := range frontier {
		if (Profile{TargetModel: m}).StrictAnchors() {
			t.Errorf("StrictAnchors(%q) = true, want false (frontier/default)", m)
		}
	}
}

func TestTokenFamilyGemini(t *testing.T) {
	p := Profile{TargetModel: "gemini-2.0-flash"}
	if got := p.TokenFamily(); got != tokens.Gemini {
		t.Errorf("gemini target_model TokenFamily() = %v, want %v", got, tokens.Gemini)
	}
}

func TestBuiltinClaude(t *testing.T) {
	p := Load("claude", "")
	if p.TargetModel != "claude" {
		t.Errorf("claude builtin: TargetModel = %q, want %q", p.TargetModel, "claude")
	}
	if p.Compression.OutputDensity != "tight" {
		t.Errorf("claude builtin: OutputDensity = %q, want %q", p.Compression.OutputDensity, "tight")
	}
	if p.Budget.MaxFiles != 10 {
		t.Errorf("claude builtin: MaxFiles = %d, want %d", p.Budget.MaxFiles, 10)
	}
	if p.TokenFamily() != tokens.Cl100k {
		t.Errorf("claude builtin: TokenFamily() = %v, want %v", p.TokenFamily(), tokens.Cl100k)
	}
}

func TestBuiltinExploreUnchanged(t *testing.T) {
	p := Load("explore", "")
	if p.Read.DefaultMode != "full" {
		t.Errorf("explore: DefaultMode = %q, want %q", p.Read.DefaultMode, "full")
	}
	// explore has no target_model set; falls back to DefaultFamily
	if p.TokenFamily() != tokens.DefaultFamily {
		t.Errorf("explore: TokenFamily() = %v, want %v", p.TokenFamily(), tokens.DefaultFamily)
	}
}

func TestTargetModelYAMLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	profilesDir := filepath.Join(dir, ".dex", "profiles")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yml := []byte("target_model: claude\nread:\n  default_mode: signatures\n")
	if err := os.WriteFile(filepath.Join(profilesDir, "myprofile.yml"), yml, 0o644); err != nil {
		t.Fatal(err)
	}
	p := Load("myprofile", dir)
	if p.TargetModel != "claude" {
		t.Errorf("YAML round-trip: TargetModel = %q, want %q", p.TargetModel, "claude")
	}
	if p.Read.DefaultMode != "signatures" {
		t.Errorf("YAML round-trip: DefaultMode = %q, want %q", p.Read.DefaultMode, "signatures")
	}
	if p.TokenFamily() != tokens.Cl100k {
		t.Errorf("YAML round-trip: TokenFamily() = %v, want %v", p.TokenFamily(), tokens.Cl100k)
	}
}
