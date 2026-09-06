package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
)

// rustCrateRootFixture builds a Rust repository whose crate_root column starts
// empty everywhere, so the resolver's own persistence is the only thing that
// can fill it in. root is a conventional crate root; each module gets a
// `mod <name>;` declaration in that root, which is the primary membership
// evidence.
type rustCrateRootFixture struct {
	store   *Store
	repoID  int64
	rootID  int64
	modules map[string]int64
}

func newRustCrateRootFixture(t *testing.T, ctx context.Context, root string, modules ...string) *rustCrateRootFixture {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &rustCrateRootFixture{store: s, repoID: repo.ID, modules: map[string]int64{}}
	f.rootID = f.addFile(t, ctx, root, "crate")
	dir := root[:len(root)-len(filepath.Base(root))]
	for _, name := range modules {
		f.modules[name] = f.addFile(t, ctx, dir+name+".rs", "crate::"+name)
		f.declare(t, ctx, f.rootID, "crate", name, dir+name)
	}
	return f
}

// addFile inserts a Rust file with empty crate_root: unproven membership.
func (f *rustCrateRootFixture) addFile(t *testing.T, ctx context.Context, path, module string) int64 {
	t.Helper()
	id, err := insertTestFileLang(ctx, f.store, f.repoID, path, "rust")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,module_path,crate_root) VALUES(?,?,?,?,'')`, f.repoID, id, "rust", module); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *rustCrateRootFixture) declare(t *testing.T, ctx context.Context, owner int64, ownerModule, name, external string) {
	t.Helper()
	if _, err := f.store.db.ExecContext(ctx, `INSERT INTO rust_module_evidence(repo_id,file_id,owner_module,module_name,external_path,visibility) VALUES(?,?,?,?,?,?)`, f.repoID, owner, ownerModule, name, external, "private"); err != nil {
		t.Fatal(err)
	}
}

func (f *rustCrateRootFixture) setCrateRoot(t *testing.T, ctx context.Context, fileID int64, root string) {
	t.Helper()
	if _, err := f.store.db.ExecContext(ctx, `UPDATE file_scope_evidence SET crate_root=? WHERE repo_id=? AND file_id=?`, root, f.repoID, fileID); err != nil {
		t.Fatal(err)
	}
}

// crateRoots reads back the persisted membership keyed by file path.
func (f *rustCrateRootFixture) crateRoots(t *testing.T, ctx context.Context) map[string]string {
	t.Helper()
	rows, err := f.store.db.QueryContext(ctx, `SELECT fl.path,e.crate_root FROM file_scope_evidence e JOIN files fl ON fl.id=e.file_id WHERE e.repo_id=? AND e.language='rust'`, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, root string
		if err := rows.Scan(&path, &root); err != nil {
			t.Fatal(err)
		}
		out[filepathSlash(path)] = filepathSlash(root)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *rustCrateRootFixture) resolveAll(t *testing.T, ctx context.Context) {
	t.Helper()
	tx, err := f.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRustModuleScope(ctx, tx, f.repoID, nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("resolve: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestRustCrateRootPersistenceWritesEveryBatch is the contract test for the
// crate-root CASE update: every proven membership must land, at one row, at
// several rows, and across more rows than one statement's parameter budget
// allows.
func TestRustCrateRootPersistenceWritesEveryBatch(t *testing.T) {
	ctx := context.Background()
	// One module past the CASE writer's batch boundary is what proves batching;
	// the root file makes it batch+1 rows in total. Larger fixtures prove the
	// same thing and only cost runtime.
	for _, moduleCount := range []int{0, 1, 2, sqliteBatchSize(1, 3)} {
		t.Run(fmt.Sprintf("modules=%d", moduleCount), func(t *testing.T) {
			modules := make([]string, 0, moduleCount)
			for i := 0; i < moduleCount; i++ {
				modules = append(modules, fmt.Sprintf("m%04d", i))
			}
			f := newRustCrateRootFixture(t, ctx, "src/lib.rs", modules...)
			f.resolveAll(t, ctx)
			got := f.crateRoots(t, ctx)
			if got["src/lib.rs"] != "src/lib.rs" {
				t.Fatalf("crate root file: crate_root=%q, want %q", got["src/lib.rs"], "src/lib.rs")
			}
			for _, name := range modules {
				path := "src/" + name + ".rs"
				if got[path] != "src/lib.rs" {
					t.Fatalf("%s: crate_root=%q, want %q", path, got[path], "src/lib.rs")
				}
			}
			if len(got) != moduleCount+1 {
				t.Fatalf("evidence rows = %d, want %d", len(got), moduleCount+1)
			}
		})
	}
}

// TestRustCrateRootClearsLostMembership pins the lifecycle contract: crate_root
// is derived evidence of *current* proven membership, so a file that loses its
// `mod` declaration loses its persisted root too.
func TestRustCrateRootClearsLostMembership(t *testing.T) {
	ctx := context.Background()
	f := newRustCrateRootFixture(t, ctx, "src/lib.rs", "helper")
	f.resolveAll(t, ctx)
	if got := f.crateRoots(t, ctx)["src/helper.rs"]; got != "src/lib.rs" {
		t.Fatalf("crate_root=%q before removal, want %q", got, "src/lib.rs")
	}
	if _, err := f.store.db.ExecContext(ctx, `DELETE FROM rust_module_evidence WHERE repo_id=?`, f.repoID); err != nil {
		t.Fatal(err)
	}
	f.resolveAll(t, ctx)
	if got := f.crateRoots(t, ctx)["src/helper.rs"]; got != "" {
		t.Fatalf("crate_root=%q after the declaration was removed, want it cleared", got)
	}
}

// TestRustCrateRootDoesNotProveItself is the semantic half: a persisted
// crate_root with no surviving declaration must not bootstrap itself into
// continued membership, or an incremental run resolves edges a fresh index
// leaves unresolved.
func TestRustCrateRootDoesNotProveItself(t *testing.T) {
	ctx := context.Background()
	f := newRustCrateRootFixture(t, ctx, "src/lib.rs")
	helper := f.addFile(t, ctx, "src/helper.rs", "crate::helper")
	f.setCrateRoot(t, ctx, helper, "src/lib.rs")
	target, err := insertTestSymbolLang(ctx, f.store, f.repoID, helper, "helper", "crate::helper::helper", "rust")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(ctx, `UPDATE symbols SET visibility='public' WHERE id=?`, target); err != nil {
		t.Fatal(err)
	}
	src, err := insertTestSymbolLang(ctx, f.store, f.repoID, f.rootID, "run", "crate::run", "rust")
	if err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, f.store, f.repoID, f.rootID, src, "crate::helper::helper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.resolveRustModuleScopeStandalone(ctx, f.repoID, map[int64]struct{}{edge: {}}); err != nil {
		t.Fatal(err)
	}
	var dst any
	var strategy string
	if err := f.store.db.QueryRowContext(ctx, `SELECT dst_symbol_id,resolution_strategy FROM edges WHERE id=?`, edge).Scan(&dst, &strategy); err != nil {
		t.Fatal(err)
	}
	if dst != nil {
		t.Fatalf("edge bound to %v via %q on a stale crate_root alone, want unresolved", dst, strategy)
	}
	if got := f.crateRoots(t, ctx)["src/helper.rs"]; got != "" {
		t.Fatalf("stale crate_root=%q survived recomputation, want it cleared", got)
	}
}

// TestRustCrateRootKeepsCratesIsolated proves the write does not leak
// membership across independent crate roots when both spell the same module
// names.
func TestRustCrateRootKeepsCratesIsolated(t *testing.T) {
	ctx := context.Background()
	f := newRustCrateRootFixture(t, ctx, "src/lib.rs")
	alt := f.addFile(t, ctx, "alt/main.rs", "crate")
	// This test's contract is cross-crate isolation of identical module names,
	// not batching -- the CASE writer's batch behaviour is proven by
	// TestRustCrateRootPersistenceWritesEveryBatch and
	// TestUpdateRustCrateRootsBindsCorrectGroups, and isolation at batch scale
	// by TestRustScopeResolvesEveryCrateAcrossRootBatches.
	const perCrate = 3
	want := map[string]string{"src/lib.rs": "src/lib.rs", "alt/main.rs": "alt/main.rs"}
	for i := 0; i < perCrate; i++ {
		name := fmt.Sprintf("m%04d", i)
		f.addFile(t, ctx, "src/"+name+".rs", "crate::"+name)
		f.declare(t, ctx, f.rootID, "crate", name, "src/"+name)
		want["src/"+name+".rs"] = "src/lib.rs"
		f.addFile(t, ctx, "alt/"+name+".rs", "crate::"+name)
		f.declare(t, ctx, alt, "crate", name, "alt/"+name)
		want["alt/"+name+".rs"] = "alt/main.rs"
	}
	f.resolveAll(t, ctx)
	got := f.crateRoots(t, ctx)
	if len(got) != len(want) {
		t.Fatalf("evidence rows = %d, want %d", len(got), len(want))
	}
	paths := make([]string, 0, len(want))
	for path := range want {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if got[path] != want[path] {
			t.Fatalf("%s: crate_root=%q, want %q", path, got[path], want[path])
		}
	}
}

// TestRustCrateRootConvergesWithFullRecompute is the fresh/incremental contract
// on the persisted column itself: whatever a scoped pass leaves behind, a full
// recompute over the same evidence must agree with it exactly.
func TestRustCrateRootConvergesWithFullRecompute(t *testing.T) {
	ctx := context.Background()
	f := newRustCrateRootFixture(t, ctx, "src/lib.rs", "a", "b", "c")
	f.resolveAll(t, ctx)
	afterFull := f.crateRoots(t, ctx)

	// Drop one declaration and rerun the scoped pass the way an update would.
	if _, err := f.store.db.ExecContext(ctx, `DELETE FROM rust_module_evidence WHERE repo_id=? AND module_name='b'`, f.repoID); err != nil {
		t.Fatal(err)
	}
	src, err := insertTestSymbolLang(ctx, f.store, f.repoID, f.rootID, "run", "crate::run", "rust")
	if err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, f.store, f.repoID, f.rootID, src, "crate::b::helper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.resolveRustModuleScopeStandalone(ctx, f.repoID, map[int64]struct{}{edge: {}}); err != nil {
		t.Fatal(err)
	}
	afterIncremental := f.crateRoots(t, ctx)
	if afterIncremental["src/b.rs"] != "" {
		t.Fatalf("src/b.rs kept crate_root %q after its declaration went away", afterIncremental["src/b.rs"])
	}
	if afterIncremental["src/a.rs"] != "src/lib.rs" || afterIncremental["src/c.rs"] != "src/lib.rs" {
		t.Fatalf("untouched modules lost membership: %v", afterIncremental)
	}

	f.resolveAll(t, ctx)
	afterRecompute := f.crateRoots(t, ctx)
	if len(afterRecompute) != len(afterIncremental) {
		t.Fatalf("recompute has %d rows, incremental had %d", len(afterRecompute), len(afterIncremental))
	}
	for path, root := range afterIncremental {
		if afterRecompute[path] != root {
			t.Fatalf("%s: incremental %q, full recompute %q", path, root, afterRecompute[path])
		}
	}
	if afterFull["src/b.rs"] != "src/lib.rs" {
		t.Fatalf("fixture never proved src/b.rs in the first place: %v", afterFull)
	}
}

// TestRustCrateRootDiscoversWindowsStoredPaths pins the separator half of the
// discovery predicate. crate_root is always written slashed, but files.path
// still holds whatever separator the indexing host used, so a slash-only
// predicate loses every blank-root file on a Windows-written database -- which
// is the branch that rediscovers a file whose `mod` declaration has just been
// restored.
func TestRustCrateRootDiscoversWindowsStoredPaths(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, spec := range []struct{ path, module string }{
		{`src\lib.rs`, "crate"},
		{`src\util.rs`, "crate::util"},
		{`other\lib.rs`, "crate"},
	} {
		id, err := insertTestFileLang(ctx, s, repo.ID, spec.path, "rust")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,module_path,crate_root) VALUES(?,?,?,?,'')`, repo.ID, id, "rust", spec.module); err != nil {
			t.Fatal(err)
		}
		ids[spec.path] = id
	}
	scoped, err := s.rustScopedFileIDs(ctx, repo.ID, map[string]struct{}{"src/lib.rs": {}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{`src\lib.rs`, `src\util.rs`} {
		if _, ok := scoped[ids[path]]; !ok {
			t.Fatalf("%s was not discovered: %v", path, scoped)
		}
	}
	if _, ok := scoped[ids[`other\lib.rs`]]; ok {
		t.Fatalf(`other\lib.rs leaked into the src crate's scope: %v`, scoped)
	}
}

// TestRustCrateRootRestoresMembershipOnWindowsPaths walks the full lifecycle a
// Windows-written database goes through: membership proven, lost when the `mod`
// declaration goes away, and regained when it comes back. Step three is the one
// that depends on the blank-root discovery branch, because by then the file's
// cached crate_root is empty and only its path can bring it back into scope.
func TestRustCrateRootRestoresMembershipOnWindowsPaths(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	addFile := func(path, module string) int64 {
		id, err := insertTestFileLang(ctx, s, repo.ID, path, "rust")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,module_path,crate_root) VALUES(?,?,?,?,'')`, repo.ID, id, "rust", module); err != nil {
			t.Fatal(err)
		}
		return id
	}
	declare := func(owner int64) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO rust_module_evidence(repo_id,file_id,owner_module,module_name,external_path,visibility) VALUES(?,?,?,?,?,?)`, repo.ID, owner, "crate", "util", "src/util", "private"); err != nil {
			t.Fatal(err)
		}
	}
	crateRoot := func(fileID int64) string {
		var root string
		if err := s.db.QueryRowContext(ctx, `SELECT crate_root FROM file_scope_evidence WHERE repo_id=? AND file_id=?`, repo.ID, fileID).Scan(&root); err != nil {
			t.Fatal(err)
		}
		return root
	}

	lib := addFile(`src\lib.rs`, "crate")
	util := addFile(`src\util.rs`, "crate::util")
	declare(lib)
	target, err := insertTestSymbolLang(ctx, s, repo.ID, util, "helper", "crate::util::helper", "rust")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE symbols SET visibility='public' WHERE id=?`, target); err != nil {
		t.Fatal(err)
	}
	src, err := insertTestSymbolLang(ctx, s, repo.ID, lib, "run", "crate::run", "rust")
	if err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, s, repo.ID, lib, src, "crate::util::helper")
	if err != nil {
		t.Fatal(err)
	}
	scoped := func() {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `UPDATE edges SET `+resolverClearResolutionSQL+` WHERE id=?`, edge); err != nil {
			t.Fatal(err)
		}
		if _, err := s.resolveRustModuleScopeStandalone(ctx, repo.ID, map[int64]struct{}{edge: {}}); err != nil {
			t.Fatal(err)
		}
	}
	bound := func() int64 {
		var got int64
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(dst_symbol_id,0) FROM edges WHERE id=?`, edge).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	scoped()
	if crateRoot(util) != "src/lib.rs" || bound() != target {
		t.Fatalf("declared: crate_root=%q bound=%d, want %q and %d", crateRoot(util), bound(), "src/lib.rs", target)
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM rust_module_evidence WHERE repo_id=?`, repo.ID); err != nil {
		t.Fatal(err)
	}
	scoped()
	if crateRoot(util) != "" || bound() != 0 {
		t.Fatalf("removed: crate_root=%q bound=%d, want cleared and unresolved", crateRoot(util), bound())
	}

	declare(lib)
	scoped()
	if crateRoot(util) != "src/lib.rs" || bound() != target {
		t.Fatalf("restored: crate_root=%q bound=%d, want %q and %d", crateRoot(util), bound(), "src/lib.rs", target)
	}
}
