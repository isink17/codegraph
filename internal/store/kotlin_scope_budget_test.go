package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type kotlinBudgetFixture struct {
	fileIDs []int64
	edgeIDs []int64
	// resolvable counts the edges that name the shared helper; the rest name
	// nothing and only widen the candidate name set.
	resolvable int
	edges      map[int64]struct{}
}

// buildKotlinBudgetFixture writes callerCount Kotlin files, each importing and
// calling the same helper from its own package and also naming an unresolvable
// symbol. Every file carries scope evidence, so the file, edge, candidate-name
// and package sets all hold more items than one statement may bind.
func buildKotlinBudgetFixture(t *testing.T, s *Store, repoID int64, callerCount int) kotlinBudgetFixture {
	t.Helper()
	ctx := context.Background()
	target, err := insertTestFileLang(ctx, s, repoID, "app/Helpers.kt", "kotlin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`,
		repoID, target, "kotlin", "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestSymbolKind(ctx, s, repoID, target, "load", "app.load", "function", "", "kotlin"); err != nil {
		t.Fatal(err)
	}
	out := kotlinBudgetFixture{edges: map[int64]struct{}{}}
	for i := range callerCount {
		file, err := insertTestFileLang(ctx, s, repoID, fmt.Sprintf("app/Caller%04d.kt", i), "kotlin")
		if err != nil {
			t.Fatal(err)
		}
		out.fileIDs = append(out.fileIDs, file)
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`,
			repoID, file, "kotlin", fmt.Sprintf("app.p%04d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard) VALUES(?,?,?,?,?,?,?,0)`,
			repoID, file, "kotlin", "app.load", "load", "load", "named"); err != nil {
			t.Fatal(err)
		}
		src, err := insertTestSymbolKind(ctx, s, repoID, file, "run", fmt.Sprintf("app.Caller%04d.run", i), "function", "", "kotlin")
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"load", fmt.Sprintf("absent%04d", i)} {
			id, err := insertTestEdge(ctx, s, repoID, file, src, name)
			if err != nil {
				t.Fatal(err)
			}
			out.edgeIDs = append(out.edgeIDs, id)
			out.edges[id] = struct{}{}
		}
		out.resolvable++
	}
	return out
}

// TestKotlinScopeStaysInVariableBudget is the Kotlin counterpart of the
// TypeScript budget case: a scoped pass over more edges and files than one
// statement may carry must still consider all of them, with no statement over
// the portable budget and no id list inlined into the SQL text.
func TestKotlinScopeStaysInVariableBudget(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	fixture := buildKotlinBudgetFixture(t, s, repo.ID, 1100)

	guard := newBudgetQuerier(s.db)
	n, err := resolveKotlinScope(ctx, guard, repo.ID, fixture.edges)
	if err != nil {
		t.Fatal(err)
	}
	if n != fixture.resolvable {
		t.Fatalf("resolved %d edges, want %d", n, fixture.resolvable)
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	for _, st := range guard.statements {
		sql := collapseSQL(st.sql)
		if strings.Contains(sql, "IN ()") {
			t.Fatalf("emitted an empty IN list: %s", sql)
		}
	}
	edgeSelect := guard.matching("FROM edges e JOIN files f", "e.dst_symbol_id IS NULL")
	if len(edgeSelect) == 0 {
		t.Fatal("no scoped edge selection ran")
	}
	last := fixture.edgeIDs[len(fixture.edgeIDs)-1]
	if statementBinding(edgeSelect, last) < 0 {
		t.Fatalf("edge %d was never named by a scoped edge selection", last)
	}
	// The evidence loads must bind the last file id too: no file may be dropped
	// between batches.
	imports := guard.matching("FROM scope_import_evidence", "file_id IN (")
	if len(imports) == 0 {
		t.Fatal("no import evidence load ran")
	}
	lastFile := fixture.fileIDs[len(fixture.fileIDs)-1]
	if statementBinding(imports, lastFile) < 0 {
		t.Fatalf("file %d was never named by an import evidence load", lastFile)
	}
}

// TestKotlinScopeLargeScopedPassMatchesFullPass pins that splitting the edge,
// file, name and package sets across batches changes no resolver decision. The
// fixture crosses every one of those batch boundaries.
func TestKotlinScopeLargeScopedPassMatchesFullPass(t *testing.T) {
	ctx := context.Background()
	resolutions := func(scoped bool) map[string]int {
		s, repo := openBudgetStore(t)
		fixture := buildKotlinBudgetFixture(t, s, repo.ID, 1100)
		only := map[int64]struct{}(nil)
		if scoped {
			only = fixture.edges
		}
		if _, err := resolveKotlinScope(ctx, s.db, repo.ID, only); err != nil {
			t.Fatal(err)
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT COALESCE(e.resolution_strategy,''),COUNT(*) FROM edges e JOIN files f ON f.id=e.file_id
			 WHERE e.repo_id=? AND f.language='kotlin' AND e.dst_symbol_id IS NOT NULL
			 GROUP BY e.resolution_strategy`, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var strategy string
			var count int
			if err := rows.Scan(&strategy, &count); err != nil {
				t.Fatal(err)
			}
			out[strategy] = count
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	full, scoped := resolutions(false), resolutions(true)
	if len(full) == 0 {
		t.Fatal("the full pass resolved nothing; the fixture proves nothing")
	}
	if len(full) != len(scoped) {
		t.Fatalf("full pass produced %v, scoped pass %v", full, scoped)
	}
	for strategy, count := range full {
		if scoped[strategy] != count {
			t.Fatalf("strategy %q: full=%d scoped=%d", strategy, count, scoped[strategy])
		}
	}
}

// TestKotlinScopeFileSelectionIsBatched covers the caller side: the file-scope
// evidence selection that decides which Kotlin edges reach resolveKotlinScope
// used to inline every file id into the SQL text.
func TestKotlinScopeFileSelectionIsBatched(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	fixture := buildKotlinBudgetFixture(t, s, repo.ID, 1100)

	guard := newBudgetQuerier(s.db)
	seen := map[int64]struct{}{}
	if err := sqliteBatchedIDQuery(ctx, guard, fixture.fileIDs,
		`SELECT file_id FROM file_scope_evidence WHERE repo_id=? AND language='kotlin' AND file_id IN (`,
		[]any{repo.ID}, func(scan func(...any) error) error {
			var id int64
			if err := scan(&id); err != nil {
				return err
			}
			seen[id] = struct{}{}
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(fixture.fileIDs) {
		t.Fatalf("selected %d evidence-bearing files, want %d", len(seen), len(fixture.fileIDs))
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	batch := sqliteBatchSize(1, 1)
	if want := (len(fixture.fileIDs) + batch - 1) / batch; len(guard.statements) != want {
		t.Fatalf("ran %d statements, want %d", len(guard.statements), want)
	}
}
