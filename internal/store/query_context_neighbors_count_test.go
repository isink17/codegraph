package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// newSeedScaleFixture builds `seeds` symbols, each with one resolved caller,
// one resolved callee and one unresolved callee, plus a population of
// unresolved edges for the shared suffix scan to walk.
func newSeedScaleFixture(t *testing.T, seeds int) (*Store, int64, []ContextSeed) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	fileID, err := insertTestFile(ctx, s, repo.ID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile() error = %v", err)
	}

	edge := func(src, dst int64, name string) {
		t.Helper()
		var dstArg any
		if dst != 0 {
			dstArg = dst
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, ?, ?, 'call', '', ?, 1)
		`, repo.ID, src, dstArg, name, fileID); err != nil {
			t.Fatalf("insert edge error = %v", err)
		}
	}

	// One seed in a language P22.9 governs, with a bare unresolved callee, so the
	// cascade's gated/ungated split actually runs -- an all-Go fixture leaves the
	// gated half empty and the bound below would not cover it.
	pyFileID, err := insertTestFileLang(ctx, s, repo.ID, "a.py", "python")
	if err != nil {
		t.Fatalf("insertTestFileLang() error = %v", err)
	}
	pySeedID, err := insertTestSymbolLang(ctx, s, repo.ID, pyFileID, "PySeed", "pkg.PySeed", "python")
	if err != nil {
		t.Fatalf("insertTestSymbolLang() error = %v", err)
	}
	pyCallerID, err := insertTestSymbolLang(ctx, s, repo.ID, pyFileID, "PyCaller", "pkg.PyCaller", "python")
	if err != nil {
		t.Fatalf("insertTestSymbolLang() error = %v", err)
	}
	pyCalleeID, err := insertTestSymbolLang(ctx, s, repo.ID, pyFileID, "PyCallee", "pkg.PyCallee", "python")
	if err != nil {
		t.Fatalf("insertTestSymbolLang() error = %v", err)
	}
	for _, e := range []struct {
		src, dst int64
		name     string
	}{
		{pyCallerID, pySeedID, "pkg.PySeed"},
		{pySeedID, pyCalleeID, "pkg.PyCallee"},
		{pySeedID, 0, "PyBare"},
	} {
		var dstArg any
		if e.dst != 0 {
			dstArg = e.dst
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, ?, ?, 'call', '', ?, 1)
		`, repo.ID, e.src, dstArg, e.name, pyFileID); err != nil {
			t.Fatalf("insert python edge error = %v", err)
		}
	}

	out := make([]ContextSeed, 0, seeds+1)
	out = append(out, ContextSeed{
		SymbolID: pySeedID, QualifiedName: "pkg.PySeed", ShortName: "PySeed", AllowShortEvidence: true,
	})
	for i := range seeds {
		qname := fmt.Sprintf("pkg%02d.Seed%02d", i, i)
		short := fmt.Sprintf("Seed%02d", i)
		seedID, err := insertTestSymbol(ctx, s, repo.ID, fileID, short, qname)
		if err != nil {
			t.Fatalf("insertTestSymbol() error = %v", err)
		}
		callerID, err := insertTestSymbol(ctx, s, repo.ID, fileID,
			fmt.Sprintf("Caller%02d", i), fmt.Sprintf("pkg%02d.Caller%02d", i, i))
		if err != nil {
			t.Fatalf("insertTestSymbol() error = %v", err)
		}
		calleeID, err := insertTestSymbol(ctx, s, repo.ID, fileID,
			fmt.Sprintf("Callee%02d", i), fmt.Sprintf("pkg%02d.Callee%02d", i, i))
		if err != nil {
			t.Fatalf("insertTestSymbol() error = %v", err)
		}
		edge(callerID, seedID, qname)
		edge(seedID, calleeID, fmt.Sprintf("pkg%02d.Callee%02d", i, i))
		// An unresolved destination, so the callee name cascade runs.
		edge(seedID, 0, fmt.Sprintf("pkg%02d.Callee%02d", i, i))
		// An unresolved caller edge naming the seed, so the suffix scan has
		// something to walk.
		edge(callerID, 0, "other."+short)
		// A BARE unresolved destination as well. P22.9 splits the cascade's
		// equality stages by whether the spelling carries a qualifier, so a
		// fixture that is entirely qualified would never exercise the split and
		// the bound below would not cover it.
		edge(seedID, 0, fmt.Sprintf("Bare%02d", i))

		out = append(out, ContextSeed{
			SymbolID: seedID, QualifiedName: qname, ShortName: short, AllowShortEvidence: true,
		})
	}
	return s, repo.ID, out
}

