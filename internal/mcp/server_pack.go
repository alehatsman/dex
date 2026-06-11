package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// PackInput is the request schema for the ctx_pack tool.
type PackInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Action      string `json:"action"                 jsonschema:"create | install | list | info | export | import | auto_load"`
	Name        string `json:"name,omitempty"         jsonschema:"package name (required for create, install, info, export, auto_load)"`
	Path        string `json:"path,omitempty"         jsonschema:"file path to import from or export to (required for import; optional for export — defaults to <name>.ctxpkg in current dir)"`
}

// ctxPkgKnowledgeFact is a serialized knowledge fact in a .ctxpkg bundle.
type ctxPkgKnowledgeFact struct {
	Archetype  string  `json:"archetype"`
	Body       string  `json:"body"`
	Confidence float64 `json:"confidence"`
}

// ctxPkgSession is the serialized session state.
type ctxPkgSession struct {
	Task  string `json:"task,omitempty"`
	Notes string `json:"notes,omitempty"`
}

// ctxPkgLayers holds the five content layers of a context package.
type ctxPkgLayers struct {
	Knowledge []ctxPkgKnowledgeFact `json:"knowledge"`
	Session   ctxPkgSession         `json:"session"`
	Patterns  []ctxPkgKnowledgeFact `json:"patterns"`
	Gotchas   []ctxPkgKnowledgeFact `json:"gotchas"`
	Graph     []json.RawMessage     `json:"graph"` // v2: graph nodes/edges; v1 is always empty
}

// ctxPkg is the top-level .ctxpkg bundle format.
type ctxPkg struct {
	Name      string       `json:"name"`
	Version   int          `json:"version"`
	CreatedAt string       `json:"created_at"`
	SHA256    string       `json:"sha256"` // SHA-256 hex of the JSON-encoded layers
	Layers    ctxPkgLayers `json:"layers"`
}

// PackOutput is the response for the ctx_pack tool.
type PackOutput struct {
	Status    string          `json:"status"` // "ok" | "not-found" | "no-index" | "error"
	Hint      string          `json:"hint,omitempty"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`      // export: path to the .ctxpkg file
	Packages  []PackListEntry `json:"packages,omitempty"`  // list action
	Info      *PackInfoEntry  `json:"info,omitempty"`      // info action
	Installed int             `json:"installed,omitempty"` // install/import: facts merged
}

// PackListEntry is one entry in the packages list.
type PackListEntry struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	AutoLoad  bool   `json:"auto_load"`
	HasFile   bool   `json:"has_file"` // whether the .ctxpkg file exists on disk
}

// PackInfoEntry is the detailed view of one package.
type PackInfoEntry struct {
	Name          string `json:"name"`
	CreatedAt     string `json:"created_at"`
	AutoLoad      bool   `json:"auto_load"`
	SHA256        string `json:"sha256"`
	KnowledgeSize int    `json:"knowledge_size"`
	GotchasSize   int    `json:"gotchas_size"`
	PatternsSize  int    `json:"patterns_size"`
	SessionTask   string `json:"session_task,omitempty"`
}

func (s *Server) ctxPack(ctx context.Context, _ *sdk.CallToolRequest, in PackInput) (*sdk.CallToolResult, PackOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, PackOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, PackOutput{
			Status: "no-index",
			Hint:   fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root),
		}, nil
	}

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	packDir := filepath.Join(p.CacheDir, "packages")

	switch in.Action {
	case "create":
		return s.packCreate(ctx, st, in, packDir)
	case "install":
		return s.packInstall(ctx, st, in, packDir)
	case "list":
		return s.packList(ctx, st, packDir)
	case "info":
		return s.packInfo(ctx, st, in, packDir)
	case "export":
		return s.packExport(in, packDir)
	case "import":
		return s.packImport(ctx, st, in, packDir)
	case "auto_load":
		return s.packAutoLoad(ctx, st, in)
	default:
		return nil, PackOutput{
			Status: "error",
			Hint:   fmt.Sprintf("unknown action %q — want: create | install | list | info | export | import | auto_load", in.Action),
		}, nil
	}
}

