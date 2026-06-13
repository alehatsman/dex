package corpus

import (
	"os"
	"path/filepath"
	"testing"
)

const validSHA = "2efaec117637fa51c6c53588899002def7eee37a"

func goodRepo() RepoSpec {
	return RepoSpec{
		Name:      "flask",
		URL:       "https://github.com/pallets/flask",
		Commit:    validSHA,
		Languages: []string{"python"},
		QuerySets: []string{"benchmark/corpus/queries/flask.json"},
	}
}

func TestValidate(t *testing.T) {
	mutate := func(f func(*RepoSpec)) Manifest {
		r := goodRepo()
		f(&r)
		return Manifest{Repos: []RepoSpec{r}}
	}

	tests := []struct {
		name    string
		m       Manifest
		wantErr bool
	}{
		{"valid curated", Manifest{Repos: []RepoSpec{goodRepo()}}, false},
		{"valid gen-only", mutate(func(r *RepoSpec) {
			r.QuerySets = nil
			r.Gen.GitHistory = GenSpec{Enabled: true}
		}), false},
		{"valid structural-only", mutate(func(r *RepoSpec) {
			r.QuerySets = nil
			r.Gen.Structural = GenSpec{Enabled: true}
		}), false},
		{"empty", Manifest{}, true},
		{"bad name", mutate(func(r *RepoSpec) { r.Name = "Flask Repo" }), true},
		{"empty name", mutate(func(r *RepoSpec) { r.Name = "" }), true},
		{"non-http url", mutate(func(r *RepoSpec) { r.URL = "git@github.com:pallets/flask" }), true},
		{"short sha", mutate(func(r *RepoSpec) { r.Commit = "2efaec1" }), true},
		{"non-hex sha", mutate(func(r *RepoSpec) { r.Commit = "zzzaec117637fa51c6c53588899002def7eee37a" }), true},
		{"no language", mutate(func(r *RepoSpec) { r.Languages = nil }), true},
		{"no query source", mutate(func(r *RepoSpec) { r.QuerySets = nil }), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.m.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateDuplicateName(t *testing.T) {
	m := Manifest{Repos: []RepoSpec{goodRepo(), goodRepo()}}
	if err := m.Validate(); err == nil {
		t.Fatal("expected duplicate-name error, got nil")
	}
}

func TestLoadManifest(t *testing.T) {
	yml := `repos:
  - name: flask
    url: https://github.com/pallets/flask
    commit: 2efaec117637fa51c6c53588899002def7eee37a
    languages: [python]
    query_sets:
      - benchmark/corpus/queries/flask.json
    gen:
      git_history: { enabled: true, max_commits: 400, max_files: 6 }
      blast_radius: { enabled: false }
`
	dir := t.TempDir()
	path := filepath.Join(dir, "repos.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(m.Repos) != 1 {
		t.Fatalf("repos = %d, want 1", len(m.Repos))
	}
	r := m.Repos[0]
	if r.Name != "flask" || r.Commit != validSHA {
		t.Errorf("unexpected repo: %+v", r)
	}
	if !r.Gen.GitHistory.Enabled || r.Gen.GitHistory.MaxCommits != 400 || r.Gen.GitHistory.MaxFiles != 6 {
		t.Errorf("git_history gen not parsed: %+v", r.Gen.GitHistory)
	}
	if r.Gen.BlastRadius.Enabled {
		t.Errorf("blast_radius should be disabled: %+v", r.Gen.BlastRadius)
	}
}

func TestLoadManifestMissing(t *testing.T) {
	if _, err := LoadManifest(filepath.Join(t.TempDir(), "nope.yml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
