package store

import (
	"testing"
)

// TestMigration027RetiresBareTailBindings: an existing database carries
// bindings written by the retired `bare_tail` evidence level. Migration 027
// must clear exactly the unsafe population -- import-path spellings degraded to
// their tail, and destinations of a kind a call edge cannot denote -- and
// relabel the safe remainder as the `exact_name` it always was, without
// dropping it.
func TestMigration027RetiresBareTailBindings(t *testing.T) {
	f := newGateFixture(t)

	callerFile := f.file(t, "internal/doctor/doctor.go", "go")
	caller := f.symbol(t, callerFile, "inspectDB", "doctor.inspectDB", "go")

	storeFile := f.file(t, "internal/store/store.go", "go")
	projectOpen := f.symbol(t, storeFile, "Open", "store.Open", "go")

	pyFile := f.file(t, "app/lib.py", "python")
	pySrc := f.symbol(t, pyFile, "run", "main.run", "python")
	pyHelper := f.symbol(t, pyFile, "helper", "lib.helper", "python")
	pyValue := f.symbolKind(t, pyFile, "Text", "consts.Text", "value", "python")

	insertBound := func(fileID, srcID, dstID int64, dstName, edgeKind, strategy string) int64 {
		res, err := f.store.db.ExecContext(f.ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line,
				resolution_strategy, resolution_confidence)
			VALUES(?, ?, ?, ?, ?, '', ?, 1, ?, ?)`,
			f.repoID, srcID, dstID, dstName, edgeKind, fileID, strategy, resolutionConfidenceFor(strategy))
		if err != nil {
			t.Fatalf("insert bound edge %q: %v", dstName, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId: %v", err)
		}
		return id
	}

	// Unsafe: an import outside the module, degraded to its tail.
	stdlibTail := insertBound(callerFile, caller, projectOpen, "database/sql.Open", "call", ResolutionStrategyBareTail)
	externalTail := insertBound(callerFile, caller, projectOpen, "github.com/acme/lib.Open", "call", ResolutionStrategyBareTail)
	// Unsafe: a non-Go expression that merely contains a slash.
	exprTail := insertBound(pyFile, pySrc, pyHelper, `(self.dir / "x").helper`, "call", ResolutionStrategyBareTail)
	// Unsafe: a destination kind no call edge can denote.
	nonCallable := insertBound(pyFile, pySrc, pyValue, "Text", "call", ResolutionStrategyBareTail)

	// Safe: a bare spelling that bound a callable. Kept, relabelled.
	safeBare := insertBound(pyFile, pySrc, pyHelper, "helper", "call", ResolutionStrategyBareTail)
	// Safe, but a GO source: a fresh index answers a bare Go spelling with the
	// package-scoped pass, so the correct label is go_package_scope. Migration
	// 026 has already cleared the Go bare rows that are not same-package, so
	// every Go row still labelled bare_tail here is one of these.
	goSameFile := f.file(t, "internal/store/helper.go", "go")
	goHelper := f.symbol(t, goSameFile, "helper", "store.helper", "go")
	goCaller := f.symbol(t, goSameFile, "callHelper", "store.callHelper", "go")
	safeGoBare := insertBound(goSameFile, goCaller, goHelper, "helper", "call", ResolutionStrategyBareTail)
	// Untouched: another strategy's row, and an explicit cross-language link.
	otherStrategy := insertBound(pyFile, pySrc, pyHelper, "lib.helper", "call", ResolutionStrategyDotTail2)
	crossLang := insertBound(pyFile, pySrc, pyHelper, "helper", EdgeKindCrossLanguageRef, ResolutionStrategyCrossLanguageSharedName)

	migrate := func() {
		t.Helper()
		if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM schema_migrations WHERE version = 27`); err != nil {
			t.Fatalf("reset migration 27: %v", err)
		}
		if err := f.store.Migrate(); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	migrate()

	assertUnresolvedNoMetadata(t, f, stdlibTail)
	assertUnresolvedNoMetadata(t, f, externalTail)
	assertUnresolvedNoMetadata(t, f, exprTail)
	assertUnresolvedNoMetadata(t, f, nonCallable)

	assertResolvedWithStrategy(t, f, safeBare, pyHelper, ResolutionStrategyExactName)
	assertResolvedWithStrategy(t, f, safeGoBare, goHelper, ResolutionStrategyGoPackageScope)
	assertResolvedWithStrategy(t, f, otherStrategy, pyHelper, ResolutionStrategyDotTail2)
	assertResolvedWithStrategy(t, f, crossLang, pyHelper, ResolutionStrategyCrossLanguageSharedName)

	// Idempotent: no `bare_tail` row survives, so a second application is a
	// no-op on every row above.
	migrate()
	assertResolvedWithStrategy(t, f, safeBare, pyHelper, ResolutionStrategyExactName)
	assertResolvedWithStrategy(t, f, safeGoBare, goHelper, ResolutionStrategyGoPackageScope)
	assertResolvedWithStrategy(t, f, otherStrategy, pyHelper, ResolutionStrategyDotTail2)
	assertResolvedWithStrategy(t, f, crossLang, pyHelper, ResolutionStrategyCrossLanguageSharedName)
	assertUnresolvedNoMetadata(t, f, stdlibTail)

	var left int
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT COUNT(*) FROM edges WHERE resolution_strategy = 'bare_tail'`).Scan(&left); err != nil {
		t.Fatalf("count bare_tail: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d bare_tail rows survived the migration", left)
	}
}
