package store

import (
	"context"
	"testing"
)

// The C++ ownership predicate has two spellings -- one SQL, one Go -- and the
// repo-wide and incremental paths disagree about which edges a generic
// strategy may answer if they drift.
func TestCppScopeVetoSQLMatchesGoTwin(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	files := map[string]int64{}
	for _, language := range []string{"cpp", "go", "php"} {
		id, err := insertTestFileLang(ctx, s, repo.ID, "src."+language, language)
		if err != nil {
			t.Fatal(err)
		}
		files[language] = id
	}
	caller, err := insertTestSymbolLang(ctx, s, repo.ID, files["cpp"], "caller", "caller", "cpp")
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id                      int64
		language, dst, evidence string
	}
	var rows []row
	names := []string{"foo", "ns::foo", "::foo", "obj.foo", "obj.ns::foo", "foo<int>", "obj.foo<int>", "operator+", "a.b.c"}
	for _, language := range []string{"cpp", "go", "php"} {
		for _, dst := range names {
			for _, evidence := range []string{"", "direct:" + dst, "macro_unexpanded:CALL"} {
				id, err := insertTestEdge(ctx, s, repo.ID, files[language], caller, dst)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.ExecContext(ctx, `UPDATE edges SET evidence = ? WHERE id = ?`, evidence, id); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, row{id: id, language: language, dst: dst, evidence: evidence})
			}
		}
	}

	// The SQL twin: the veto is a NOT, so its negation is the owned set.
	ownedBySQL := map[int64]struct{}{}
	sqlRows, err := s.db.QueryContext(ctx, `
		SELECT edges.id FROM edges JOIN files f ON f.id = edges.file_id
		WHERE edges.repo_id = ? AND NOT (`+cppScopeVetoSQL+`)`, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlRows.Close()
	for sqlRows.Next() {
		var id int64
		if err := sqlRows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ownedBySQL[id] = struct{}{}
	}
	if err := sqlRows.Err(); err != nil {
		t.Fatal(err)
	}
	owned := 0
	for _, r := range rows {
		_, sqlOwned := ownedBySQL[r.id]
		goOwned := cppScopeOwned(edgeTarget{srcLanguage: r.language, dstName: r.dst, evidence: r.evidence})
		if sqlOwned != goOwned {
			t.Errorf("%s %q [%s]: cppScopeVetoSQL owned=%v, cppScopeOwned=%v", r.language, r.dst, r.evidence, sqlOwned, goOwned)
		}
		if goOwned {
			owned++
			if r.language != "cpp" {
				t.Errorf("%s %q: owned outside C++", r.language, r.dst)
			}
		}
	}
	if owned == 0 || owned == len(rows) {
		t.Fatalf("owned %d of %d rows; the fixture does not separate the two sides", owned, len(rows))
	}
}

// A mixed incremental batch decides every edge exactly once, whatever position
// the C++ edges take in it: owned C++ edges stay unresolved and are counted
// once, a C++ member spelling the gate does not own still reaches the generic
// lookups, and the other languages are untouched.
func TestCppOwnedEdgesInMixedIncrementalBatch(t *testing.T) {
	ctx := context.Background()
	names := []string{"pkg.Target", "ns::foo", "obj.method", "foo"}
	for run := 0; run < 8; run++ {
		s, repo := openBudgetStore(t)
		goFile, _ := insertTestFileLang(ctx, s, repo.ID, "pkg/pkg.go", "go")
		goCaller, _ := insertTestSymbolLang(ctx, s, repo.ID, goFile, "Caller", "pkg.Caller", "go")
		if _, err := insertTestSymbolLang(ctx, s, repo.ID, goFile, "Target", "pkg.Target", "go"); err != nil {
			t.Fatal(err)
		}
		lib, _ := insertTestFileLang(ctx, s, repo.ID, "lib.cpp", "cpp")
		insertTestSymbolKind(ctx, s, repo.ID, lib, "foo", "ns::foo", "function", "", "cpp")
		insertTestSymbolLang(ctx, s, repo.ID, lib, "foo", "foo", "cpp")
		insertTestSymbolLang(ctx, s, repo.ID, lib, "method", "obj.method", "cpp")
		app, _ := insertTestFileLang(ctx, s, repo.ID, "app.cpp", "cpp")
		cppCaller, _ := insertTestSymbolLang(ctx, s, repo.ID, app, "caller", "caller", "cpp")

		edges := map[string]int64{}
		add := func(key string, file, src int64, dst, evidence string) {
			id, err := insertTestEdge(ctx, s, repo.ID, file, src, dst)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE edges SET evidence = ? WHERE id = ?`, evidence, id); err != nil {
				t.Fatal(err)
			}
			edges[key] = id
		}
		// Vary which kind of edge comes first in id order as well as letting
		// the name batch iterate in map order.
		order := []string{"go", "owned", "member", "macro"}
		for i := 0; i < run%len(order); i++ {
			order = append(order[1:], order[0])
		}
		for _, key := range order {
			switch key {
			case "go":
				add(key, goFile, goCaller, "pkg.Target", "")
			case "owned":
				add(key, app, cppCaller, "ns::foo", "static_qualified:ns::foo")
			case "member":
				add(key, app, cppCaller, "obj.method", "")
			case "macro":
				add(key, app, cppCaller, "foo", "macro_unexpanded:CALL")
			}
		}

		stats, err := s.ResolveEdgesForPathsAndNames(ctx, repo.ID, nil, names)
		if err != nil {
			t.Fatal(err)
		}
		bound := func(key string) string {
			var dst string
			if err := s.db.QueryRowContext(ctx, `
				SELECT COALESCE((SELECT qualified_name FROM symbols WHERE id = e.dst_symbol_id), '')
				FROM edges e WHERE e.id = ?`, edges[key]).Scan(&dst); err != nil {
				t.Fatal(err)
			}
			return dst
		}
		want := map[string]string{"go": "pkg.Target", "owned": "", "member": "obj.method", "macro": ""}
		for key, w := range want {
			if got := bound(key); got != w {
				t.Errorf("run %d order %v: %s edge bound %q, want %q", run, order, key, got, w)
			}
		}
		if stats.TargetsSelected != 4 || stats.TargetsResolved != 2 || stats.TargetsUnresolved != 2 {
			t.Errorf("run %d order %v: selected/resolved/unresolved = %d/%d/%d, want 4/2/2",
				run, order, stats.TargetsSelected, stats.TargetsResolved, stats.TargetsUnresolved)
		}
	}
}
