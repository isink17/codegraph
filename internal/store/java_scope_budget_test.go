package store

import (
	"context"
	"testing"
)

// buildJavaBudgetFixture writes one caller file holding edgeCount `Util.run()`
// edges, resolved through a wildcard import of the package that declares Util.
// Concentrating the edges in a single file is the point: a scoped pass batches
// the edge ids, and any evidence load routed through those ids would select the
// caller file once per batch and duplicate its wildcard import -- which
// javaType reads as two candidates, and therefore as ambiguity.
func buildJavaBudgetFixture(t *testing.T, s *Store, repoID int64, edgeCount int) map[int64]struct{} {
	t.Helper()
	ctx := context.Background()
	owner, err := insertTestFileLang(ctx, s, repoID, "a/Util.java", "java")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`,
		repoID, owner, "java", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestSymbolKind(ctx, s, repoID, owner, "Util", "a.Util", "type", "a", "java"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestSymbolKind(ctx, s, repoID, owner, "run", "a.Util.run", "function", "Util", "java"); err != nil {
		t.Fatal(err)
	}

	caller, err := insertTestFileLang(ctx, s, repoID, "b/Caller.java", "java")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`,
		repoID, caller, "java", "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard,is_static) VALUES(?,?,?,?,?,?,?,1,0)`,
		repoID, caller, "java", "a", "", "", "wildcard"); err != nil {
		t.Fatal(err)
	}
	src, err := insertTestSymbolKind(ctx, s, repoID, caller, "x", "b.Caller.x", "function", "Caller", "java")
	if err != nil {
		t.Fatal(err)
	}
	only := map[int64]struct{}{}
	for range edgeCount {
		id, err := insertTestEdge(ctx, s, repoID, caller, src, "Util.run")
		if err != nil {
			t.Fatal(err)
		}
		only[id] = struct{}{}
	}
	return only
}

// TestJavaScopeScopedPassStaysInVariableBudget pins both halves of the Java
// contract at scale: no statement exceeds the portable budget, and batching the
// edge ids neither drops nor duplicates the evidence a decision rests on.
func TestJavaScopeScopedPassStaysInVariableBudget(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	const edgeCount = 1200
	only := buildJavaBudgetFixture(t, s, repo.ID, edgeCount)

	guard := newBudgetQuerier(s.db)
	n, err := resolveJavaScope(ctx, guard, repo.ID, only)
	if err != nil {
		t.Fatal(err)
	}
	if n != edgeCount {
		t.Fatalf("scoped pass resolved %d edges, want %d", n, edgeCount)
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	batch := sqliteBatchSize(1, 1)
	edgeSelect := guard.matching("FROM edges e JOIN files f", "e.dst_symbol_id IS NULL")
	if want := (edgeCount + batch - 1) / batch; len(edgeSelect) != want {
		t.Fatalf("edge selection ran %d statements, want %d", len(edgeSelect), want)
	}
}

// TestJavaScopeScopedPassMatchesFullPass compares the batched scoped pass with
// the unfiltered pass over the same fixture.
func TestJavaScopeScopedPassMatchesFullPass(t *testing.T) {
	ctx := context.Background()
	resolved := func(scoped bool) int {
		s, repo := openBudgetStore(t)
		only := buildJavaBudgetFixture(t, s, repo.ID, 1200)
		if !scoped {
			only = nil
		}
		n, err := resolveJavaScope(ctx, s.db, repo.ID, only)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	full, scoped := resolved(false), resolved(true)
	if full == 0 {
		t.Fatal("the full pass resolved nothing; the fixture proves nothing")
	}
	if full != scoped {
		t.Fatalf("full pass resolved %d edges, scoped pass %d", full, scoped)
	}
}
