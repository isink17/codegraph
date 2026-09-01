package store

import "testing"

func TestFindCallersExactIDConflictingRawNameStaysInTargetScope(t *testing.T) {
	f := newGateFixture(t)
	aFile := f.file(t, "a/caller.go", "go")
	bFile := f.file(t, "b/caller.go", "go")
	targetFile := f.file(t, "b/target.go", "go")
	target := f.symbol(t, targetFile, "Foo", "b.Foo", "go")
	aCaller := f.symbol(t, aFile, "caller", "a.caller", "go")
	bCaller := f.symbol(t, bFile, "caller", "b.caller", "go")
	f.edge(t, aFile, aCaller, "a.Foo")
	f.edge(t, bFile, bCaller, "Foo")

	for _, spelling := range []string{"b.Foo", "Foo", "a.Foo", "::Foo", ""} {
		got, err := f.store.FindCallers(f.ctx, f.repoID, spelling, target, 20, 0)
		if err != nil {
			t.Fatalf("FindCallers(%q) error = %v", spelling, err)
		}
		assertQNames(t, "conflicting raw exact callers", got)
	}
}

func TestFindCallersExactIDKeepsResolvedEdge(t *testing.T) {
	f := newGateFixture(t)
	targetFile := f.file(t, "b/target.go", "go")
	callerFile := f.file(t, "a/caller.go", "go")
	target := f.symbol(t, targetFile, "Foo", "b.Foo", "go")
	caller := f.symbol(t, callerFile, "caller", "a.caller", "go")
	edge := f.edge(t, callerFile, caller, "Foo")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, target, edge); err != nil {
		t.Fatalf("bind edge: %v", err)
	}

	got, err := f.store.FindCallers(f.ctx, f.repoID, "unrelated.Foo", target, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers() error = %v", err)
	}
	assertQNames(t, "resolved exact callers", got, "a.caller")
}

func TestFindCallersExactIDForeignRepoFailsClosed(t *testing.T) {
	f := newGateFixture(t)
	callFile := f.file(t, "a/caller.go", "go")
	caller := f.symbol(t, callFile, "caller", "a.caller", "go")
	f.edge(t, callFile, caller, "b.Foo")

	foreign, err := f.store.UpsertRepo(f.ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo(foreign) error = %v", err)
	}
	foreignFile, err := insertTestFileLang(f.ctx, f.store, foreign.ID, "b/target.go", "go")
	if err != nil {
		t.Fatalf("insert foreign file: %v", err)
	}
	foreignID, err := insertTestSymbolLang(f.ctx, f.store, foreign.ID, foreignFile, "Foo", "b.Foo", "go")
	if err != nil {
		t.Fatalf("insert foreign symbol: %v", err)
	}

	got, err := f.store.FindCallers(f.ctx, f.repoID, "b.Foo", foreignID, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers() error = %v", err)
	}
	assertQNames(t, "foreign exact callers", got)
}

func TestFindCalleesExactIDStaleAndMissingFailClosed(t *testing.T) {
	f := newGateFixture(t)
	file := f.file(t, "a/caller.go", "go")
	source := f.symbol(t, file, "Caller", "a.Caller", "go")
	f.edge(t, file, source, "Foo")

	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE id = ?`, source); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	for _, id := range []int64{source, 999999999} {
		got, err := f.store.FindCallees(f.ctx, f.repoID, "Caller", id, 20, 0)
		if err != nil {
			t.Fatalf("FindCallees(%d) error = %v", id, err)
		}
		assertQNames(t, "exact callees", got)
	}
}

func TestFindCalleesExactIDForeignRepoFailsClosed(t *testing.T) {
	f := newGateFixture(t)
	file := f.file(t, "a/caller.go", "go")
	targetFile := f.file(t, "a/target.go", "go")
	target := f.symbol(t, targetFile, "Foo", "a.Foo", "go")

	foreign, err := f.store.UpsertRepo(f.ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo(foreign) error = %v", err)
	}
	foreignFile, err := insertTestFileLang(f.ctx, f.store, foreign.ID, "b/caller.go", "go")
	if err != nil {
		t.Fatalf("insert foreign file: %v", err)
	}
	foreignID, err := insertTestSymbolLang(f.ctx, f.store, foreign.ID, foreignFile, "Caller", "b.Caller", "go")
	if err != nil {
		t.Fatalf("insert foreign symbol: %v", err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, file_id, line)
		VALUES (?, ?, ?, 'Foo', 'calls', ?, 1)
	`, f.repoID, foreignID, target, file); err != nil {
		t.Fatalf("insert cross-repo edge: %v", err)
	}

	got, err := f.store.FindCallees(f.ctx, f.repoID, "Caller", foreignID, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees() error = %v", err)
	}
	assertQNames(t, "foreign exact callees", got)
}