// P19's central claim, stated as an upper bound rather than an exact number so
// that legitimate re-chunking does not break the test: neighbour work is
// chunk-driven, not seed-driven.
//
// The pre-P19 shape ran two statements per seed plus their internals -- 60
// statements at 30 seeds, and one full unresolved-edge scan per seed on top.
// If this test starts failing at the higher seed counts, the batch API has
// grown a per-seed loop again.
func TestContextNeighborStatementsDoNotScaleWithSeeds(t *testing.T) {
	// The fixed pipeline is: one seed package-scope lookup (P22.6), one shared
	// suffix scan, one unresolved-destination query, the name cascade, and one
	// page statement per chunk per direction. The cascade is four evidence
	// stages, and since P22.9 its two equality stages each split by whether the
	// spelling carries a qualifier and by whether the source language is one
	// whose visibility the type rule decides -- so it is bounded by statements,
	// not by names, and never by seeds. Two chunks' worth of headroom on top.
	const bound = 16
	ctx := context.Background()

	counts := map[int]int64{}
	for _, seeds := range []int{1, 8, 30} {
		s, repoID, seedSet := newSeedScaleFixture(t, seeds)
		s.resetContextNeighborStatements()
		if _, err := s.FindContextNeighbors(ctx, repoID, seedSet, 10); err != nil {
			t.Fatalf("FindContextNeighbors(%d seeds) error = %v", seeds, err)
		}
		got := s.contextNeighborStatements()
		counts[seeds] = got
		t.Logf("seeds=%d neighbour statements=%d", seeds, got)
		if got > bound {
			t.Fatalf("%d seeds issued %d neighbour statements, want at most %d", seeds, got, bound)
		}
	}
	// Not merely bounded: flat. A slope of even one statement per seed would
	// show up as a difference of 22 between 8 and 30.
	if counts[30] != counts[1] {
		t.Fatalf("statement count moved with seed count: 1 seed = %d, 30 seeds = %d", counts[1], counts[30])
	}
}

// The chunker splits on the bound-variable budget, so a seed set far past one
// chunk still runs -- and still does not go per-seed.
func TestContextNeighborStatementsChunkCleanly(t *testing.T) {
	const seeds = 200
	ctx := context.Background()
	s, repoID, seedSet := newSeedScaleFixture(t, seeds)
	s.resetContextNeighborStatements()
	got, err := s.FindContextNeighbors(ctx, repoID, seedSet, 10)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	// seeds+1: the fixture adds one python seed so the cascade's gated/ungated
	// split runs (an all-Go fixture never reaches it).
	if len(got) != seeds+1 {
		t.Fatalf("got %d result slots for %d seeds", len(got), seeds+1)
	}
	for i, n := range got {
		if len(n.Callers) == 0 || len(n.Callees) == 0 {
			t.Fatalf("seed %d lost its neighbours across the chunk boundary", i)
		}
	}
	// 200 seeds is four 64-seed chunks per direction at most; nothing close to
	// the 400 statements a per-seed shape would issue.
	if stmts := s.contextNeighborStatements(); stmts > 24 {
		t.Fatalf("%d seeds issued %d statements", seeds, stmts)
	}
}

