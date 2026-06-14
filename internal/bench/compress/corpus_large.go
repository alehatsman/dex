package compress

import (
	"fmt"
	"strings"
)

// Large, representative samples for the compress benchmark (#492).
//
// The small BuiltinCorpus fixtures (16–50 lines) are too short to exercise the
// passes that matter most in production: the lossless dictionary passes
// (codebook / ngram_codebook / symmap) only pay off once a token repeats enough
// to be worth dictionary-coding, and the lossy passes (aggressive / entropy)
// only show representative behaviour at the multi-hundred-line scale that
// `dex shell` and the indexer actually compress (build/test logs, stack traces,
// large diffs, large source files).
//
// These samples are generated deterministically (no randomness) so the bench
// stays reproducible across runs. They are appended to BuiltinCorpus at init.
func init() { BuiltinCorpus = append(BuiltinCorpus, largeSamples()...) }

// largeSamples returns the four large modality fixtures.
func largeSamples() []Sample {
	return []Sample{
		largeTestLog(),
		largeStackTrace(),
		largeDiff(),
		largeSource(),
	}
}

// pkgs is a stable set of repeated package paths. Their repetition across the
// generated samples is exactly what the dictionary passes are meant to fold.
var pkgs = []string{
	"internal/store", "internal/embed", "internal/index", "internal/chunk",
	"internal/graph", "internal/retrieve", "internal/summarize", "internal/compress",
	"internal/tokens", "internal/proj", "internal/mcp", "internal/eval",
}

// largeTestLog builds a multi-hundred-line `go test ./...` log: a mix of PASS
// and FAIL packages with per-test file:line detail. The repeated
// "github.com/alehatsman/dex/<pkg>" prefix and "--- PASS:/FAIL:" markers give
// the dictionary passes real material to fold.
func largeTestLog() Sample {
	var b strings.Builder
	for i, p := range pkgs {
		// Two packages fail to keep failure-detail lines in the corpus.
		fail := i == 0 || i == 4
		for t := 1; t <= 6; t++ {
			name := fmt.Sprintf("Test%s%d", titleOf(p), t)
			if fail && t == 1 {
				b.WriteString(fmt.Sprintf("--- FAIL: %s (0.%02ds)\n", name, t*7%99))
				b.WriteString(fmt.Sprintf("    %s_test.go:%d: %s: want %d hits, got %d\n",
					baseOf(p), 100+t*13, name, 3, 1))
				b.WriteString(fmt.Sprintf("    %s_test.go:%d: first hit: github.com/alehatsman/dex/%s/%s.go (score=0.%d)\n",
					baseOf(p), 101+t*13, p, baseOf(p), 70+t))
			} else {
				b.WriteString(fmt.Sprintf("--- PASS: %s (0.%02ds)\n", name, t*3%99))
			}
		}
		if fail {
			b.WriteString("FAIL\n")
			b.WriteString(fmt.Sprintf("FAIL\tgithub.com/alehatsman/dex/%s\t0.%02ds\n", p, (i*11)%99))
		} else {
			b.WriteString(fmt.Sprintf("ok  \tgithub.com/alehatsman/dex/%s\t0.%02ds\n", p, (i*7)%99))
		}
	}
	content := b.String()
	return Sample{
		Name:    "large-test-log",
		Kind:    "log",
		Content: content,
		Anchors: []string{
			"TestStore1",
			"github.com/alehatsman/dex/internal/store",
			"github.com/alehatsman/dex/internal/embed",
			"FAIL",
		},
		Spans: []string{
			"FAIL\tgithub.com/alehatsman/dex/internal/store",
			"want 3 hits, got 1",
		},
	}
}

// largeStackTrace builds a panic goroutine dump with many frames. The repeated
// import-path prefix on every other line is highly dictionary-able.
func largeStackTrace() Sample {
	var b strings.Builder
	b.WriteString("panic: runtime error: invalid memory address or nil pointer dereference\n")
	b.WriteString("[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4a1c20]\n\n")
	b.WriteString("goroutine 42 [running]:\n")
	for i := 0; i < 40; i++ {
		p := pkgs[i%len(pkgs)]
		fn := fmt.Sprintf("(*%s).step%d", titleOf(p), i)
		b.WriteString(fmt.Sprintf("github.com/alehatsman/dex/%s.%s(0xc0000b%04x, 0x0)\n", p, fn, i))
		b.WriteString(fmt.Sprintf("\t/home/aleh/projects/dex/%s/%s.go:%d +0x%x\n", p, baseOf(p), 100+i*7, 0x40+i))
	}
	content := b.String()
	return Sample{
		Name:    "large-stacktrace",
		Kind:    "log",
		Content: content,
		Anchors: []string{
			"panic: runtime error",
			"SIGSEGV",
			"goroutine 42",
			"github.com/alehatsman/dex/internal/store",
		},
		Spans: []string{
			"panic: runtime error: invalid memory address or nil pointer dereference",
			"goroutine 42 [running]:",
		},
	}
}

