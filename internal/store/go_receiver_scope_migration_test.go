package store

import (
	"testing"
)

// TestMigration035RepairsGoSelectorBindings: an existing database carries the
// high-confidence module_import edges the old selector handling produced,
// including the miswire where a local shadowed an import alias. File contents
// are unchanged, so nothing would ever ask for those files again.
//
// The migration must leave the database immediately safe -- no Go module_import
// binding survives to be queried as valid before the reparse happens -- and
// must force every Go file to be parsed again so the correct edges come back.
// Non-Go bindings and other strategies must be untouched.
func TestMigration035RepairsGoSelectorBindings(t *testing.T) {
	f := newGateFixture(t)

	goFile := f.file(t, "internal/app/main.go", "go")
	goCaller := f.symbol(t, goFile, "shadow", "main.shadow", "go")
	goTarget := f.symbol(t, goFile, "Get", "store.Get", "go")

	pyFile := f.file(t, "app/lib.py", "python")
	pySrc := f.symbol(t, pyFile, "run", "main.run", "python")
	pyHelper := f.symbol(t, pyFile, "helper", "lib.helper", "python")

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

	// The miswire, and a legitimate sibling written by the same old rule. The
	// migration cannot tell them apart without the evidence it is introducing,
	// so it clears both and lets the reparse rebuild the correct one.
	miswired := insertBound(goFile, goCaller, goTarget, "example.com/project/store.Get", ResolutionStrategyModuleImport)
	legitimate := insertBound(goFile, goCaller, goTarget, "example.com/project/store.Other", ResolutionStrategyModuleImport)
	// A Go binding from an unaffected strategy stays.
	goBare := insertBound(goFile, goCaller, goTarget, "Get", ResolutionStrategyGoPackageScope)
	// The mis-rewritten spelling was offered to every strategy, so a Go row
	// that another one took is wrong for the same reason and must go too.
	goSuffix := insertBound(goFile, goCaller, goTarget, "example.com/project/store.Third", ResolutionStrategyDotSuffix)
	// A non-Go binding is out of scope entirely.
	pyBound := insertBound(pyFile, pySrc, pyHelper, "lib.helper", ResolutionStrategyPythonImportScope)

	// Pretend the Go files were already parsed, as a pre-035 database would.
	if _, err := f.store.db.ExecContext(f.ctx,
		`UPDATE files SET content_sha256 = 'stale', parse_state = 'indexed'`); err != nil {
		t.Fatalf("seed parse state: %v", err)
	}

	migrate := func() {
		t.Helper()
		if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM schema_migrations WHERE version = 35`); err != nil {
			t.Fatalf("reset migration 35: %v", err)
		}
		if err := f.store.Migrate(); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	migrate()

	assertUnresolvedNoMetadata(t, f, miswired)
	assertUnresolvedNoMetadata(t, f, legitimate)
	assertUnresolvedNoMetadata(t, f, goSuffix)
	assertResolvedWithStrategy(t, f, goBare, goTarget, ResolutionStrategyGoPackageScope)
	assertResolvedWithStrategy(t, f, pyBound, pyHelper, ResolutionStrategyPythonImportScope)

	// Safety: nothing Go-sourced is left claiming module_import.
	var left int
	if err := f.store.db.QueryRowContext(f.ctx, `
		SELECT COUNT(*) FROM edges
		WHERE resolution_strategy = 'module_import'
		  AND file_id IN (SELECT id FROM files WHERE language = 'go')`).Scan(&left); err != nil {
		t.Fatalf("count go module_import: %v", err)
	}
	if left != 0 {
		t.Errorf("%d Go module_import bindings survived the migration", left)
	}

	// Every Go file must be queued for a reparse; other languages must not be.
	var goPending, pyPending int
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT COUNT(*) FROM files WHERE language = 'go' AND content_sha256 = '' AND parse_state = 'pending'`).Scan(&goPending); err != nil {
		t.Fatalf("count go pending: %v", err)
	}
	if goPending == 0 {
		t.Error("no Go file was queued for reparse; the new evidence would never be built")
	}
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT COUNT(*) FROM files WHERE language = 'python' AND parse_state = 'pending'`).Scan(&pyPending); err != nil {
		t.Fatalf("count python pending: %v", err)
	}
	if pyPending != 0 {
		t.Errorf("%d Python files were queued for reparse; migration 035 is Go-only", pyPending)
	}

	// Idempotent.
	migrate()
	assertUnresolvedNoMetadata(t, f, miswired)
	assertResolvedWithStrategy(t, f, goBare, goTarget, ResolutionStrategyGoPackageScope)
	assertResolvedWithStrategy(t, f, pyBound, pyHelper, ResolutionStrategyPythonImportScope)
}
