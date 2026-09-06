package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// pyBudgetFixture is a Python import component wide enough that a scoped pass
// naming every edge would exceed the portable variable budget in one statement.
type pyBudgetFixture struct {
	edgeIDs []int64
	edges   map[int64]struct{}
}

// buildPythonBudgetFixture writes callerCount Python files, each importing the
// same helper by an absolute import, a relative import, an alias, and the
// package's __init__ re-export. Every caller therefore contributes several
// scoped edges, so a caller count in the hundreds crosses the batch boundary.
func buildPythonBudgetFixture(t *testing.T, s *Store, repoID int64, callerCount int) pyBudgetFixture {
	t.Helper()
	ctx := context.Background()
	target, err := insertTestFileLang(ctx, s, repoID, "pkg/helpers.py", "python")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestFileLang(ctx, s, repoID, "pkg/__init__.py", "python"); err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestSymbolLang(ctx, s, repoID, target, "load", "helpers.load", "python"); err != nil {
		t.Fatal(err)
	}
	out := pyBudgetFixture{edges: map[int64]struct{}{}}
	for i := range callerCount {
		file, err := insertTestFileLang(ctx, s, repoID, fmt.Sprintf("pkg/caller_%04d.py", i), "python")
		if err != nil {
			t.Fatal(err)
		}
		src, err := insertTestSymbolLang(ctx, s, repoID, file, "run", fmt.Sprintf("caller_%04d.run", i), "python")
		if err != nil {
			t.Fatal(err)
		}
		imports := []struct{ source, imported, local string }{
			{"pkg.helpers", "load", "load"},         // absolute
			{".helpers", "load", "load_relative"},   // relative
			{"pkg.helpers", "load", "load_aliased"}, // alias
			{"pkg", "helpers", "helpers"},           // package submodule arm
		}
		for _, imp := range imports {
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard) VALUES(?,?,?,?,?,?,?,0)`,
				repoID, file, "python", imp.source, imp.imported, imp.local, "named"); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"load", "load_relative", "load_aliased", "helpers.load"} {
			id, err := insertTestEdge(ctx, s, repoID, file, src, name)
			if err != nil {
				t.Fatal(err)
			}
			out.edgeIDs = append(out.edgeIDs, id)
			out.edges[id] = struct{}{}
		}
	}
	return out
}

// TestPythonScopeScopedPassStaysInVariableBudget is the load-bearing Python
// case: a scoped pass naming more edges than one statement may bind must still
// consider every one of them without ever exceeding the portable budget.
func TestPythonScopeScopedPassStaysInVariableBudget(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	fixture := buildPythonBudgetFixture(t, s, repo.ID, 500) // 2000 scoped edges
	if len(fixture.edgeIDs) <= sqliteDefaultMaxVariables {
		t.Fatalf("fixture has %d edges, want more than %d", len(fixture.edgeIDs), sqliteDefaultMaxVariables)
	}

	guard := newBudgetQuerier(s.db)
	bound, _, err := resolvePythonScope(ctx, guard, repo.ID, fixture.edges, false)
	if err != nil {
		t.Fatal(err)
	}
	if bound == 0 {
		t.Fatal("scoped pass bound no edges")
	}
	if guard.maxArgs > sqliteDefaultMaxVariables {
		t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
	}
	for _, st := range guard.matching(" IN (?") {
		if strings.Contains(collapseSQL(st.sql), "IN ()") {
			t.Fatalf("emitted an empty IN list: %s", collapseSQL(st.sql))
		}
	}
	edgeSelect := guard.matching("FROM edges e JOIN files f", "e.dst_symbol_id IS NULL")
	if len(edgeSelect) == 0 {
		t.Fatal("no scoped edge selection ran")
	}
	// The last edge id must be bound by some batch: batching may not silently
	// drop the tail of the caller's set.
	last := fixture.edgeIDs[len(fixture.edgeIDs)-1]
	if statementBinding(edgeSelect, last) < 0 {
		t.Fatalf("edge %d was never named by a scoped edge selection", last)
	}
}

// TestPythonScopeScopedBoundaryCounts walks the batch boundary itself.
func TestPythonScopeScopedBoundaryCounts(t *testing.T) {
	batch := sqliteBatchSize(1, 1)
	for _, n := range []int{batch - 1, batch, batch + 1, 1801} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			ctx := context.Background()
			s, repo := openBudgetStore(t)
			fixture := buildPythonBudgetFixture(t, s, repo.ID, (n+3)/4)
			only := map[int64]struct{}{}
			for _, id := range fixture.edgeIDs[:n] {
				only[id] = struct{}{}
			}
			guard := newBudgetQuerier(s.db)
			edges, err := pythonScopeEdges(ctx, guard, repo.ID, only)
			if err != nil {
				t.Fatal(err)
			}
			if len(edges) != n {
				t.Fatalf("read %d edges, want %d", len(edges), n)
			}
			for _, e := range edges {
				if _, ok := only[e.id]; !ok {
					t.Fatalf("edge %d was admitted but not requested", e.id)
				}
			}
			if guard.maxArgs > sqliteDefaultMaxVariables {
				t.Fatalf("max bound args = %d, want <= %d", guard.maxArgs, sqliteDefaultMaxVariables)
			}
			if want := (n + batch - 1) / batch; len(guard.statements) != want {
				t.Fatalf("ran %d statements, want %d", len(guard.statements), want)
			}
		})
	}
}

// TestPythonScopeUnfilteredPassIsOneStatement pins that the nil/unfiltered mode
// still runs exactly one edge query rather than a batched sweep.
func TestPythonScopeUnfilteredPassIsOneStatement(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	buildPythonBudgetFixture(t, s, repo.ID, 300)
	guard := newBudgetQuerier(s.db)
	edges, err := pythonScopeEdges(ctx, guard, repo.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1200 {
		t.Fatalf("unfiltered pass read %d edges, want 1200", len(edges))
	}
	if len(guard.statements) != 1 {
		t.Fatalf("unfiltered pass ran %d statements, want 1", len(guard.statements))
	}
}

// TestPythonScopeEmptyFilteredSetSelectsNothing pins the empty-set contract: an
// empty `only` is the unfiltered mode, matching the pre-batching API semantics.
func TestPythonScopeEmptyFilteredSetSelectsNothing(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	buildPythonBudgetFixture(t, s, repo.ID, 2)
	guard := newBudgetQuerier(s.db)
	edges, err := pythonScopeEdges(ctx, guard, repo.ID, map[int64]struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 8 {
		t.Fatalf("empty filter read %d edges, want the unfiltered 8", len(edges))
	}
	if len(guard.statements) != 1 {
		t.Fatalf("empty filter ran %d statements, want 1", len(guard.statements))
	}
}

// TestPythonScopeLargeScopedPassMatchesFullPass is the semantic parity check:
// candidates split across SQL batches must not change any resolver decision.
func TestPythonScopeLargeScopedPassMatchesFullPass(t *testing.T) {
	ctx := context.Background()
	resolutions := func(scoped bool) map[int64]string {
		s, repo := openBudgetStore(t)
		fixture := buildPythonBudgetFixture(t, s, repo.ID, 500)
		only := map[int64]struct{}(nil)
		if scoped {
			only = fixture.edges
		}
		if _, _, err := resolvePythonScope(ctx, s.db, repo.ID, only, false); err != nil {
			t.Fatal(err)
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT e.dst_name,COALESCE(e.resolution_strategy,''),COUNT(*) FROM edges e JOIN files f ON f.id=e.file_id
			 WHERE e.repo_id=? AND f.language='python' AND e.dst_symbol_id IS NOT NULL
			 GROUP BY e.dst_name,e.resolution_strategy`, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int64]string{}
		var i int64
		for rows.Next() {
			var name, strategy string
			var count int
			if err := rows.Scan(&name, &strategy, &count); err != nil {
				t.Fatal(err)
			}
			out[i] = fmt.Sprintf("%s|%s|%d", name, strategy, count)
			i++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	full, scoped := resolutions(false), resolutions(true)
	if len(full) != len(scoped) {
		t.Fatalf("full pass produced %d resolution groups, scoped pass %d", len(full), len(scoped))
	}
	for k, v := range full {
		if scoped[k] != v {
			t.Fatalf("group %d: full=%q scoped=%q", k, v, scoped[k])
		}
	}
	if len(full) == 0 {
		t.Fatal("neither pass resolved anything; the fixture proves nothing")
	}
}