// largeDiff builds a multi-file unified diff with many hunks. The repeated
// "diff --git", "@@", and Go boilerplate (ctx context.Context, fmt.Errorf) lines
// recur across files.
func largeDiff() Sample {
	var b strings.Builder
	for i, p := range pkgs {
		f := fmt.Sprintf("%s/%s.go", p, baseOf(p))
		b.WriteString(fmt.Sprintf("diff --git a/%s b/%s\n", f, f))
		b.WriteString(fmt.Sprintf("index %06x..%06x 100644\n", 0x3a1b00+i, 0x9d4e00+i))
		b.WriteString(fmt.Sprintf("--- a/%s\n", f))
		b.WriteString(fmt.Sprintf("+++ b/%s\n", f))
		b.WriteString(fmt.Sprintf("@@ -%d,6 +%d,14 @@ func (s *%s) Open(ctx context.Context) error {\n",
			10+i*4, 10+i*4, titleOf(p)))
		b.WriteString(" \treturn s.init(ctx)\n }\n\n")
		b.WriteString(fmt.Sprintf("+func (s *%s) OpenWith(ctx context.Context, opts Options) error {\n", titleOf(p)))
		b.WriteString("+\tif err := s.init(ctx); err != nil {\n")
		b.WriteString(fmt.Sprintf("+\t\treturn fmt.Errorf(\"%s open: %%w\", err)\n", baseOf(p)))
		b.WriteString("+\t}\n+\treturn nil\n+}\n")
	}
	content := b.String()
	return Sample{
		Name:    "large-diff",
		Kind:    "diff",
		Content: content,
		Anchors: []string{
			"diff --git a/internal/store/store.go",
			"OpenWith",
			"ctx context.Context",
			"fmt.Errorf",
		},
		Spans: []string{
			"diff --git a/internal/store/store.go b/internal/store/store.go",
			"func (s *Store) OpenWith(ctx context.Context, opts Options) error",
		},
	}
}

// largeSource builds a large Go file with many similar methods. The recurring
// "func (s *Store)", "ctx context.Context", and "fmt.Errorf" tokens make this
// the canonical case where symmap / codebook earn their legend cost.
func largeSource() Sample {
	var b strings.Builder
	b.WriteString("// Package store provides the on-disk index for dex.\n")
	b.WriteString("package store\n\n")
	b.WriteString("import (\n\t\"context\"\n\t\"database/sql\"\n\t\"fmt\"\n)\n\n")
	verbs := []string{"Count", "Prune", "Vacuum", "Reindex", "Touch", "Flush", "Verify", "Compact"}
	for i := 0; i < 40; i++ {
		v := verbs[i%len(verbs)]
		name := fmt.Sprintf("%s%d", v, i)
		b.WriteString(fmt.Sprintf("// %s runs the %s maintenance step %d against the index.\n", name, strings.ToLower(v), i))
		b.WriteString(fmt.Sprintf("func (s *Store) %s(ctx context.Context, n int) (int, error) {\n", name))
		b.WriteString("\tvar got int\n")
		b.WriteString(fmt.Sprintf("\trow := s.db.QueryRowContext(ctx, \"SELECT COUNT(*) FROM chunks WHERE step = %d\")\n", i))
		b.WriteString("\tif err := row.Scan(&got); err != nil {\n")
		b.WriteString(fmt.Sprintf("\t\treturn 0, fmt.Errorf(\"%s: %%w\", err)\n", strings.ToLower(name)))
		b.WriteString("\t}\n\treturn got, nil\n}\n\n")
	}
	content := b.String()
	return Sample{
		Name:    "large-source",
		Kind:    "code",
		Content: content,
		Anchors: []string{
			"package store",
			"func (s *Store) Count0",
			"ctx context.Context",
			"fmt.Errorf",
			"QueryRowContext",
		},
		Spans: []string{
			"func (s *Store) Count0(ctx context.Context, n int) (int, error)",
			"return got, nil",
		},
	}
}

// titleOf returns the CamelCase type-ish name for a package path's last
// segment ("internal/store" -> "Store").
func titleOf(pkgPath string) string {
	base := baseOf(pkgPath)
	if base == "" {
		return ""
	}
	return strings.ToUpper(base[:1]) + base[1:]
}

// baseOf returns the last path segment ("internal/store" -> "store").
func baseOf(pkgPath string) string {
	if i := strings.LastIndex(pkgPath, "/"); i >= 0 {
		return pkgPath[i+1:]
	}
	return pkgPath
}
