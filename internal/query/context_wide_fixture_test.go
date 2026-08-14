package query

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// The wide fixture exists to exercise seed *count*, which the shared
// contextFixture cannot: it holds four semantic seeds, so an expansion ceiling
// of eight is invisible there.
//
// It builds `seeds` packages whose doc comments carry exactly the same task
// tokens, so every hit scores identically and SemanticSearch's tie-break --
// score DESC, path ASC, qualified_name ASC -- puts them in package order.
// Seed rank is therefore an assertion the test can make rather than something
// it has to discover.
//
// One extra package calls the seed at `callerOfSeed` and shares none of the
// task's tokens, so it can only enter the answer as caller context. Whether it
// does is the whole question: at rank 10 it is past the old eight-seed ceiling.
const (
	wideTask = "invoice ledger reconciliation"
	// callerOfSeed is 1-based and deliberately past contextExpansionSeeds.
	callerOfSeed = 10
)

func wideSeedPkg(i int) string { return fmt.Sprintf("s%02d", i) }

func wideSeedSymbol(i int) string { return fmt.Sprintf("%s.Alpha%02d", wideSeedPkg(i), i) }

// newWideFixture indexes a repository with `seeds` equally-ranked semantic
// seeds plus one non-matching caller of seed `callerOfSeed`.
func newWideFixture(t testing.TB, seeds int) *contextFixture {
	t.Helper()
	if seeds < callerOfSeed {
		t.Fatalf("newWideFixture(%d): need at least %d seeds", seeds, callerOfSeed)
	}
	ctx := context.Background()
	repoRoot := t.TempDir()

	writeFixtureFile(t, repoRoot, "go.mod", "module example.test/wide\n\ngo 1.22\n")
	for i := 1; i <= seeds; i++ {
		pkg := wideSeedPkg(i)
		writeFixtureFile(t, repoRoot, pkg+"/"+pkg+".go", fmt.Sprintf(`package %s

// Alpha%02d performs reconciliation of the invoice ledger.
func Alpha%02d(id string) error { return nil }
`, pkg, i, i))
	}
	// The caller. Its package name, symbol name and doc share no token with the
	// task, so semantic search never returns it.
	target := wideSeedPkg(callerOfSeed)
	writeFixtureFile(t, repoRoot, "zzdriver/dispatch.go", fmt.Sprintf(`package zzdriver

import "example.test/wide/%s"

// Dispatch forwards a request downstream.
func Dispatch(id string) error { return %s.Alpha%02d(id) }
`, target, target, callerOfSeed))

	st, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	idx := indexer.New(st, parser.NewRegistry(goparser.New()), nil)
	repo, err := st.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	fx := &contextFixture{repoRoot: repoRoot, repoID: repo.ID, store: st, indexer: idx}
	fx.svc = New(st, nil)
	return fx
}

// wideOpts asks for enough files that the file cap cannot be what removes the
// caller: this fixture puts every symbol in its own package.
func wideOpts(seeds int) ContextForTaskOptions {
	return ContextForTaskOptions{
		MaxFiles:       seeds + 5,
		MaxSymbols:     seeds,
		IncludeCallers: true,
		MaxTokens:      MaxContextMaxTokens,
	}
}

// contextQualifiedNames returns every qualified name in the answer.
func contextQualifiedNames(t *testing.T, fx *contextFixture, task string, opts ContextForTaskOptions) map[string]string {
	t.Helper()
	res := mustContext(t, fx, task, opts)
	out := map[string]string{}
	for _, sym := range allSymbols(res) {
		out[sym.QualifiedName] = sym.Relevance
	}
	return out
}

// The seed stream is what the test claims it is: equal scores, package order.
// Without this the "past rank 8" assertions below would be untethered.
func TestWideFixtureSeedOrderIsPackageOrder(t *testing.T) {
	fx := newWideFixture(t, 12)
	raw, err := fx.svc.SemanticSearch(context.Background(), fx.repoID, wideTask, 12, 0)
	if err != nil {
		t.Fatalf("SemanticSearch() error = %v", err)
	}
	hits := parseSearchHits(raw)
	if len(hits) < 12 {
		t.Fatalf("got %d semantic hits, want 12", len(hits))
	}
	for i := 0; i < 12; i++ {
		if want := wideSeedSymbol(i + 1); hits[i].QualifiedName != want {
			t.Fatalf("hit[%d] = %q, want %q", i, hits[i].QualifiedName, want)
		}
	}
}

// P19: a seed past the old eight-seed ceiling contributes graph context.
//
// The seed itself was always there -- it is a direct match, and MaxSymbols
// covers it. What the ceiling removed was its neighbourhood, which is the half
// an agent cannot reconstruct from the search result alone.
func TestContextExpandsSeedsPastRankEight(t *testing.T) {
	const seeds = 12
	fx := newWideFixture(t, seeds)
	got := contextQualifiedNames(t, fx, wideTask, wideOpts(seeds))

	seed := wideSeedSymbol(callerOfSeed)
	if _, ok := got[seed]; !ok {
		t.Fatalf("seed %q missing from context; got %v", seed, got)
	}
	rel, ok := got["zzdriver.Dispatch"]
	if !ok {
		t.Fatalf("caller of rank-%d seed %q is absent: expansion still stops early.\ngot %v",
			callerOfSeed, seed, got)
	}
	if rel != relevanceCaller {
		t.Fatalf("zzdriver.Dispatch relevance = %q, want %q", rel, relevanceCaller)
	}
}

// All of the default MaxSymbols seeds take part in expansion, not a prefix.
func TestContextExpandsAllDefaultSeeds(t *testing.T) {
	const seeds = defaultContextMaxSymbols
	fx := newWideFixture(t, seeds)
	opts := wideOpts(seeds)
	opts.MaxSymbols = 0 // default
	got := contextQualifiedNames(t, fx, wideTask, opts)

	if _, ok := got["zzdriver.Dispatch"]; !ok {
		t.Fatalf("caller of rank-%d seed absent with %d default seeds; got %v", callerOfSeed, seeds, got)
	}
}
