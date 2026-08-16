package store

import (
	"testing"
)

// TestMigration026ClearsCrossPackageGoBareBindings: an existing database carries
// bindings the retired repo-global bare-name match wrote. Re-applying migration
// 026 must clear exactly that class -- a bare Go spelling bound outside the
// calling symbol's own package, or bound to a method -- and leave same-package
// binds, other languages, and qualifier-bearing spellings untouched.
func TestMigration026ClearsCrossPackageGoBareBindings(t *testing.T) {
	f := newGateFixture(t)

	callerFile := f.file(t, "src/go/a/caller.go", "go")
	caller := f.symbol(t, callerFile, "Caller", "a.Caller", "go")

	ownFile := f.file(t, "src/go/a/own.go", "go")
	own := f.symbol(t, ownFile, "Helper", "a.Helper", "go")
	ownMethod := f.method(t, ownFile, "Close", "a.App.Close", "App", "go")

	foreignFile := f.file(t, "src/go/b/foreign.go", "go")
	foreign := f.symbol(t, foreignFile, "countTags", "b.countTags", "go")
	foreignExported := f.symbol(t, foreignFile, "NewThing", "b.NewThing", "go")

	// Same directory as package a, different package: an external test package
	// has no bare access to production declarations.
	externalTestFile := f.file(t, "src/go/a/caller_test.go", "go")
	externalTest := f.symbol(t, externalTestFile, "TestCaller", "a_test.TestCaller", "go")

	// Same directory, same package: an internal test file IS package a, and its
	// bare call is valid Go. The migration must tell the two apart by the
	// package clause rather than by the _test.go path.
	internalTestFile := f.file(t, "src/go/a/own_test.go", "go")
	internalTest := f.symbol(t, internalTestFile, "TestOwn", "a.TestOwn", "go")

	pyFile := f.file(t, "src/py/caller.py", "python")
	pySrc := f.symbol(t, pyFile, "py_caller", "py_caller", "python")
	pyDst := f.symbol(t, pyFile, "helper", "helper", "python")

	insertBound := func(fileID, srcID, dstID int64, dstName, strategy string) int64 {
		res, err := f.store.db.ExecContext(f.ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line,
				resolution_strategy, resolution_confidence)
			VALUES(?, ?, ?, ?, 'call', '', ?, 1, ?, ?)`,
			f.repoID, srcID, dstID, dstName, fileID, strategy, resolutionConfidenceFor(strategy))
		if err != nil {
			t.Fatalf("insert bound edge %q: %v", dstName, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId: %v", err)
		}
		return id
	}

	insertBoundKind := func(fileID, srcID, dstID int64, dstName, edgeKind, strategy string) int64 {
		res, err := f.store.db.ExecContext(f.ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line,
				resolution_strategy, resolution_confidence)
			VALUES(?, ?, ?, ?, ?, '', ?, 1, ?, ?)`,
			f.repoID, srcID, dstID, dstName, edgeKind, fileID, strategy, resolutionConfidenceFor(strategy))
		if err != nil {
			t.Fatalf("insert bound %s edge %q: %v", edgeKind, dstName, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId: %v", err)
		}
		return id
	}

	crossPackage := insertBound(callerFile, caller, foreign, "countTags", ResolutionStrategyExactName)
	crossPackageExported := insertBound(callerFile, caller, foreignExported, "NewThing", ResolutionStrategyExactName)
	methodStolen := insertBound(callerFile, caller, ownMethod, "Close", ResolutionStrategyReceiverMethod)
	externalTestReach := insertBound(externalTestFile, externalTest, own, "Helper", ResolutionStrategyExactName)

	samePackage := insertBound(callerFile, caller, own, "Helper", ResolutionStrategyExactName)
	internalTestKept := insertBound(internalTestFile, internalTest, own, "Helper", ResolutionStrategyExactName)
	qualifiedKept := insertBound(callerFile, caller, foreign, "b.countTags", ResolutionStrategyDotTail2)
	pythonKept := insertBound(pyFile, pySrc, pyDst, "helper", ResolutionStrategyExactName)
	// Cross-language links are written already bound by their own pass, out of
	// import-bridge evidence, and no resolver strategy ever reconsiders them.
	// Clearing one here would leave a row the next cross-language run deletes.
	crossLangKept := insertBoundKind(callerFile, caller, pyDst, "helper",
		EdgeKindCrossLanguageRef, ResolutionStrategyCrossLanguageSharedName)

	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM schema_migrations WHERE version = 26`); err != nil {
		t.Fatalf("reset migration 26: %v", err)
	}
	if err := f.store.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	assertUnresolvedNoMetadata(t, f, crossPackage)
	assertUnresolvedNoMetadata(t, f, crossPackageExported)
	assertUnresolvedNoMetadata(t, f, methodStolen)
	assertUnresolvedNoMetadata(t, f, externalTestReach)

	assertResolvedWithStrategy(t, f, samePackage, own, ResolutionStrategyExactName)
	assertResolvedWithStrategy(t, f, internalTestKept, own, ResolutionStrategyExactName)
	assertResolvedWithStrategy(t, f, qualifiedKept, foreign, ResolutionStrategyDotTail2)
	assertResolvedWithStrategy(t, f, pythonKept, pyDst, ResolutionStrategyExactName)
	assertResolvedWithStrategy(t, f, crossLangKept, pyDst, ResolutionStrategyCrossLanguageSharedName)

	// Idempotent: a second application changes nothing, because every surviving
	// row already satisfies the rule.
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM schema_migrations WHERE version = 26`); err != nil {
		t.Fatalf("reset migration 26 again: %v", err)
	}
	if err := f.store.Migrate(); err != nil {
		t.Fatalf("Migrate (second pass): %v", err)
	}
	assertResolvedWithStrategy(t, f, samePackage, own, ResolutionStrategyExactName)
	assertResolvedWithStrategy(t, f, internalTestKept, own, ResolutionStrategyExactName)
	assertResolvedWithStrategy(t, f, qualifiedKept, foreign, ResolutionStrategyDotTail2)
	assertResolvedWithStrategy(t, f, pythonKept, pyDst, ResolutionStrategyExactName)
}
