package store

import (
	"testing"
	"time"
)

func TestBuildMultiScaleEmpty(t *testing.T) {
	st, ctx := newStore(t)
	idx, err := st.BuildMultiScale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if idx == nil {
		t.Fatal("expected non-nil index even when empty")
	}
	// Empty index returns nothing.
	if got := idx.SearchMeso([]string{"foo"}, 5); len(got) != 0 {
		t.Errorf("SearchMeso on empty index = %v, want []", got)
	}
	if got := idx.SearchMacro([]string{"foo"}, 5); len(got) != 0 {
		t.Errorf("SearchMacro on empty index = %v, want []", got)
	}
}

func TestBuildMultiScaleSearchMeso(t *testing.T) {
	st, ctx := newStore(t)

	chunks := []PendingChunk{
		{Path: "auth/login.go", Kind: "fn", StartLine: 1, EndLine: 10, ContentSHA: "h1",
			Content: "login authenticate session password token credential"},
		{Path: "auth/signup.go", Kind: "fn", StartLine: 1, EndLine: 10, ContentSHA: "h2",
			Content: "signup register email verify account confirm password"},
		{Path: "api/handler.go", Kind: "fn", StartLine: 1, EndLine: 10, ContentSHA: "h3",
			Content: "handler route dispatch request response http"},
	}
	if err := st.UpsertMany(ctx, chunks, time.Now()); err != nil {
		t.Fatal(err)
	}

	idx, err := st.BuildMultiScale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if idx == nil {
		t.Fatal("nil index")
	}

	// "login" appears only in auth/login.go — it should be ranked first.
	results := idx.SearchMeso(TokeniseQuery("login"), 3)
	if len(results) == 0 {
		t.Fatal("SearchMeso returned no results")
	}
	if results[0] != "auth/login.go" {
		t.Errorf("top result = %q, want auth/login.go (got %v)", results[0], results)
	}
}

func TestBuildMultiScaleSearchMacro(t *testing.T) {
	st, ctx := newStore(t)

	chunks := []PendingChunk{
		// auth/ has two files sharing auth-specific tokens
		{Path: "auth/login.go", Kind: "fn", StartLine: 1, EndLine: 10, ContentSHA: "h1",
			Content: "authenticate session password validate credentials"},
		{Path: "auth/signup.go", Kind: "fn", StartLine: 1, EndLine: 10, ContentSHA: "h2",
			Content: "register account email password credentials verify"},
		{Path: "api/routes.go", Kind: "fn", StartLine: 1, EndLine: 10, ContentSHA: "h3",
			Content: "router dispatch handler middleware"},
	}
	if err := st.UpsertMany(ctx, chunks, time.Now()); err != nil {
		t.Fatal(err)
	}

	idx, err := st.BuildMultiScale(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// "credentials" appears in both auth/ files but not api/ → auth should rank top.
	results := idx.SearchMacro(TokeniseQuery("credentials authenticate"), 3)
	if len(results) == 0 {
		t.Fatal("SearchMacro returned no results")
	}
	if results[0] != "auth" {
		t.Errorf("top macro result = %q, want auth (got %v)", results[0], results)
	}
}

func TestSearchMesoEmptyQuery(t *testing.T) {
	st, ctx := newStore(t)
	if err := st.UpsertMany(ctx, []PendingChunk{
		{Path: "a/a.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "h1", Content: "func Foo(){}"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	idx, _ := st.BuildMultiScale(ctx)
	if got := idx.SearchMeso(nil, 5); len(got) != 0 {
		t.Errorf("empty query returned results: %v", got)
	}
	if got := idx.SearchMeso([]string{}, 5); len(got) != 0 {
		t.Errorf("empty query returned results: %v", got)
	}
}

func TestSearchMesoKLimit(t *testing.T) {
	st, ctx := newStore(t)
	chunks := []PendingChunk{
		{Path: "a/a.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "h1", Content: "authentication token session login alpha"},
		{Path: "b/b.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "h2", Content: "authentication token session login beta"},
		{Path: "c/c.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "h3", Content: "authentication token session login gamma"},
		{Path: "d/d.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "h4", Content: "authentication token session login delta"},
	}
	if err := st.UpsertMany(ctx, chunks, time.Now()); err != nil {
		t.Fatal(err)
	}
	idx, _ := st.BuildMultiScale(ctx)
	results := idx.SearchMeso(TokeniseQuery("authentication login"), 2)
	if len(results) > 2 {
		t.Errorf("SearchMeso returned %d results with k=2", len(results))
	}
}

func TestBuildMultiScaleSkipsSummaryChunks(t *testing.T) {
	st, ctx := newStore(t)
	chunks := []PendingChunk{
		{Path: "x/x.go", Kind: "file_summary", StartLine: 0, EndLine: 0, ContentSHA: "hs",
			Content: "only summary tokens here summary tokens"},
		{Path: "y/y.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "hf",
			Content: "real function tokens here"},
	}
	if err := st.UpsertMany(ctx, chunks, time.Now()); err != nil {
		t.Fatal(err)
	}
	idx, _ := st.BuildMultiScale(ctx)

	// x/x.go (summary only) should not appear; y/y.go (fn) should.
	all := idx.SearchMeso([]string{"tokens"}, 10)
	for _, p := range all {
		if p == "x/x.go" {
			t.Errorf("file_summary chunk path x/x.go appeared in meso results: %v", all)
		}
	}
}