func (s *Server) packCreate(ctx context.Context, st *store.Store, in PackInput, packDir string) (*sdk.CallToolResult, PackOutput, error) {
	if in.Name == "" {
		return nil, PackOutput{Status: "error", Hint: "name is required for create"}, nil
	}

	// Collect knowledge facts.
	facts, err := st.KnowledgeQuery(ctx, 50)
	if err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("read knowledge: %v", err)}, nil
	}
	var knowledge, patterns, gotchas []ctxPkgKnowledgeFact
	for _, f := range facts {
		kf := ctxPkgKnowledgeFact{Archetype: f.Archetype, Body: f.Body, Confidence: f.Confidence}
		knowledge = append(knowledge, kf)
		switch f.Archetype {
		case "Pattern":
			patterns = append(patterns, kf)
		case "Gotcha":
			gotchas = append(gotchas, kf)
		}
	}

	// Collect session.
	var sess ctxPkgSession
	if ss, ok, err := st.SessionGet(ctx); err == nil && ok {
		sess.Task = ss.Task
		sess.Notes = ss.Notes
	}

	layers := ctxPkgLayers{
		Knowledge: knowledge,
		Session:   sess,
		Patterns:  patterns,
		Gotchas:   gotchas,
		Graph:     []json.RawMessage{},
	}

	layersJSON, err := json.Marshal(layers)
	if err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("marshal layers: %v", err)}, nil
	}
	sum := sha256.Sum256(layersJSON)
	pkg := ctxPkg{
		Name:      in.Name,
		Version:   1,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		SHA256:    fmt.Sprintf("%x", sum),
		Layers:    layers,
	}

	if err := os.MkdirAll(packDir, 0o755); err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("mkdir packages: %v", err)}, nil
	}
	pkgPath := filepath.Join(packDir, in.Name+".ctxpkg")
	data, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("marshal package: %v", err)}, nil
	}
	if err := os.WriteFile(pkgPath, data, 0o644); err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("write package: %v", err)}, nil
	}

	if err := st.PackRegister(ctx, in.Name, false); err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("register package: %v", err)}, nil
	}
	return nil, PackOutput{
		Status: "ok",
		Name:   in.Name,
		Path:   pkgPath,
		Hint:   fmt.Sprintf("created %s with %d knowledge facts", in.Name, len(knowledge)),
	}, nil
}

func (s *Server) packInstall(ctx context.Context, st *store.Store, in PackInput, packDir string) (*sdk.CallToolResult, PackOutput, error) {
	if in.Name == "" {
		return nil, PackOutput{Status: "error", Hint: "name is required for install"}, nil
	}
	pkgPath := filepath.Join(packDir, in.Name+".ctxpkg")
	pkg, err := loadCtxPkg(pkgPath)
	if err != nil {
		return nil, PackOutput{Status: "not-found", Hint: fmt.Sprintf("load package: %v", err)}, nil
	}
	n, err := mergeKnowledge(ctx, st, pkg.Layers.Knowledge)
	if err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("merge knowledge: %v", err)}, nil
	}
	if err := st.PackRegister(ctx, in.Name, false); err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("register package: %v", err)}, nil
	}
	return nil, PackOutput{
		Status:    "ok",
		Name:      in.Name,
		Installed: n,
		Hint:      fmt.Sprintf("installed %d facts from %s", n, in.Name),
	}, nil
}

func (s *Server) packList(ctx context.Context, st *store.Store, packDir string) (*sdk.CallToolResult, PackOutput, error) {
	records, err := st.PackList(ctx)
	if err != nil {
		return nil, PackOutput{Status: "error", Hint: err.Error()}, nil
	}
	out := PackOutput{Status: "ok"}
	for _, r := range records {
		pkgPath := filepath.Join(packDir, r.Name+".ctxpkg")
		_, statErr := os.Stat(pkgPath)
		out.Packages = append(out.Packages, PackListEntry{
			Name:      r.Name,
			CreatedAt: r.CreatedAt.Format(time.DateTime),
			AutoLoad:  r.AutoLoad,
			HasFile:   statErr == nil,
		})
	}
	return nil, out, nil
}

func (s *Server) packInfo(ctx context.Context, st *store.Store, in PackInput, packDir string) (*sdk.CallToolResult, PackOutput, error) {
	if in.Name == "" {
		return nil, PackOutput{Status: "error", Hint: "name is required for info"}, nil
	}
	r, ok, err := st.PackGet(ctx, in.Name)
	if err != nil {
		return nil, PackOutput{Status: "error", Hint: err.Error()}, nil
	}
	if !ok {
		return nil, PackOutput{Status: "not-found", Hint: fmt.Sprintf("package %q not registered — call create first", in.Name)}, nil
	}
	pkgPath := filepath.Join(packDir, in.Name+".ctxpkg")
	pkg, err := loadCtxPkg(pkgPath)
	if err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("load package file: %v", err)}, nil
	}
	return nil, PackOutput{
		Status: "ok",
		Info: &PackInfoEntry{
			Name:          r.Name,
			CreatedAt:     r.CreatedAt.Format(time.DateTime),
			AutoLoad:      r.AutoLoad,
			SHA256:        pkg.SHA256,
			KnowledgeSize: len(pkg.Layers.Knowledge),
			GotchasSize:   len(pkg.Layers.Gotchas),
			PatternsSize:  len(pkg.Layers.Patterns),
			SessionTask:   pkg.Layers.Session.Task,
		},
	}, nil
}

