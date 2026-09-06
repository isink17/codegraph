package store

import (
	"context"
	"fmt"
	"testing"
)

// rustBudgetFixture is a repository with many independent Rust crate roots --
// enough that the old one-statement root fan-out (three bound parameters per
// root) would have exceeded the portable SQLite variable limit.
type rustBudgetFixture struct {
	store  *Store
	repoID int64
	roots  map[string]struct{}
	// edgeByCrate maps a crate root path to the edge its root file owns.
	edgeByCrate map[string]int64
	// wantTarget maps that edge to the symbol it must bind.
	wantTarget map[int64]int64
	// ambiguous is the edge whose module declaration matches files in two
	// different crates, and which therefore must stay unresolved.
	ambiguous int64
}

const rustBudgetCrateCount = 400

func buildRustBudgetFixture(t *testing.T, s *Store, repoID int64) *rustBudgetFixture {
	t.Helper()
	ctx := context.Background()
	f := &rustBudgetFixture{
		store:       s,
		repoID:      repoID,
		roots:       map[string]struct{}{},
		edgeByCrate: map[string]int64{},
		wantTarget:  map[int64]int64{},
	}
	addFile := func(path, module string) int64 {
		id, err := insertTestFileLang(ctx, s, repoID, path, "rust")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,module_path,crate_root) VALUES(?,?,?,?,'')`, repoID, id, "rust", module); err != nil {
			t.Fatal(err)
		}
		return id
	}
	declare := func(owner int64, ownerModule, name, external string) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO rust_module_evidence(repo_id,file_id,owner_module,module_name,external_path,visibility) VALUES(?,?,?,?,?,?)`, repoID, owner, ownerModule, name, external, "private"); err != nil {
			t.Fatal(err)
		}
	}
	publicFn := func(file int64, name, qualified string) int64 {
		id, err := insertTestSymbolLang(ctx, s, repoID, file, name, qualified, "rust")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE symbols SET visibility='public' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i := range rustBudgetCrateCount {
		crate := fmt.Sprintf("c%04d", i)
		root := crate + "/lib.rs"
		f.roots[root] = struct{}{}
		lib := addFile(root, "crate")
		member := addFile(crate+"/m.rs", "crate::m")
		declare(lib, "crate", "m", crate+"/m")
		target := publicFn(member, "helper", "crate::m::helper")
		src := publicFn(lib, "run", "crate::run")
		edge, err := insertTestEdge(ctx, s, repoID, lib, src, "crate::m::helper")
		if err != nil {
			t.Fatal(err)
		}
		f.edgeByCrate[root] = edge
		f.wantTarget[edge] = target
		// The ambiguity: `shared/util` suffix-matches a file in the first crate
		// and a file in the last one, so no single membership is proven. The two
		// candidates deliberately land in different root batches.
		if i == 0 || i == rustBudgetCrateCount-1 {
			util := addFile(crate+"/shared/util.rs", "crate::util")
			publicFn(util, "shared", "crate::util::shared")
			if i == 0 {
				declare(lib, "crate", "util", "shared/util")
				f.ambiguous, err = insertTestEdge(ctx, s, repoID, lib, src, "crate::util::shared")
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return f
}

// TestRustScopeRootFanOutStaysInVariableBudget is the scale contract: an
// arbitrary number of affected crate roots must resolve inside the portable
// SQLite parameter budget, with a batch-proportional number of statements, and
// without any crate leaking into another.
func TestRustScopeRootFanOutStaysInVariableBudget(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	f := buildRustBudgetFixture(t, s, repo.ID)

	// Pre-change reproduction: one statement carrying every root would bind
	// three parameters per root plus the repo id, well past the limit.
	if unlimited := len(f.roots)*rustCrateRootPredicateParams + 1; unlimited <= sqliteDefaultMaxVariables {
		t.Fatalf("fixture binds %d parameters unbatched, want more than %d", unlimited, sqliteDefaultMaxVariables)
	}

	guard := newBudgetQuerier(s.db)
	scoped, err := rustScopedFileIDs(ctx, guard, repo.ID, sortedRustRoots(f.roots))
	if err != nil {
		t.Fatal(err)
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	if guard.maxArgs > sqliteInClauseBatchSize {
		t.Fatalf("max bound args = %d, want <= the working ceiling %d", guard.maxArgs, sqliteInClauseBatchSize)
	}
	// Two files per crate, plus the two ambiguous `shared/util` files.
	if want := rustBudgetCrateCount*2 + 2; len(scoped) != want {
		t.Fatalf("scoped %d files, want %d", len(scoped), want)
	}
	batchSize := sqliteBatchSize(1, rustCrateRootPredicateParams)
	wantBatches := (len(f.roots) + batchSize - 1) / batchSize
	if wantBatches < 2 {
		t.Fatalf("fixture fits one batch (%d roots, batch %d); the test proves nothing", len(f.roots), batchSize)
	}
	if got := len(guard.matching("SELECT f.id FROM files f")); got != wantBatches {
		t.Fatalf("root discovery ran %d statements, want %d", got, wantBatches)
	}
}

// TestRustScopeResolvesEveryCrateAcrossRootBatches proves batching is transport
// only: every crate resolves its own member, no crate binds another's, and the
// cross-batch ambiguity stays unresolved.
func TestRustScopeResolvesEveryCrateAcrossRootBatches(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	f := buildRustBudgetFixture(t, s, repo.ID)

	only := make(map[int64]struct{}, len(f.edgeByCrate)+1)
	for _, edge := range f.edgeByCrate {
		only[edge] = struct{}{}
	}
	only[f.ambiguous] = struct{}{}
	if _, err := s.resolveRustModuleScopeStandalone(ctx, repo.ID, only); err != nil {
		t.Fatal(err)
	}
	for root, edge := range f.edgeByCrate {
		var got int64
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(dst_symbol_id,0) FROM edges WHERE id=?`, edge).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != f.wantTarget[edge] {
			t.Fatalf("%s: edge bound to %d, want %d", root, got, f.wantTarget[edge])
		}
	}
	var ambiguous int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(dst_symbol_id,0) FROM edges WHERE id=?`, f.ambiguous).Scan(&ambiguous); err != nil {
		t.Fatal(err)
	}
	if ambiguous != 0 {
		t.Fatalf("cross-batch ambiguity bound to %d, want unresolved", ambiguous)
	}
}

// TestRustNamesForChangedPathsCoversEveryRootBatch pins the aggregation
// contract: the name set is complete, sorted, collapsed, and carries nothing
// from an unrelated crate.
func TestRustNamesForChangedPathsCoversEveryRootBatch(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	f := buildRustBudgetFixture(t, s, repo.ID)

	guard := newBudgetQuerier(s.db)
	names, err := rustNamesForChangedPaths(ctx, guard, repo.ID, f.roots)
	if err != nil {
		t.Fatal(err)
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	// Every crate emits the same two dst_names, so DISTINCT over the union must
	// collapse to exactly those, sorted.
	want := []string{"crate::m::helper", "crate::util::shared"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i, name := range want {
		if names[i] != name {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}

	// One crate's roots must not surface another crate's names.
	single := map[string]struct{}{"c0001/lib.rs": {}}
	if _, err := s.db.ExecContext(ctx, `UPDATE file_scope_evidence SET crate_root='c0001/lib.rs' WHERE repo_id=? AND file_id IN (SELECT id FROM files WHERE repo_id=? AND path LIKE 'c0001/%')`, repo.ID, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE file_scope_evidence SET crate_root='c0002/lib.rs' WHERE repo_id=? AND file_id IN (SELECT id FROM files WHERE repo_id=? AND path LIKE 'c0002/%')`, repo.ID, repo.ID); err != nil {
		t.Fatal(err)
	}
	scopedNames, err := rustNamesForChangedPaths(ctx, s.db, repo.ID, single)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopedNames) != 1 || scopedNames[0] != "crate::m::helper" {
		t.Fatalf("scoped names = %v, want only crate::m::helper", scopedNames)
	}
}

// TestInvalidateRustBindingsForRootsClearsEveryBatch proves invalidation is
// complete past the first root batch and leaves unrelated crates bound.
func TestInvalidateRustBindingsForRootsClearsEveryBatch(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	f := buildRustBudgetFixture(t, s, repo.ID)

	only := make(map[int64]struct{}, len(f.edgeByCrate))
	for _, edge := range f.edgeByCrate {
		only[edge] = struct{}{}
	}
	if _, err := s.resolveRustModuleScopeStandalone(ctx, repo.ID, only); err != nil {
		t.Fatal(err)
	}
	bound := func() int {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM edges WHERE repo_id=? AND resolution_strategy IN ('rust_module_scope','rust_use_scope','rust_associated_function')`, repo.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if bound() != rustBudgetCrateCount {
		t.Fatalf("bound %d edges before invalidation, want %d", bound(), rustBudgetCrateCount)
	}

	// Hold one crate back so completeness is not the same thing as "cleared
	// everything".
	keep := "c0399/lib.rs"
	affected := map[string]struct{}{}
	for root := range f.roots {
		if root != keep {
			affected[root] = struct{}{}
		}
	}
	guard := newBudgetQuerier(s.db)
	if err := invalidateRustBindingsForRoots(ctx, guard, repo.ID, affected); err != nil {
		t.Fatal(err)
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	batchSize := sqliteBatchSize(2, rustCrateRootPredicateParams)
	if want := (len(affected) + batchSize - 1) / batchSize; len(guard.matching("UPDATE edges SET")) != want {
		t.Fatalf("invalidation ran %d statements, want %d", len(guard.matching("UPDATE edges SET")), want)
	}
	if bound() != 1 {
		t.Fatalf("bound %d edges after invalidation, want only the held-back crate", bound())
	}
	var keptStrategy string
	if err := s.db.QueryRowContext(ctx, `SELECT resolution_strategy FROM edges WHERE id=?`, f.edgeByCrate[keep]).Scan(&keptStrategy); err != nil {
		t.Fatal(err)
	}
	if keptStrategy == "" {
		t.Fatalf("unrelated crate %s lost its binding", keep)
	}
}

// TestUpdateRustCrateRootsBindsCorrectGroups is the direct write contract:
// zero rows run nothing, one row lands, mixed empty and non-empty roots land,
// several batches all land, and no statement exceeds the budget.
func TestUpdateRustCrateRootsBindsCorrectGroups(t *testing.T) {
	ctx := context.Background()
	for _, rowCount := range []int{0, 1, 2, 3, sqliteBatchSize(1, 3) + 1, 1000} {
		t.Run(fmt.Sprintf("rows=%d", rowCount), func(t *testing.T) {
			s, repo := openBudgetStore(t)
			ids := make([]int64, 0, rowCount)
			roots := make(map[int64]string, rowCount)
			for i := range rowCount {
				path := fmt.Sprintf("src/f%05d.rs", i)
				id, err := insertTestFileLang(ctx, s, repo.ID, path, "rust")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.ExecContext(ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,module_path,crate_root) VALUES(?,?,?,?,?)`, repo.ID, id, "rust", "crate", "stale/lib.rs"); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
				// Every third file has lost its membership, which must be
				// persisted as an empty root rather than skipped.
				if i%3 == 0 {
					roots[id] = ""
				} else {
					roots[id] = fmt.Sprintf("src/lib%d.rs", i%2)
				}
			}
			guard := newBudgetQuerier(s.db)
			if err := updateRustCrateRoots(ctx, guard, repo.ID, ids, roots); err != nil {
				t.Fatal(err)
			}
			if guard.maxArgs > sqliteDefaultMaxVariables {
				t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
			}
			if guard.maxArgs > sqliteInClauseBatchSize {
				t.Fatalf("max bound args = %d, want <= the working ceiling %d", guard.maxArgs, sqliteInClauseBatchSize)
			}
			statements := len(guard.matching("UPDATE file_scope_evidence SET crate_root=CASE file_id"))
			batch := sqliteBatchSize(1, 3)
			if want := (rowCount + batch - 1) / batch; statements != want {
				t.Fatalf("ran %d statements for %d rows, want %d", statements, rowCount, want)
			}
			for i, id := range ids {
				var got string
				if err := s.db.QueryRowContext(ctx, `SELECT crate_root FROM file_scope_evidence WHERE repo_id=? AND file_id=?`, repo.ID, id).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != roots[id] {
					t.Fatalf("row %d: crate_root=%q, want %q", i, got, roots[id])
				}
			}
		})
	}
}

// TestUpdateRustCrateRootsReportsBatchFailure proves a failing batch surfaces
// as an error rather than a partially applied success.
func TestUpdateRustCrateRootsReportsBatchFailure(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	id, err := insertTestFileLang(ctx, s, repo.ID, "src/lib.rs", "rust")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE file_scope_evidence`); err != nil {
		t.Fatal(err)
	}
	if err := updateRustCrateRoots(ctx, s.db, repo.ID, []int64{id}, map[int64]string{id: "src/lib.rs"}); err == nil {
		t.Fatal("update reported success with no table to write to")
	}
}

// TestNameInvalidationKeepsUnaffectedRustCrates pins both directions of the
// crate-scope guard: a Rust binding inside the affected crates is cleared by
// the name pass, one in an unaffected crate is not (nothing would rebind it,
// and an unaffected crate's answer cannot have changed), and a Rust binding
// the crate-scoped pass does not own is still cleared so it can converge.
func TestNameInvalidationKeepsUnaffectedRustCrates(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	f := buildRustBudgetFixture(t, s, repo.ID)

	only := make(map[int64]struct{}, len(f.edgeByCrate))
	for _, edge := range f.edgeByCrate {
		only[edge] = struct{}{}
	}
	if _, err := s.resolveRustModuleScopeStandalone(ctx, repo.ID, only); err != nil {
		t.Fatal(err)
	}
	// A legacy binding on a Rust file, written by a strategy the Rust pass does
	// not own. It must still be cleared, or it can never converge.
	legacy := f.edgeByCrate["c0100/lib.rs"]
	if _, err := s.db.ExecContext(ctx, `UPDATE edges SET resolution_strategy=? WHERE id=?`, ResolutionStrategyDotSuffix, legacy); err != nil {
		t.Fatal(err)
	}

	affected := map[string]struct{}{"c0000/lib.rs": {}, "c0100/lib.rs": {}}
	scope, err := s.rustScopedFileIDs(ctx, repo.ID, affected)
	if err != nil {
		t.Fatal(err)
	}
	inScope := f.edgeByCrate["c0000/lib.rs"]
	outOfScope := f.edgeByCrate["c0399/lib.rs"]
	if _, err := s.invalidateNameEvidenceBindings(ctx, repo.ID, []string{"crate::m::helper"}, scope); err != nil {
		t.Fatal(err)
	}
	strategy := func(edge int64) string {
		var got string
		if err := s.db.QueryRowContext(ctx, `SELECT resolution_strategy FROM edges WHERE id=?`, edge).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := strategy(inScope); got != "" {
		t.Fatalf("in-scope crate kept strategy %q, want it cleared", got)
	}
	if got := strategy(outOfScope); got == "" {
		t.Fatal("unaffected crate lost its binding to the repo-wide name pass")
	}
	if got := strategy(legacy); got != "" {
		t.Fatalf("legacy %q binding on a Rust file survived, want it cleared", got)
	}
}