// A hot hub must not materialize its whole fan-in just because the call is
// batched: the page is taken inside SQLite, exactly as P12 made the public
// query take it.
func TestContextNeighborsDoNotMaterializeWholeHub(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	fileID, err := insertTestFile(ctx, s, repo.ID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile() error = %v", err)
	}
	hubID, err := insertTestSymbol(ctx, s, repo.ID, fileID, "Hub", "pkg.Hub")
	if err != nil {
		t.Fatalf("insertTestSymbol() error = %v", err)
	}
	const fanIn = 5000
	for i := range fanIn {
		cid, err := insertTestSymbol(ctx, s, repo.ID, fileID,
			fmt.Sprintf("C%04d", i), fmt.Sprintf("pkg.C%04d", i))
		if err != nil {
			t.Fatalf("insertTestSymbol() error = %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, ?, 'pkg.Hub', 'call', '', ?, 1)
		`, repo.ID, cid, hubID, fileID); err != nil {
			t.Fatalf("insert edge error = %v", err)
		}
	}

	seed := ContextSeed{SymbolID: hubID, QualifiedName: "pkg.Hub", ShortName: "Hub", AllowShortEvidence: true}
	allocs := testing.AllocsPerRun(3, func() {
		if _, err := s.FindContextNeighbors(ctx, repo.ID, []ContextSeed{seed}, 10); err != nil {
			t.Fatalf("FindContextNeighbors() error = %v", err)
		}
	})
	// Ten returned symbols cost on the order of a few hundred allocations. A
	// shape that pulled all 5000 callers into Go before slicing would be two
	// orders of magnitude above this.
	if allocs > 3000 {
		t.Fatalf("hub expansion cost %.0f allocations for a 10-symbol page; fan-in is being materialized", allocs)
	}
}

// One seed whose short name matches hundreds of unresolved destinations has
// more evidence than a single statement's bound-variable budget allows. It must
// still get the right page: the chunker splits its evidence, each statement
// returns the best rows of its share, and the merge re-trims to the same page a
// single statement would have produced.
func TestContextNeighborsSplitOversizedSeedEvidence(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	fileID, err := insertTestFile(ctx, s, repo.ID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile() error = %v", err)
	}
	seedID, err := insertTestSymbol(ctx, s, repo.ID, fileID, "Widget", "pkg.Widget")
	if err != nil {
		t.Fatalf("insertTestSymbol() error = %v", err)
	}
	// 600 distinct unresolved destination names, all extending the seed's
	// identity "pkg.Widget" at a boundary, each from its own caller. 600 name
	// pairs is well past contextStatementVarBudget/2.
	const callers = 600
	for i := range callers {
		cid, err := insertTestSymbol(ctx, s, repo.ID, fileID,
			fmt.Sprintf("C%03d", i), fmt.Sprintf("pkg.C%03d", i))
		if err != nil {
			t.Fatalf("insertTestSymbol() error = %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, NULL, ?, 'call', '', ?, 1)
		`, repo.ID, cid, fmt.Sprintf("mod%03d.pkg.Widget", i), fileID); err != nil {
			t.Fatalf("insert edge error = %v", err)
		}
	}

	seed := ContextSeed{SymbolID: seedID, QualifiedName: "pkg.Widget", ShortName: "Widget", AllowShortEvidence: true}
	s.resetContextNeighborStatements()
	got, err := s.FindContextNeighbors(ctx, repo.ID, []ContextSeed{seed}, 10)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	if len(got[0].Callers) != 10 {
		t.Fatalf("got %d callers, want the fanout of 10", len(got[0].Callers))
	}
	// The page is the first ten by qualified_name across the whole evidence set,
	// not the first ten of whichever chunk happened to run first.
	for i, c := range got[0].Callers {
		if want := fmt.Sprintf("pkg.C%03d", i); c.QualifiedName != want {
			t.Fatalf("caller[%d] = %q, want %q -- the split pages were not merged", i, c.QualifiedName, want)
		}
	}
	// And it must agree with the public query, which runs it as one statement.
	want, err := s.FindCallers(ctx, repo.ID, "pkg.Widget", seedID, 10, 0)
	if err != nil {
		t.Fatalf("FindCallers() error = %v", err)
	}
	for i := range want {
		if want[i].ID != got[0].Callers[i].ID {
			t.Fatalf("caller[%d] = %s, public = %s", i, got[0].Callers[i].QualifiedName, want[i].QualifiedName)
		}
	}
	// Unsplit, this fixture is four statements: the shared suffix scan, one
	// caller page, the unresolved-destination query and one callee page. The
	// split adds a second caller page. Anything less means splitNeighborWork
	// did not fire and the merge above was never exercised.
	if stmts := s.contextNeighborStatements(); stmts < 5 {
		t.Fatalf("%d statements; the evidence did not split, so the merge path is untested", stmts)
	}
}

// A statement's bound-variable count is what the driver enforces, and the legs
// no longer cost the same: an id row binds two values, a name row three (P22.6
// added the package-scope column). Budgeting in rows and assuming two variables
// each would build a 1050-variable statement out of a 700-variable budget, past
// the 999 ceiling a cgo build still has.
func TestSplitNeighborWorkBudgetsVariablesNotRows(t *testing.T) {
	names := make([]namePair, 1000)
	for i := range names {
		names[i] = namePair{idx: 1, name: fmt.Sprintf("n%03d", i), scope: "pkg/"}
	}
	ids := make([]int64, 1000)
	for i := range ids {
		ids[i] = int64(i + 1)
	}

	for _, tc := range []struct {
		name string
		work neighborWork
	}{
		{"names only", neighborWork{idx: 1, names: names}},
		{"ids only", neighborWork{idx: 1, ids: ids}},
		{"mixed", neighborWork{idx: 1, ids: ids, names: names, fallback: ids}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remaining := tc.work
			total, parts := 0, 0
			for !remaining.empty() {
				var part neighborWork
				part, remaining = splitNeighborWork(remaining, contextStatementVarBudget)
				if part.empty() {
					t.Fatalf("split made no progress with a full budget")
				}
				if v := part.vars(); v > contextStatementVarBudget {
					t.Fatalf("part binds %d variables, budget is %d", v, contextStatementVarBudget)
				}
				total += part.vars()
				parts++
				if parts > 10000 {
					t.Fatalf("split did not terminate")
				}
			}
			if total != tc.work.vars() {
				t.Fatalf("split lost evidence: %d variables across parts, input had %d", total, tc.work.vars())
			}
		})
	}
}