func (s *Server) packExport(in PackInput, packDir string) (*sdk.CallToolResult, PackOutput, error) {
	if in.Name == "" {
		return nil, PackOutput{Status: "error", Hint: "name is required for export"}, nil
	}
	pkgPath := filepath.Join(packDir, in.Name+".ctxpkg")
	if _, err := os.Stat(pkgPath); errors.Is(err, os.ErrNotExist) {
		return nil, PackOutput{Status: "not-found", Hint: fmt.Sprintf("package file not found at %s — call create first", pkgPath)}, nil
	}
	dest := pkgPath
	if in.Path != "" {
		data, err := os.ReadFile(pkgPath)
		if err != nil {
			return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("read package: %v", err)}, nil
		}
		if err := os.WriteFile(in.Path, data, 0o644); err != nil {
			return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("write to %s: %v", in.Path, err)}, nil
		}
		dest = in.Path
	}
	return nil, PackOutput{Status: "ok", Name: in.Name, Path: dest, Hint: "package ready for transfer"}, nil
}

func (s *Server) packImport(ctx context.Context, st *store.Store, in PackInput, packDir string) (*sdk.CallToolResult, PackOutput, error) {
	if in.Path == "" {
		return nil, PackOutput{Status: "error", Hint: "path is required for import"}, nil
	}
	pkg, err := loadCtxPkg(in.Path)
	if err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("load package: %v", err)}, nil
	}
	// Copy to local packages dir.
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("mkdir packages: %v", err)}, nil
	}
	data, err := os.ReadFile(in.Path)
	if err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("read file: %v", err)}, nil
	}
	dest := filepath.Join(packDir, pkg.Name+".ctxpkg")
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("write package: %v", err)}, nil
	}
	n, err := mergeKnowledge(ctx, st, pkg.Layers.Knowledge)
	if err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("merge knowledge: %v", err)}, nil
	}
	if err := st.PackRegister(ctx, pkg.Name, false); err != nil {
		return nil, PackOutput{Status: "error", Hint: fmt.Sprintf("register: %v", err)}, nil
	}
	return nil, PackOutput{
		Status:    "ok",
		Name:      pkg.Name,
		Path:      dest,
		Installed: n,
		Hint:      fmt.Sprintf("imported %s: merged %d facts", pkg.Name, n),
	}, nil
}

func (s *Server) packAutoLoad(ctx context.Context, st *store.Store, in PackInput) (*sdk.CallToolResult, PackOutput, error) {
	if in.Name == "" {
		return nil, PackOutput{Status: "error", Hint: "name is required for auto_load"}, nil
	}
	if err := st.PackSetAutoLoad(ctx, in.Name); err != nil {
		if errors.Is(err, store.ErrPackNotFound) {
			return nil, PackOutput{Status: "not-found", Hint: fmt.Sprintf("package %q not registered — call create or import first", in.Name)}, nil
		}
		return nil, PackOutput{Status: "error", Hint: err.Error()}, nil
	}
	return nil, PackOutput{Status: "ok", Name: in.Name, Hint: "package will be loaded automatically on ctx_overview"}, nil
}

// loadCtxPkg reads and verifies a .ctxpkg file from disk.
func loadCtxPkg(path string) (*ctxPkg, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pkg ctxPkg
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	// Verify SHA-256 of layers.
	layersJSON, err := json.Marshal(pkg.Layers)
	if err != nil {
		return nil, fmt.Errorf("re-marshal layers: %w", err)
	}
	sum := sha256.Sum256(layersJSON)
	got := fmt.Sprintf("%x", sum)
	if pkg.SHA256 != got {
		return nil, fmt.Errorf("SHA-256 mismatch: file=%s computed=%s", pkg.SHA256, got)
	}
	return &pkg, nil
}

// mergeKnowledge adds each knowledge fact from the package into the store.
// Returns the number of facts that were new (first time stored).
func mergeKnowledge(ctx context.Context, st *store.Store, facts []ctxPkgKnowledgeFact) (int, error) {
	n := 0
	for _, f := range facts {
		if f.Body == "" {
			continue
		}
		rev, err := st.KnowledgeAdd(ctx, f.Archetype, f.Body, f.Confidence)
		if err != nil {
			continue
		}
		if rev == 0 {
			n++ // first time stored
		}
	}
	return n, nil
}
